package web

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// The Deviating devices tab (/policy/deviations): every deviation of every device, grouped by
// device, with a box per row for Make compliant, and the devices that cannot be judged yet
// with the way to change that (a backup, or a live read). Nothing is written from here: the
// boxes post to the Make compliant preview.

const policyDevPath = policyPath + "/deviations"

func (h *handler) registerPolicyDev(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	if h.d.Policy == nil {
		return
	}
	route("GET "+policyDevPath, op, func(w http.ResponseWriter, r *http.Request) {
		h.devRender(w, r, http.StatusOK, false)
	})
}

// devRow is one deviation of one device.
type devRow struct {
	Pair, Rule  string
	Required    bool
	Expected    string
	Actual      string
	Source      string // "live · 6 min", "backup · 28 Oct"
	Pickable    bool   // a box: the value was read live
	Checked     bool   // Required rows only
	NotLiveNote string // why a backup-only row has no box
}

// devGroup is a device with its deviating rules.
type devGroup struct {
	ID                    model.DeviceID
	Name, Family, Address string
	Count                 string // "1 required · 2 advisory"
	Rows                  []devRow
}

// devChip is a device that cannot be judged.
type devChip struct {
	ID   model.DeviceID
	Name string
}

type devView struct {
	Head         policyHead
	Base, CSRF   string
	Admin        bool
	CanFix       bool // Make compliant is available (admin and the service exists)
	CanBackup    bool
	Failed       bool // the report could not be built: the live-check tile says why
	Show, Q      string
	NReq, NAdv   int
	NUnknown     int
	Groups       []devGroup
	Checked      int
	Pickable     int
	Backup, Live []devChip // unjudged: a backup would tell / only a live read tells
}

// devRender writes the tab. live: run the live check first (the Run now button of this tab).
func (h *handler) devRender(w http.ResponseWriter, r *http.Request, status int, live bool) {
	ctx := r.Context()
	v := h.view(r, "Policy", "policy", nil)
	rep, repErr := h.d.Policy.Report(ctx, actor(r), live)

	st, err := h.d.PolicyRules.State(ctx)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	valid, invalid := st.Specs, 0
	if len(st.Specs) > 0 {
		var bad []*sysconfig.RuleError
		_, valid, bad = h.d.PolicyRules.ValidRules(st.Specs)
		invalid = len(bad)
	}

	p := devView{Base: v.Base, CSRF: v.CSRF, Admin: v.IsAdmin, Failed: repErr != nil,
		CanFix: v.IsAdmin && h.d.PolicyFix != nil, CanBackup: v.IsAdmin && h.d.BackupBatch != nil}
	p.Show = r.URL.Query().Get("show")
	if !slices.Contains([]string{"required", "advisory", "unknown"}, p.Show) {
		p.Show = ""
	}
	p.Q = strings.TrimSpace(r.URL.Query().Get("q"))

	p.Head = h.policyHead(v, valid, invalid, rep, repErr)
	p.Head.HasVersions = len(st.History) > 0
	p.Head.Show = len(st.Specs) > 0 || p.Head.HasVersions
	p.Head.Back = "deviations"
	if repErr == nil {
		h.devFill(&p, rep)
	}
	p.Head.Tabs = h.policyTabs(v.Base, "deviations", len(st.Specs), p.Head.Fleet.Deviating)
	if live && repErr == nil && p.Head.Line != nil && p.Head.Line.State != "failed" {
		v.Flash = "Live check finished · " + deviateText(p.Head.Fleet.Deviating)
	}
	h.show(w, r, status, "policy_deviations", v, policyDevPage(p))
}

