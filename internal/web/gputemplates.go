package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmotacmd"
	"github.com/nofuturekid/tasmofleet/internal/templates"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The GPIO templates tab of the Policy page (/policy/templates, #327): devices grouped by template
// NAME and chip family, each group compared with its most common template or the one the person
// chose (?ref=<deviceID>, not stored), and the preview and apply pages behind "Apply reference to N…".
// Operators read; admins apply. All decisions are the service's (internal/templates, internal/policy);
// this file only turns them into pages.

const policyTemplatesPath = policyPath + "/templates"

func (h *handler) registerGPIOTemplates(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op, adm func(http.Handler) http.Handler) {
	if h.d.Policy == nil || h.d.Templates == nil {
		return
	}
	route("GET "+policyTemplatesPath, op, func(w http.ResponseWriter, r *http.Request) { h.gtRender(w, r, false) })
	route("POST "+policyTemplatesPath+"/read", op, func(w http.ResponseWriter, r *http.Request) {
		limitBody(w, r)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		h.gtRender(w, r, true)
	})
	route("POST "+policyTemplatesPath+"/preview", adm, h.gtPreview)
	route("POST "+policyTemplatesPath+"/apply", adm, h.gtApply)
}

// ---- the tab ----

type gtDiffLine struct{ Pin, Have, Want string }

type gtPinRow struct {
	Pin, Ref, Dev string
	Bad           bool
}

type gtPins struct {
	Summary, Fine, Device string
	Rows                  []gtPinRow
	Open                  bool
}

type gtDevice struct {
	ID         model.DeviceID
	Name, Href string
	Chip       string // the status chip ("" = none)
	Tone       ui.Tone
	Meta       string // "live", "live · tasmota-minimal", "from backup"
	At         time.Time
	Diff       []gtDiffLine
	WantWord   string // "reference" or "variant A"
	Why        string
	Pick       bool   // a tick box: the reference can be applied
	UseRef     string // href of "Use as reference" ("" = none)
	Pins       *gtPins
}

type gtNote struct {
	Text   string // the plain note
	Choose string // bold lead-in of "Choose a reference."
	Back   string // href of "Back to most common"
	Tail   string // text after the back link
}

type gtGroup struct {
	Title, Chip, Count string
	Note               gtNote
	Rows               []gtDevice
	Apply              *gtApply
	Rank               string
	Fine               string // a line under the rows
}

type gtApply struct {
	FormID string
	Fields []ui.KVRow // the hidden fields of the group's form
	N      int
}

type gtListItem struct {
	Name, Href, Why string
	Chip, Title     string // "Used by one device": template name and chip
}

type gtView struct {
	Head        policyHead
	Base, CSRF  string
	Admin       bool
	Summary     string
	ReadAt      time.Time
	Groups      []gtGroup
	NotJudged   []gtListItem
	CanBackup   bool
	BackupIDs   []model.DeviceID
	BuiltIn     []gtListItem
	OneDevice   []gtListItem
	Empty       bool
	Differ      int
	Stacks      [][]gtCardRef
	Failed      string
	Throttled   bool
	GroupsCount int
}

// gtCardRef places one card in a stack: the group index, or one of the three folds.
type gtCardRef struct {
	Group int // index into Groups, -1 for a fold
	Fold  string
	Rank  string // phone place: the cards in reading order
}

// gtRefs reads ?ref=<deviceID> (repeatable). The group of a device comes from the cached overview, so
// a forged id of a device that is no live member of any group is dropped by the service's planning.
func (h *handler) gtRefs(r *http.Request, base templates.Overview) (templates.Refs, []model.DeviceID) {
	refs := templates.Refs{}
	var ids []model.DeviceID
	for _, s := range r.URL.Query()["ref"] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		id := model.DeviceID(n)
		for _, g := range base.Groups {
			if g.Key.BuiltIn {
				continue
			}
			if slices.ContainsFunc(g.Devices, func(d templates.DeviceView) bool { return d.ID == id }) {
				refs[g.Key] = id
				ids = append(ids, id)
			}
		}
	}
	return refs, ids
}

