package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// PolicyFix starts a "Make compliant" transaction (ota.Engine.SubmitPolicy,
// behind the device locks). Devices it cannot take are returned with the
// reason.
type PolicyFix interface {
	Submit(ctx context.Context, actor string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error)
}

func (h *handler) registerPolicyFix(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Policy == nil || h.d.PolicyFix == nil {
		return
	}
	route("POST /policy/fix", adm, h.policyFixPreview)
	route("POST /policy/fix/apply", adm, h.policyFixApply)
}

type fixCmd struct {
	Label, Text, Have, Want, Note string
	// Advisory: the command comes from an Advisory rule that was picked by hand.
	Advisory bool
}

type fixSkipView struct{ Label, Reason string }

// fixOption is an Advisory deviation the admin may add to the plan by ticking it (never ticked
// unless picked before).
type fixOption struct {
	Pair       string // the value of the "pick" field
	Name       string
	Have, Want string
	Picked     bool
}

type fixRow struct {
	Device  model.Device
	Cmds    []fixCmd
	Skipped []fixSkipView
	// Blocked is why the device is not touched at all ("" = it is).
	Blocked string
	// Options: the Advisory deviations of the device that can be added to the plan.
	Options []fixOption
	// Pairs are the picked deviations of this row the apply form has to carry (every included one
	// in a by-pick plan, the picked Advisory ones otherwise).
	Pairs []fixPair
}

type fixPage struct {
	Rows     []fixRow
	Ready    int // devices that get commands
	Commands int
	IDs      []model.DeviceID
	Digest   string
	Stale    bool // the values changed since the preview the admin saw
	ByPick   bool // opened from the Deviating devices tab (Cancel goes back there)
	// Fields are what the apply form posts back (besides csrf and digest): the devices, or the
	// picks of a by-pick plan, and the picked Advisory deviations.
	Fields []ui.KVRow
	// Select is every device of the selection and its picks, for the form that re-draws the plan
	// with other Advisory ticks (device plans only).
	Select []ui.KVRow
}

type fixStarted struct {
	Batch   model.BatchID
	Skipped []fixSkipView // device name and reason
}

// activeJobStates are the states in which a job holds its device.
var activeJobStates = []model.JobState{
	model.JobQueued, model.JobPreflight, model.JobBackup, model.JobSetOtaURL, model.JobSafebootSwitch, model.JobSafebootRecover, model.JobUpgradeIssued,
	model.JobAwaitReboot, model.JobVerify, model.JobRestoreOtaURL, model.JobRestorePower, model.JobApplying,
}

// fixPair names one deviation of one device by the rule's stable id; it is the unit of "pick".
type fixPair struct {
	Dev  model.DeviceID
	Rule string
}

func (p fixPair) String() string { return strconv.FormatInt(int64(p.Dev), 10) + ":" + p.Rule }

// fixSel is which deviations the admin asked to fix. Devices come from "only", "all" or "device"
// (every Required deviation of those devices) or from "pick" alone (exactly the picked
// deviations). In the device modes a pick adds one Advisory deviation of a selected device: an
// Advisory deviation is never in the plan unless it was picked by hand.
type fixSel struct {
	IDs    []model.DeviceID
	Picks  map[fixPair]bool // only pairs that are findings of the report, of selected devices
	ByPick bool             // the plan holds the picked deviations and nothing else
}

func (s fixSel) has(id model.DeviceID) bool { return slices.Contains(s.IDs, id) }

// include says whether a finding is part of the plan.
func (s fixSel) include(f policy.Finding) bool {
	if !s.has(f.DeviceID) {
		return false
	}
	picked := s.Picks[fixPair{f.DeviceID, f.Rule.ID}]
	if s.ByPick || advisory(f) {
		return picked
	}
	return true
}