// devFill groups the report's deviations by device and sorts the unjudged devices into the two
// folds. Tile and filter numbers come from the whole report; ?show= and ?q= only narrow the table.
func (h *handler) devFill(p *devView, rep PolicyReport) {
	now := h.d.Clock.Now()
	byDev := make(map[model.DeviceID]model.Device, len(rep.Devices))
	for _, d := range rep.Devices {
		byDev[d.ID] = d
	}
	rows := map[model.DeviceID][]devRow{}
	required := map[model.DeviceID]int{}
	deviates := map[model.DeviceID]bool{}
	unknown := map[model.DeviceID][]policy.Rule{}
	for _, o := range rep.Outcomes {
		if o.RuleIndex < 0 || o.RuleIndex >= len(rep.Rules) {
			continue
		}
		rule := rep.Rules[o.RuleIndex]
		isReq := rule.Severity != policy.SeverityInfo
		switch o.Kind {
		case policy.OutcomeUnknown:
			unknown[o.DeviceID] = append(unknown[o.DeviceID], rule)
		case policy.OutcomeDeviate:
			deviates[o.DeviceID] = true
			src, haveSrc := rep.Provenance[o.DeviceID][strings.ToLower(rule.Command)]
			row := devRow{
				Pair: fixPair{o.DeviceID, rule.ID}.String(), Rule: ruleTitle(rule), Required: isReq,
				Expected: rule.Want, Actual: o.Have, Source: valueSource(src, haveSrc, now),
				Pickable: haveSrc && src.Kind == "live" && rule.ID != "",
			}
			if haveSrc && src.Kind != "live" {
				row.NotLiveNote = "only known from a backup, not read live"
			}
			row.Checked = row.Pickable && isReq
			rows[o.DeviceID] = append(rows[o.DeviceID], row)
			if isReq {
				required[o.DeviceID]++
				p.NReq++
			} else {
				p.NAdv++
			}
		case policy.OutcomeMatch, policy.OutcomeShadowed, policy.OutcomeExcepted:
			// not a deviation and not unknown: nothing to list
		}
	}

	q := strings.ToLower(p.Q)
	for _, d := range rep.Devices {
		rs := rows[d.ID]
		if len(rs) == 0 {
			continue
		}
		g := devGroup{ID: d.ID, Name: d.Name, Family: chipLabel(d.Identity.Family), Address: d.Address}
		if nr, na := required[d.ID], len(rs)-required[d.ID]; nr > 0 && na > 0 {
			g.Count = strconv.Itoa(nr) + " required · " + strconv.Itoa(na) + " advisory"
		} else if nr > 0 {
			g.Count = strconv.Itoa(nr) + " required"
		} else {
			g.Count = strconv.Itoa(na) + " advisory"
		}
		for _, row := range rs {
			switch {
			case p.Show == "required" && !row.Required, p.Show == "advisory" && row.Required, p.Show == "unknown":
				continue
			case q != "" && !strings.Contains(strings.ToLower(d.Name), q) && !strings.Contains(strings.ToLower(row.Rule), q):
				continue
			}
			g.Rows = append(g.Rows, row)
			if row.Pickable {
				p.Pickable++
			}
			if row.Checked {
				p.Checked++
			}
		}
		if len(g.Rows) > 0 {
			p.Groups = append(p.Groups, g)
		}
	}
	sort.SliceStable(p.Groups, func(i, j int) bool { return strings.ToLower(p.Groups[i].Name) < strings.ToLower(p.Groups[j].Name) })

	// Not judged: a device with an unknown value and no deviation (the same definition as the tile).
	for _, d := range rep.Devices {
		rs := unknown[d.ID]
		if len(rs) == 0 || deviates[d.ID] {
			continue
		}
		p.NUnknown++
		if q != "" && !strings.Contains(strings.ToLower(d.Name), q) {
			continue
		}
		chip := devChip{ID: d.ID, Name: d.Name}
		// A backup tells the settings it decodes; for the rest only a live read does.
		if slices.ContainsFunc(rs, func(r policy.Rule) bool { return policy.BackupReadable(r.Command) }) {
			p.Backup = append(p.Backup, chip)
		} else {
			p.Live = append(p.Live, chip)
		}
	}
	for _, l := range [][]devChip{p.Backup, p.Live} {
		sort.SliceStable(l, func(i, j int) bool { return strings.ToLower(l[i].Name) < strings.ToLower(l[j].Name) })
	}
}