func (h *handler) gtRender(w http.ResponseWriter, r *http.Request, refresh bool) {
	ctx := r.Context()
	v := h.view(r, "Policy", "policy", nil)
	rep, repErr := h.d.Policy.Report(ctx, actor(r), false)
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
	// The first pass uses the most common reference everywhere; it also tells which group a ?ref belongs to.
	base, err := h.d.Templates.Overview(ctx, nil, refresh)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ov := base
	refs, chosen := h.gtRefs(r, base)
	if len(refs) > 0 {
		if ov, err = h.d.Templates.Overview(ctx, refs, false); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	p := h.gtBuild(ov, base, chosen, v.IsAdmin, r.URL.Query())
	p.Base, p.CSRF, p.Admin = v.Base, v.CSRF, v.IsAdmin
	p.Head = h.policyHead(v, valid, invalid, rep, repErr)
	p.Head.HasVersions = len(st.History) > 0
	p.Head.Show = len(st.Specs) > 0 || p.Head.HasVersions
	p.Head.Back = "templates"
	p.Head.Tabs = h.policyTabs(v.Base, "templates", len(st.Specs), p.Head.Fleet.Deviating, p.Differ)
	p.CanBackup = v.IsAdmin && h.d.BackupBatch != nil
	if refresh {
		switch {
		case ov.Throttled:
			v.Flash = "Read again was asked a moment ago; the readings shown are the last ones."
		default:
			v.Flash = "Read again finished."
		}
	}
	h.show(w, r, http.StatusOK, "policy_templates", v, policyTemplatesPage(p))
}

func groupTitle(k policy.GroupKey) string { return k.Name }

// gtBuild turns the overview into the cards.
func (h *handler) gtBuild(ov, base templates.Overview, chosen []model.DeviceID, admin bool, q url.Values) gtView {
	p := gtView{ReadAt: ov.ReadAt, Throttled: ov.Throttled}
	var rank int
	groupsShown, notJudged := 0, len(ov.NotJudged)
	cannot, needRef := 0, 0
	baseNeeds := map[policy.GroupKey]bool{}
	for _, g := range base.Groups {
		baseNeeds[g.Key] = g.NeedsReference
	}
	// Groups that need a look come first: a deviating device, then a reference to choose, then the rest;
	// the service's order (family, name) is kept inside each tier.
	tier := func(g templates.GroupView) int {
		switch {
		case slices.ContainsFunc(g.Devices, func(d templates.DeviceView) bool { return d.Plan != nil && !d.Plan.Matches }):
			return 0
		case g.NeedsReference:
			return 1
		}
		return 2
	}
	ordered := slices.Clone(ov.Groups)
	sort.SliceStable(ordered, func(i, j int) bool { return tier(ordered[i]) < tier(ordered[j]) })
	for _, g := range ordered {
		if len(g.Devices) < 2 {
			it := gtListItem{Title: groupTitle(g.Key), Chip: chipLabel(g.Key.Family)}
			if len(g.Devices) == 1 {
				it.Name, it.Href = g.Devices[0].Name, h.devHrefOf(g.Devices[0].ID)
			}
			p.OneDevice = append(p.OneDevice, it)
			continue
		}
		groupsShown++
		rank++
		gg, differ, cant := h.gtGroupOf(g, baseNeeds[g.Key], admin, rank, q)
		p.Differ += differ
		cannot += cant
		if g.NeedsReference {
			needRef++
		}
		p.Groups = append(p.Groups, gg)
	}
	for _, g := range ov.BuiltIn {
		for _, d := range g.Devices {
			p.BuiltIn = append(p.BuiltIn, gtListItem{Name: d.Name, Href: h.devHrefOf(d.ID), Why: builtInText(g.Key.Name, d.Reading)})
		}
	}
	for _, d := range ov.NotJudged {
		p.NotJudged = append(p.NotJudged, gtListItem{Name: d.Name, Href: h.devHrefOf(d.ID), Why: d.NotJudgedReason})
		if d.Source == templates.SourceNone && strings.Contains(strings.ToLower(d.NotJudgedReason), "no backup") {
			p.BackupIDs = append(p.BackupIDs, d.ID)
		}
	}
	p.GroupsCount = groupsShown
	p.Summary = plural(groupsShown, "group") + " · " + plural(p.Differ, "device") + " differ"
	if p.Differ == 1 {
		p.Summary = plural(groupsShown, "group") + " · 1 device differs"
	}
	if needRef > 0 {
		p.Summary += " · " + plural(needRef, "group") + " need a reference"
		if needRef == 1 {
			p.Summary = strings.Replace(p.Summary, "1 group need a reference", "1 group needs a reference", 1)
		}
	}
	if cannot > 0 {
		p.Summary += " · " + strconv.Itoa(cannot) + " cannot be changed here"
	}
	p.Summary += " · " + strconv.Itoa(notJudged) + " not judged"
	p.Empty = len(p.Groups)+len(p.NotJudged)+len(p.BuiltIn)+len(p.OneDevice) == 0
	p.Stacks = gtStacks(p)
	return p
}

// devHrefOf is the device page; Base is added by the template through gtHref.
func (h *handler) devHrefOf(id model.DeviceID) string { return "/devices/" + devStr(id) }

func builtInText(module string, r policy.TemplateDevice) string {
	if r.Module != nil {
		return module + " (module " + strconv.Itoa(r.Module.Number) + ")"
	}
	return module
}

// gtStacks deals the cards into the two column stacks (balanced by an estimate of their height) in
// reading order; the folds come after the groups.
func gtStacks(p gtView) [][]gtCardRef {
	cards := make([]gtCardRef, 0, len(p.Groups)+3)
	for i := range p.Groups {
		cards = append(cards, gtCardRef{Group: i})
	}
	if len(p.NotJudged) > 0 {
		cards = append(cards, gtCardRef{Group: -1, Fold: "notjudged"})
	}
	if len(p.BuiltIn) > 0 {
		cards = append(cards, gtCardRef{Group: -1, Fold: "builtin"})
	}
	if len(p.OneDevice) > 0 {
		cards = append(cards, gtCardRef{Group: -1, Fold: "one"})
	}
	// size is the card's height in lines of text, close enough to put the next card under the shorter stack.
	size := func(c gtCardRef) int {
		switch {
		case c.Group >= 0:
			g := p.Groups[c.Group]
			n := 4
			if g.Fine != "" {
				n += 2
			}
			for _, d := range g.Rows {
				n += 2 + len(d.Diff)
				if d.Why != "" {
					n += 2
				}
				if d.Pins != nil {
					n += 2
					if d.Pins.Open {
						n += len(d.Pins.Rows) + 2
					}
				}
			}
			return n
		case c.Fold == "notjudged":
			return 6 + 2*len(p.NotJudged)
		case c.Fold == "builtin":
			return 6 + len(p.BuiltIn)
		}
		return 3
	}
	stacks := make([][]gtCardRef, 2)
	var h [2]int
	for _, c := range cards {
		i := 0
		if h[1] < h[0] {
			i = 1
		}
		stacks[i] = append(stacks[i], c)
		h[i] += size(c)
	}
	for i, c := range cards {
		for _, st := range stacks {
			for j := range st {
				if st[j] == (gtCardRef{Group: c.Group, Fold: c.Fold}) {
					st[j].Rank = ui.StackRank(i + 1)
				}
			}
		}
	}
	if len(stacks[1]) == 0 {
		return stacks[:1]
	}
	return stacks
}

func variantLetter(i int) string {
	const letters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	return letters[i%len(letters) : i%len(letters)+1]
}

// gtGroupOf builds one group card. differ counts the devices that differ and can be changed, cannot
// those that differ but are refused.
func (h *handler) gtGroupOf(g templates.GroupView, baseNeedsRef, admin bool, rank int, q url.Values) (gg gtGroup, differ, cannot int) {
	gg = gtGroup{Title: groupTitle(g.Key), Chip: chipLabel(g.Key.Family), Count: plural(len(g.Devices), "device"), Rank: ui.StackRank(rank)}
	gg.Count = strconv.Itoa(len(g.Devices)) + " devices"
	fam := string(g.Key.Family)

	var refID model.DeviceID
	var refT *policy.GPIOTemplate
	if g.Reference != nil {
		refID, refT = g.Reference.Device.ID, g.Reference.Device.Template
	}
	// Variants for a group without a reference: layouts of the live devices, most used first.
	variant := map[string]int{}
	var firstLive *policy.GPIOTemplate
	if g.Reference == nil {
		counts := map[string]int{}
		first := map[string]model.DeviceID{}
		var order []string
		for _, d := range g.Devices {
			if d.Reading.Live && d.Reading.State() == policy.OnTemplate {
				fp := d.Reading.Template.Fingerprint()
				if counts[fp] == 0 {
					order = append(order, fp)
					first[fp] = d.ID
				}
				counts[fp]++
			}
		}
		sort.SliceStable(order, func(i, j int) bool { return counts[order[i]] > counts[order[j]] })
		for i, fp := range order {
			variant[fp] = i
		}
		for _, d := range g.Devices {
			if len(order) > 0 && d.ID == first[order[0]] {
				firstLive = d.Reading.Template
			}
		}
		gg.Note = noteChoose(order, counts)
		gg.Fine = "Differences are shown against variant A until you choose. Nothing can be applied without a reference."
	} else {
		gg.Note = gtNoteOf(g, baseNeedsRef, q, h)
	}

	variantShown := map[int]bool{} // the diff of a variant is listed once, under its first device
	canApply := admin && g.Reference != nil
	for _, d := range g.Devices {
		row := gtDevice{ID: d.ID, Name: d.Name, Href: h.devHrefOf(d.ID), Meta: "live", WantWord: "reference", At: time.Time{}}
		if d.Source == templates.SourceBackup {
			row.Meta, row.At = "from backup", d.ReadAt
		}
		if d.Reading.Variant == "minimal" {
			row.Meta += " · tasmota-minimal"
		}
		live := d.Reading.Live && d.Reading.State() == policy.OnTemplate
		if live && d.ID != refID {
			row.UseRef = gtRefHref(q, g, d.ID)
		}
		switch {
		case g.Reference != nil && d.ID == refID && g.Reference.How == policy.RefChosen:
			row.Chip, row.Tone = "Reference", ui.ToneAccent
		case g.Reference != nil && d.ID == refID:
			row.Chip, row.Tone = "Matches", ui.ToneOK // the most common layout: the reference is one of those that match
		case d.Plan == nil && g.Reference != nil:
			row.Chip, row.Tone = "Not judged", ui.ToneNeutral
			row.Why = d.NotJudgedReason
			if row.Why == "" {
				row.Why = "Its template was read from a backup; backup values are shown, never applied."
			}
		case d.Plan != nil && d.Plan.Refusal != policy.RefuseNone:
			row.Chip, row.Tone = "Cannot apply", ui.ToneDanger
			row.Why = d.Plan.Reason
			row.Diff = gtDiffLines(d.Plan.Diff)
			cannot++
		case d.Plan != nil && d.Plan.Matches:
			row.Chip, row.Tone = "Matches", ui.ToneOK
		case d.Plan != nil:
			row.Chip, row.Tone = "Differs", ui.ToneWarn
			row.Diff = gtDiffLines(d.Plan.Diff)
			differ++
			row.Pick = canApply
			if d.Reading.Template != nil && refT != nil {
				row.Pins = gtPinTable(fam, d.Name, *refT, *d.Reading.Template, false)
			}
		case g.Reference == nil && live:
			i := variant[d.Reading.Template.Fingerprint()]
			row.Chip, row.Tone = "Variant "+variantLetter(i), ui.ToneNeutral
			row.WantWord = "variant A"
			if i > 0 && firstLive != nil && !variantShown[i] {
				variantShown[i] = true
				if diff, err := policy.DiffTemplates(g.Key.Family, *d.Reading.Template, *firstLive); err == nil {
					row.Diff = gtDiffLines(diff)
				}
			}
		default:
			row.Chip, row.Tone = "Not judged", ui.ToneNeutral
			row.Why = d.NotJudgedReason
		}
		gg.Rows = append(gg.Rows, row)
	}
	if differ == 1 { // one device differs: its pin table is open, with more it stays a line to open
		for i := range gg.Rows {
			if gg.Rows[i].Pins != nil {
				gg.Rows[i].Pins.Open = true
			}
		}
	}
	if canApply && differ > 0 {
		gg.Apply = &gtApply{FormID: "gt-f-" + strconv.Itoa(rank), N: differ, Fields: gtGroupFields(g.Key, refID, g.Reference.How)}
		// The devices without a tick box go along, so the preview can say why each one is not included.
		for _, row := range gg.Rows {
			if !row.Pick {
				gg.Apply.Fields = append(gg.Apply.Fields, ui.KVRow{Key: "pick", Value: devStr(row.ID)})
			}
		}
	}
	return gg, differ, cannot
}

func gtGroupFields(k policy.GroupKey, ref model.DeviceID, how policy.RefHow) []ui.KVRow {
	return []ui.KVRow{{Key: "family", Value: string(k.Family)}, {Key: "name", Value: k.Name},
		{Key: "ref", Value: devStr(ref)}, {Key: "how", Value: string(how)}}
}

func noteChoose(order []string, counts map[string]int) gtNote {
	switch {
	case len(order) == 0:
		return gtNote{Choose: "Choose a reference.", Text: " Fewer than two devices were read live, so none is picked for you."}
	case len(order) >= 2 && counts[order[0]] == counts[order[1]]:
		var tied []string
		for i, fp := range order {
			if counts[fp] == counts[order[0]] {
				tied = append(tied, variantLetter(i))
			}
		}
		return gtNote{Choose: "Choose a reference.", Text: " Variants " + strings.Join(tied[:len(tied)-1], ", ") + " and " + tied[len(tied)-1] +
			" are used by " + strconv.Itoa(counts[order[0]]) + " devices each, so none is picked for you."}
	}
	return gtNote{Choose: "Choose a reference.", Text: " No layout is used by most devices."}
}

func gtNoteOf(g templates.GroupView, baseNeedsRef bool, q url.Values, h *handler) gtNote {
	ref := g.Reference
	if ref.How == policy.RefChosen {
		refName := ""
		for _, d := range g.Devices {
			if d.ID == ref.Device.ID {
				refName = d.Name
			}
		}
		n := gtNote{Text: "Reference: chosen, " + refName + " · ", Back: gtBackHref(q, g, ref.Device.ID)}
		if baseNeedsRef {
			n.Tail = " (none: a tie)"
		}
		return n
	}
	t := "Reference: most common, " + strconv.Itoa(ref.Count) + " of " + strconv.Itoa(ref.Of) + " read live"
	if ref.Count == ref.Of {
		t += " · all match"
	}
	return gtNote{Text: t}
}

func gtDiffLines(diff []policy.DiffLine) []gtDiffLine {
	out := make([]gtDiffLine, 0, len(diff))
	for _, l := range diff {
		pin := l.Text
		if i := strings.IndexAny(l.Text, ": "); i > 0 {
			pin = l.Text[:i]
		}
		out = append(out, gtDiffLine{Pin: pin, Have: l.Have, Want: l.Want})
	}
	return out
}

// gtRefQuery is the tab address with the other groups' choices kept and this group's set to id (0 = none).
func gtRefQuery(q url.Values, g templates.GroupView, id model.DeviceID) string {
	var keep []string
	for _, s := range q["ref"] {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		own := slices.ContainsFunc(g.Devices, func(d templates.DeviceView) bool { return int64(d.ID) == n })
		if !own {
			keep = append(keep, s)
		}
	}
	if id != 0 {
		keep = append(keep, devStr(id))
	}
	if len(keep) == 0 {
		return policyTemplatesPath
	}
	v := url.Values{"ref": keep}
	return policyTemplatesPath + "?" + v.Encode()
}

func gtRefHref(q url.Values, g templates.GroupView, id model.DeviceID) string {
	return gtRefQuery(q, g, id)
}
func gtBackHref(q url.Values, g templates.GroupView, _ model.DeviceID) string {
	return gtRefQuery(q, g, 0)
}

// gtPinTable lists the entries of the reference and the device side by side. Pins that are None on
// both are left out (counted in the fine print); the rows that differ are marked.
func gtPinTable(family, device string, ref, dev policy.GPIOTemplate, open bool) *gtPins {
	tab := tasmotacmd.GPIO()
	pins := &gtPins{Device: device, Open: open}
	total, differ, none := len(ref.GPIO)+2, 0, 0
	for i := range ref.GPIO {
		if i >= len(dev.GPIO) {
			break
		}
		a, b := ref.GPIO[i], dev.GPIO[i]
		if a == 0 && b == 0 {
			none++
			continue
		}
		pin, _ := tab.PhysicalPin(family, i)
		label := "GPIO" + strconv.Itoa(pin)
		if family == string(model.ChipESP8266) && pin == 17 {
			label = "ADC0"
		}
		row := gtPinRow{Pin: label, Ref: tab.FunctionName(family, a), Dev: tab.FunctionName(family, b), Bad: a != b}
		if row.Bad {
			differ++
		}
		pins.Rows = append(pins.Rows, row)
	}
	for _, e := range []struct {
		n    string
		a, b int
	}{{"FLAG", ref.Flag, dev.Flag}, {"BASE", ref.Base, dev.Base}} {
		row := gtPinRow{Pin: e.n, Ref: strconv.Itoa(e.a), Dev: strconv.Itoa(e.b), Bad: e.a != e.b}
		if row.Bad {
			differ++
		}
		pins.Rows = append(pins.Rows, row)
	}
	pins.Summary = "Pin table · " + strconv.Itoa(differ) + " of " + strconv.Itoa(total) + " entries differ"
	if none > 0 {
		pins.Fine = strconv.Itoa(none) + " more pins are None on both. "
	}
	pins.Fine += "User pins (set per device with Gpio) are not compared."
	return pins
}

// ---- preview ----

type gtPrevRow struct {
	ID               model.DeviceID
	Name, Href, Meta string
	ReadAt           time.Time
	Diff             []gtPlanDiff
	Cmd              string
	Restart          string
	Warning          string
	ModuleNote       bool
}

type gtPlanDiff struct{ Pin, Have, Want string }

type gtExcl struct{ Name, Href, Why string }

type gtPreview struct {
	Base, CSRF string
	Title      string
	Group      string
	Ref        string
	RefHref    string
	RefHow     string
	RefAt      time.Time
	RefPins    []gtPinRow
	RefFine    string
	Rows       []gtPrevRow
	Excluded   []gtExcl
	Digest     string
	Backup     string
	Restart    string
	Fields     []ui.KVRow
	N          int
	Cancel     string
	Stale      bool
	NeedTick   bool
	Count      string
}

func parseGroupKey(r *http.Request) (policy.GroupKey, model.DeviceID, []model.DeviceID, bool) {
	k := policy.GroupKey{Family: model.ChipFamily(r.PostForm.Get("family")), Name: r.PostForm.Get("name")}
	n, err := strconv.ParseInt(r.PostForm.Get("ref"), 10, 64)
	if k.Family == "" || k.Name == "" || err != nil || n <= 0 {
		return k, 0, nil, false
	}
	var ids []model.DeviceID
	seen := map[model.DeviceID]bool{}
	for _, s := range r.PostForm["pick"] {
		if m, err := strconv.ParseInt(s, 10, 64); err == nil && m > 0 && !seen[model.DeviceID(m)] {
			seen[model.DeviceID(m)] = true
			ids = append(ids, model.DeviceID(m))
		}
	}
	return k, model.DeviceID(n), ids, true
}

func (h *handler) gtForm(w http.ResponseWriter, r *http.Request) (templates.Request, bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return templates.Request{}, false
	}
	k, ref, ids, ok := parseGroupKey(r)
	if !ok {
		h.message(w, r, http.StatusBadRequest, h.view(r, "Nothing to apply", "policy", "Choose a group, a reference and at least one device on the GPIO templates tab."))
		return templates.Request{}, false
	}
	return templates.Request{Group: k, Reference: ref, Devices: ids, Chosen: r.PostForm.Get("how") == string(policy.RefChosen)}, true
}