// fixSelection reads which deviations the admin picked. A pick for a pair that is not a finding
// of the report is dropped, so a forged form never widens the plan.
func fixSelection(r *http.Request, rep PolicyReport) fixSel {
	parse := func(vals []string) []model.DeviceID {
		var out []model.DeviceID
		seen := map[model.DeviceID]bool{}
		for _, v := range vals {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !seen[model.DeviceID(n)] {
				seen[model.DeviceID(n)] = true
				out = append(out, model.DeviceID(n))
			}
		}
		return out
	}
	known := map[fixPair]bool{}
	for _, f := range rep.Findings {
		if f.Rule.ID != "" {
			known[fixPair{f.DeviceID, f.Rule.ID}] = true
		}
	}
	var picks []fixPair
	for _, v := range r.PostForm["pick"] {
		dev, rule, ok := strings.Cut(v, ":")
		n, err := strconv.ParseInt(dev, 10, 64)
		if p := (fixPair{model.DeviceID(n), rule}); ok && err == nil && known[p] {
			picks = append(picks, p)
		}
	}
	var sel fixSel
	switch {
	case r.PostForm.Get("only") != "":
		sel.IDs = parse(r.PostForm["only"][:1])
	case r.PostForm.Get("all") != "":
		seen := map[model.DeviceID]bool{}
		for _, f := range rep.Findings {
			if !f.Unknown && !advisory(f) && !seen[f.DeviceID] {
				seen[f.DeviceID] = true
				sel.IDs = append(sel.IDs, f.DeviceID)
			}
		}
	case len(r.PostForm["device"]) > 0:
		sel.IDs = parse(r.PostForm["device"])
	default:
		sel.ByPick = true
		for _, p := range picks {
			if !sel.has(p.Dev) {
				sel.IDs = append(sel.IDs, p.Dev)
			}
		}
	}
	sel.Picks = map[fixPair]bool{}
	for _, p := range picks {
		if sel.has(p.Dev) {
			sel.Picks[p] = true
		}
	}
	return sel
}

// fixRows turns the report's findings of the selection into the plan the admin reviews. Every
// Required deviation of a selected device is included; an Advisory one (a hint) only when it was
// picked by hand (fixSel.include). Only values read live count (see policy.PlanFix); a device
// with an active job is listed as not touched.
func (h *handler) fixRows(ctx context.Context, rep PolicyReport, sel fixSel) []fixRow {
	live := func(id model.DeviceID, key string) bool { return rep.Provenance[id][key].Kind == "live" }
	var findings []policy.Finding
	included := map[model.DeviceID][]fixPair{}
	hint := map[model.DeviceID]map[string]bool{} // device, lower-case command: every included rule is Advisory
	options := map[model.DeviceID][]fixOption{}
	for _, f := range rep.Findings {
		if !sel.has(f.DeviceID) {
			continue
		}
		pair := fixPair{f.DeviceID, f.Rule.ID}
		if sel.include(f) {
			findings = append(findings, f)
			key := strings.ToLower(f.Rule.Command)
			if hint[f.DeviceID] == nil {
				hint[f.DeviceID] = map[string]bool{}
			}
			if was, ok := hint[f.DeviceID][key]; !ok || was {
				hint[f.DeviceID][key] = advisory(f)
			}
			if sel.ByPick || advisory(f) {
				included[f.DeviceID] = append(included[f.DeviceID], pair)
			}
		}
		if !sel.ByPick && advisory(f) && !f.Unknown && f.Rule.ID != "" && live(f.DeviceID, strings.ToLower(f.Rule.Command)) {
			options[f.DeviceID] = append(options[f.DeviceID], fixOption{
				Pair: pair.String(), Name: ruleTitle(f.Rule), Have: f.Have, Want: f.Rule.Want, Picked: sel.Picks[pair],
			})
		}
	}
	plans := map[model.DeviceID]policy.FixPlan{}
	for _, p := range policy.PlanFix(findings, live) {
		plans[p.DeviceID] = p
	}
	now := h.d.Clock.Now()
	var rows []fixRow
	for _, d := range rep.Devices {
		if !sel.has(d.ID) {
			continue
		}
		row := fixRow{Device: d, Options: options[d.ID], Pairs: included[d.ID]}
		p := plans[d.ID]
		onlyMQTT := h.d.MQTTOnly != nil && h.d.MQTTOnly(ctx, d.ID)
		for _, c := range p.Commands {
			if onlyMQTT && cmdpolicy.CutsMQTTOff(c.Text) {
				// Make compliant never confirms a change that can cut off a device reached over MQTT only
				// (the router would refuse it); the plan says so instead of promising it.
				p.Skipped = append(p.Skipped, policy.FixSkip{Command: c.Command, Reason: "reached over MQTT only (use the console)"})
				continue
			}
			cmd := fixCmd{Label: settings.Label(c.Command), Text: c.Text, Have: c.Have, Want: c.Want, Advisory: hint[d.ID][strings.ToLower(c.Command)]}
			if src, ok := rep.Provenance[d.ID][strings.ToLower(c.Command)]; ok {
				cmd.Note = sourceNote(src, now)
			}
			row.Cmds = append(row.Cmds, cmd)
		}
		// Two rules for one setting (a different expected value each) skip with the same words:
		// say it once, with the number of rules.
		var skipped []fixSkipView
		counts := map[fixSkipView]int{}
		for _, s := range p.Skipped {
			v := fixSkipView{Label: settings.Label(s.Command), Reason: s.Reason}
			if counts[v]++; counts[v] == 1 {
				skipped = append(skipped, v)
			}
		}
		for _, v := range skipped {
			if n := counts[v]; n > 1 {
				v.Label = fmt.Sprintf("%s (%d rules)", v.Label, n)
			}
			row.Skipped = append(row.Skipped, v)
		}
		switch {
		case len(row.Cmds) == 0 && len(row.Skipped) == 0 && len(row.Options) > 0:
			row.Blocked = "No Required rule deviates. Tick an Advisory deviation below to include it."
		case len(row.Cmds) == 0 && len(row.Skipped) == 0:
			row.Blocked = "Already compliant, nothing to change."
		case len(row.Cmds) == 0:
			row.Blocked = "Nothing can be changed: every deviation is unknown or not read live."
		case h.d.Jobs != nil:
			if act, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{DeviceID: d.ID, States: activeJobStates, Limit: 1}); err == nil && len(act) > 0 {
				row.Blocked = "Another transaction is running on this device (#" + strconv.FormatInt(int64(act[0].BatchID), 10) + ")."
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.ToLower(rows[i].Device.Name) < strings.ToLower(rows[j].Device.Name)
	})
	return rows
}

// fixPageOf builds the page of a plan. The digest covers every command per device and the picked
// deviations, so a changed pick can never apply under the digest of another plan.
func fixPageOf(rows []fixRow, byPick bool) fixPage {
	p := fixPage{Rows: rows, ByPick: byPick}
	sum := sha256.New()
	for _, row := range rows {
		if !byPick {
			p.Select = append(p.Select, ui.KVRow{Key: "device", Value: strconv.FormatInt(int64(row.Device.ID), 10)})
		}
		if row.Blocked != "" {
			continue
		}
		p.Ready++
		p.IDs = append(p.IDs, row.Device.ID)
		if !byPick {
			p.Fields = append(p.Fields, ui.KVRow{Key: "device", Value: strconv.FormatInt(int64(row.Device.ID), 10)})
		}
		for _, c := range row.Cmds {
			p.Commands++
			sum.Write([]byte(strconv.FormatInt(int64(row.Device.ID), 10) + "\x00" + c.Text + "\n"))
		}
		for _, pair := range row.Pairs {
			p.Fields = append(p.Fields, ui.KVRow{Key: "pick", Value: pair.String()})
			sum.Write([]byte("pick\x00" + pair.String() + "\n"))
		}
	}
	p.Digest = hex.EncodeToString(sum.Sum(nil))
	return p
}

func (h *handler) fixForm(w http.ResponseWriter, r *http.Request) (PolicyReport, bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return PolicyReport{}, false
	}
	rep, err := h.d.Policy.Report(r.Context(), actor(r), false)
	if err != nil {
		h.fail(w, r, err)
		return rep, false
	}
	return rep, true
}

// policyFixPreview shows the exact commands per device. Nothing is sent.
func (h *handler) policyFixPreview(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.fixForm(w, r)
	if !ok {
		return
	}
	sel := fixSelection(r, rep)
	p := fixPageOf(h.fixRows(r.Context(), rep, sel), sel.ByPick)
	h.fixRender(w, r, http.StatusOK, p)
}