func (h *handler) gtPreview(w http.ResponseWriter, r *http.Request) {
	req, ok := h.gtForm(w, r)
	if !ok {
		return
	}
	h.gtPreviewRender(w, r, req, http.StatusOK, false, false)
}

func (h *handler) gtPreviewRender(w http.ResponseWriter, r *http.Request, req templates.Request, status int, stale, needTick bool) {
	pv, err := h.d.Templates.Preview(r.Context(), req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, "Apply GPIO template", "policy", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "GPIO templates", Path: policyTemplatesPath}, {Label: "Apply"}}
	p := h.gtPreviewOf(pv, req, r.PostForm.Get("how"), v)
	p.Stale, p.NeedTick = stale, needTick
	h.show(w, r, status, "policy_templates_preview", v, gpioPreviewPage(p))
}

func (h *handler) gtPreviewOf(pv templates.Preview, req templates.Request, how string, v view) gtPreview {
	fam := string(req.Group.Family)
	p := gtPreview{Base: v.Base, CSRF: v.CSRF, Group: req.Group.Name, Ref: pv.Reference.Name, RefHref: h.devHrefOf(pv.Reference.ID),
		RefHow: "Chosen", RefAt: pv.Reference.ReadAt, Digest: pv.Digest, Backup: pv.BackupLine, Restart: strings.TrimPrefix(pv.RestartLine, "Each device restarts once. "), N: len(pv.Rows),
		Count: plural(len(pv.Rows), "device"), Cancel: policyTemplatesPath}
	if how == string(policy.RefMostCommon) {
		p.RefHow = "Most common"
	} else {
		p.Cancel = policyTemplatesPath + "?" + url.Values{"ref": {devStr(req.Reference)}}.Encode()
	}
	p.Title = "Apply the " + req.Group.Name + " template to " + plural(len(pv.Rows), "device")
	p.Fields = gtGroupFields(req.Group, req.Reference, policy.RefHow(how))
	for _, row := range pv.Rows {
		p.Fields = append(p.Fields, ui.KVRow{Key: "pick", Value: devStr(row.Device.ID)})
		r := gtPrevRow{ID: row.Device.ID, Name: row.Device.Name, Href: h.devHrefOf(row.Device.ID), ReadAt: row.Device.ReadAt, Warning: row.Warning,
			Meta: chipLabel(row.Device.Family) + " · runs the template (Module 0) · read live",
			Cmd:  strings.Join(row.Plan.Commands, "\n"), ModuleNote: true}
		for _, l := range gtDiffLines(row.Plan.Diff) {
			r.Diff = append(r.Diff, gtPlanDiff(l))
		}
		r.Restart = row.Device.PowerOnStateText
		p.Rows = append(p.Rows, r)
	}
	for _, e := range pv.Excluded {
		p.Excluded = append(p.Excluded, gtExcl{Name: e.Device.Name, Href: h.devHrefOf(e.Device.ID), Why: e.Why})
	}
	// The reference as a table: the entries that are not None, then FLAG and BASE.
	tab := tasmotacmd.GPIO()
	none := 0
	for i, c := range pv.ReferenceTemplate.GPIO {
		if c == 0 {
			none++
			continue
		}
		pin, _ := tab.PhysicalPin(fam, i)
		label := "GPIO" + strconv.Itoa(pin)
		if req.Group.Family == model.ChipESP8266 && pin == 17 {
			label = "ADC0"
		}
		p.RefPins = append(p.RefPins, gtPinRow{Pin: label, Ref: tab.FunctionName(fam, c)})
	}
	p.RefPins = append(p.RefPins, gtPinRow{Pin: "FLAG", Ref: strconv.Itoa(pv.ReferenceTemplate.Flag)}, gtPinRow{Pin: "BASE", Ref: strconv.Itoa(pv.ReferenceTemplate.Base)})
	if none > 0 {
		p.RefFine = strconv.Itoa(none) + " more pins are None. "
	}
	p.RefFine += "NAME stays \"" + pv.ReferenceTemplate.Name + "\"."
	return p
}