// policyFixApply starts the transaction. The plan is built again from the
// current values; if it differs from the one the admin reviewed, the preview is
// shown again instead of applying something nobody looked at.
func (h *handler) policyFixApply(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.fixForm(w, r)
	if !ok {
		return
	}
	sel := fixSelection(r, rep)
	rows := h.fixRows(r.Context(), rep, sel)
	p := fixPageOf(rows, sel.ByPick)
	if p.Digest != r.PostForm.Get("digest") {
		p.Stale = true
		h.fixRender(w, r, http.StatusConflict, p)
		return
	}
	var reqs []ota.PolicyRequest
	for _, row := range rows {
		if row.Blocked != "" {
			continue
		}
		req := ota.PolicyRequest{DeviceID: row.Device.ID}
		for _, c := range row.Cmds {
			req.Commands = append(req.Commands, c.Text)
		}
		reqs = append(reqs, req)
	}
	batch, skipped, err := h.d.PolicyFix.Submit(r.Context(), actor(r), reqs)
	if errors.Is(err, ota.ErrNothingToFix) {
		h.message(w, r, http.StatusConflict, h.view(r, "Nothing to change", "policy", "Nothing was sent. "+h.refusalText(r, skipped)))
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(skipped) == 0 {
		redirect(w, r, "/transactions/"+strconv.FormatInt(int64(batch), 10))
		return
	}
	s := fixStarted{Batch: batch}
	for _, x := range skipped {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		s.Skipped = append(s.Skipped, fixSkipView{Label: d.Name, Reason: humanReason(x.Reason)})
	}
	v := h.view(r, "Make compliant", "policy", nil)
	v.Crumbs = fixCrumbs
	h.show(w, r, http.StatusOK, "policy_fix_started", v, fixStartedPage(v.Base, s))
}

// fixCrumbs is the trail of the Make compliant pages.
var fixCrumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "Make compliant"}}

// fixRender shows the plan as a page: the no-JavaScript way of "Make compliant (N)…", and the
// answer when the values changed after the dialog was drawn.
func (h *handler) fixRender(w http.ResponseWriter, r *http.Request, status int, p fixPage) {
	v := h.view(r, "Make compliant", "policy", nil)
	v.Crumbs = fixCrumbs
	h.show(w, r, status, "policy_fix", v, fixPreviewPage(v.Base, v.CSRF, p))
}

// fixDialogOf is the plan behind "Make compliant (N)…" on the policy page: every device with
// a Required deviation read live. nil when there is nothing to change. Advisory deviations are
// not in it; "Choose Advisory rows…" opens the preview where they can be ticked.
func (h *handler) fixDialogOf(ctx context.Context, rep PolicyReport) *fixDialog {
	sel := fixSel{Picks: map[fixPair]bool{}}
	seen := map[model.DeviceID]bool{}
	for _, f := range rep.Findings {
		if !f.Unknown && !advisory(f) && !seen[f.DeviceID] {
			seen[f.DeviceID] = true
			sel.IDs = append(sel.IDs, f.DeviceID)
		}
	}
	rows := h.fixRows(ctx, rep, sel)
	p := fixPageOf(rows, false)
	if p.Ready == 0 {
		return nil
	}
	d := &fixDialog{Ready: p.Ready, Commands: p.Commands, Fields: append([]ui.KVRow{{Key: "digest", Value: p.Digest}}, p.Fields...)}
	for _, row := range rows {
		if len(row.Options) > 0 {
			d.Advisory = true
		}
		if row.Blocked != "" {
			continue
		}
		var cmds []string
		for _, c := range row.Cmds {
			cmds = append(cmds, c.Text)
		}
		d.Changes = append(d.Changes, ui.KVRow{Key: row.Device.Name, Value: strings.Join(cmds, "; ")})
	}
	return d
}

func (h *handler) refusalText(r *http.Request, skipped []ota.Refusal) string {
	var b strings.Builder
	for _, x := range skipped {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		b.WriteString(d.Name + ": " + humanReason(x.Reason) + ". ")
	}
	return b.String()
}

// fixNote says what a command changes: the setting, its value now and where that came from.
func fixNote(c fixCmd) string {
	s := c.Label + ": has " + c.Have
	if c.Note != "" {
		s += ", " + c.Note
	}
	return s
}