// ---- apply ----

func (h *handler) gtApply(w http.ResponseWriter, r *http.Request) {
	req, ok := h.gtForm(w, r)
	if !ok {
		return
	}
	// The tick is the confirmation that devices restart and relays may switch (maintainer decision on #327).
	if r.PostForm.Get("confirm") != "1" {
		h.gtPreviewRender(w, r, req, http.StatusUnprocessableEntity, false, true)
		return
	}
	pr, _ := auth.FromContext(r.Context())
	batch, refused, err := h.d.Templates.Apply(ota.WithActorKind(r.Context(), pr.Method), actor(r), req, r.PostForm.Get("digest"))
	switch {
	case errors.Is(err, templates.ErrStaleDigest):
		h.gtPreviewRender(w, r, req, http.StatusConflict, true, false)
		return
	case errors.Is(err, templates.ErrNothingToApply):
		h.message(w, r, http.StatusConflict, h.view(r, "Nothing to change", "policy", "Nothing was sent: no device of the selection can be sent the template any more."))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	}
	if len(refused) == 0 {
		redirect(w, r, "/transactions/"+strconv.FormatInt(int64(batch), 10))
		return
	}
	s := fixStarted{Batch: batch}
	for _, x := range refused {
		d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
		s.Skipped = append(s.Skipped, fixSkipView{Label: d.Name, Reason: x.Reason})
	}
	v := h.view(r, "Apply GPIO template", "policy", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: "Policy", Path: policyPath}, {Label: "GPIO templates", Path: policyTemplatesPath}, {Label: "Apply"}}
	h.show(w, r, http.StatusOK, "policy_templates_started", v, gpioStartedPage(v.Base, s))
}
