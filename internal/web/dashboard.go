package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/stability"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The dashboard (design v2 screen "dashboard", WP P1): six stat tiles, the fleet health bar,
// "Needs attention" and the update transactions. The counts are N7's fleetSummary.

const (
	// dashAttentionMax is how many devices "Needs attention" lists (the prototype's slice(0, 8)):
	// a glance, the device list has the rest.
	dashAttentionMax = 8
	// dashProposalsMax is how many discovery proposals it lists; the "New devices found" tile
	// leads to all of them.
	dashProposalsMax = 5
	// dashTxnRows is how many transactions follow the card as rows.
	dashTxnRows = 3
)

// dashView is what the dashboard page draws.
type dashView struct {
	Base, CSRF string
	Total      int
	Sub        string
	// AddHref is the "Add devices" button (admins); empty hides it. NewUpdate likewise.
	AddHref, NewUpdate string
	Tiles              []ui.StatTileData
	OK, Off, Unk       int // the health bar
	Props              []ui.AttProposal
	Items              []ui.AttItem
	// TxnCard is shown when updates are on or transactions exist; Card is the newest one.
	TxnCard bool
	Card    *ui.TxnData
	Rows    []ui.TxnRowData
	// Empty: no devices. The page then shows only the empty state, with these ways to add
	// devices for admins (empty hrefs are not offered).
	Empty                         bool
	ScanHref, ManualHref, ImpHref string
}

func (h *handler) dashboard(w http.ResponseWriter, r *http.Request) {
	seq := h.eventSeq() // before anything is read (#160)
	// An admin on a fresh install lands in the setup until it is finished or skipped.
	if p, _ := auth.FromContext(r.Context()); h.d.Setup != nil && h.d.SysConfig != nil && p.Role >= model.RoleAdmin && !h.d.Setup.Done() {
		redirect(w, r, setupPath)
		return
	}
	rows, err := h.rows(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := h.view(r, "Dashboard", "dashboard", nil)
	v.EventSeq = seq
	v.Live = true // "state streams in live": the body reloads on state changes (TestDashboardIsLive)
	facts := h.fleetFacts(r.Context(), actor(r))
	facts.annotate(rows)
	h.annotateStability(r.Context(), rows)
	s := h.fleetSummaryFor(r.Context(), v, rows, facts)
	var txs []txView
	if h.d.Jobs != nil {
		// Enough jobs to cover the last few transactions, however many devices each has.
		jobs, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{Limit: 500})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		txs = groupTransactions(jobs)
	}
	var props []discovery.Proposal
	if s.HasDiscovery {
		// fleetSummaryFor read them already; a second read is the same list (no device I/O).
		if props, err = h.d.Discovery.Proposals(r.Context()); err != nil {
			props = nil
		}
	}
	devs := map[model.DeviceID]model.Device{}
	for _, row := range rows {
		devs[row.Device.ID] = row.Device
	}
	d := dashView{Base: v.Base, CSRF: v.CSRF, Total: len(rows), Empty: len(rows) == 0}
	d.Sub = fmt.Sprintf("%d %s · state streams in live", len(rows), noun(len(rows), "device", "devices"))
	if v.IsAdmin {
		d.AddHref = v.Base + "/devices/new"
		d.ManualHref = v.Base + "/devices/new"
		if v.Features.Discovery {
			d.ScanHref = v.Base + "/devices/new#card-scan"
		}
		if v.Features.Import {
			d.ImpHref = v.Base + "/devices/new#card-import"
		}
		// Always offered, as in the prototype: with updates switched off the page explains
		// what to configure (FIRMWARE_BASE_URL) instead of the button just missing.
		d.NewUpdate = v.Base + "/updates/new"
	}
	d.Tiles = dashTiles(v.Base, s)
	d.OK, d.Off, d.Unk = s.Online, s.Offline, s.Unknown
	d.Props = dashProposals(v.Base, props, devs)
	now := h.d.Clock.Now()
	d.Items = dashAttention(v.Base, now, rows, facts, h.channelWindow())
	d.TxnCard = v.Features.Updates || len(txs) > 0
	if len(txs) > 0 {
		c := dashTxnCard(v.Base, txs[0])
		d.Card = &c
		for _, t := range txs[1:min(len(txs), 1+dashTxnRows)] {
			d.Rows = append(d.Rows, dashTxnRow(v.Base, now, t))
		}
	}
	h.show(w, r, http.StatusOK, "dashboard", v, dashboardPage(d))
}

// channelWindow is how recent a channel's last contact must be to count as working: the
// configured stale threshold (the same window as the status cell's MQTT/HTTP marks).
func (h *handler) channelWindow() time.Duration {
	if h.d.StaleAfter != nil {
		return h.d.StaleAfter()
	}
	return 11 * time.Minute // the health default (2 x TelePeriod + 60 s)
}

func itoa(n int) string { return strconv.Itoa(n) }

// noun is the word for a count without the number: "device" or "devices".
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// dashTiles are the prototype's six tiles; the policy tile needs a policy report and the
// discovery tile is for admins with discovery on. Each links to the list that shows what it counts.
func dashTiles(base string, s fleetSummary) []ui.StatTileData {
	chip := func(c string) string { return base + "/devices?chip=" + c }
	tiles := []ui.StatTileData{
		{Tone: ui.ToneOK, Label: "Online", Value: itoa(s.Online), Hint: "of " + itoa(s.Total) + " " + noun(s.Total, "device", "devices"), Href: base + "/devices?health=" + string(model.HealthOnline)},
		{Tone: ui.ToneDanger, Label: "Offline", Value: itoa(s.Offline), Hint: "not reachable", Href: chip(chipDown)},
		{Tone: ui.ToneNeutral, Label: "Unknown", Value: itoa(s.Unknown), Hint: "never answered", Href: chip(chipDown)},
	}
	fw := ui.StatTileData{Tone: ui.ToneAccent, Label: "Firmware behind", Value: itoa(s.Outdated), Hint: "older than " + s.Latest, Href: chip(chipOutdated)}
	if s.Latest == "" { // no release list yet: unknown, never "0 behind"
		fw.Value, fw.Hint = "–", "no release list yet"
	}
	tiles = append(tiles, fw)
	if s.HasPolicy {
		pol := ui.StatTileData{Tone: ui.ToneWarn, Label: "Policy deviations", Value: itoa(s.Deviating),
			Hint: noun(s.Deviating, "device", "devices") + ", " + itoa(s.DeviatingRules) + " " + noun(s.DeviatingRules, "rule", "rules"), Href: chip(chipNonCompliant)}
		if s.PolicyUnread { // no value read yet (e.g. just restarted): unknown, never "0 deviations"
			pol.Value, pol.Hint = "–", "not checked yet"
		} else if s.PolicyUnchecked > 0 { // partly read: the known count is a lower bound
			pol.Hint += ", " + itoa(s.PolicyUnchecked) + " not checked yet"
		}
		tiles = append(tiles, pol)
	}
	if s.HasDiscovery {
		hint := "None waiting"
		switch {
		case s.AddressChanged > 0:
			hint = "+ " + itoa(s.AddressChanged) + " address changed"
		case s.NewFound > 0:
			hint = "waiting to be added"
		}
		if s.NewFoundOffline > 0 { // the list below hides them, the Discovered page shows them
			hint += " · " + itoa(s.NewFoundOffline) + " offline"
		}
		tiles = append(tiles, ui.StatTileData{Tone: ui.ToneAccent, Label: "New devices found", Value: itoa(s.NewFound), Hint: hint, Href: base + "/devices/new#card-discovered"})
	}
	return tiles
}

// viaText words where a proposal came from.
func viaText(via string) string {
	if via == discovery.ViaMQTT {
		return "MQTT discovery"
	}
	return "Network scan"
}

// dashProposals are the proposals an admin can act on from the dashboard: new devices (Ignore,
// Add) first, then address changes (Dismiss, Apply). Conflicts need the discovery page.
func dashProposals(base string, props []discovery.Proposal, devs map[model.DeviceID]model.Device) []ui.AttProposal {
	var news, moves []ui.AttProposal
	act := func(p discovery.Proposal, verb string) string {
		return fmt.Sprintf("%s/discovery/proposals/%d/%s", base, p.ID, verb)
	}
	for _, p := range props {
		switch p.Kind {
		case discovery.KindNew:
			if p.LWT == "Offline" {
				continue // an offline proposal is listed on the Discovered page only (#294)
			}
			name := p.Device.Name
			if name == "" {
				name = p.Device.Address
			}
			news = append(news, ui.AttProposal{Name: name, Text: "New device · " + p.Device.Address + " · " + viaText(p.Via),
				Primary: "Add", Secondary: "Ignore", PrimaryAction: act(p, "accept"), SecondaryAction: act(p, "dismiss"), Version: p.Version})
		case discovery.KindAddressChange:
			cur, ok := devs[p.ExistingID]
			if !ok {
				continue
			}
			moves = append(moves, ui.AttProposal{Name: cur.Name, Text: "Address changed · " + cur.Address + " → " + p.Device.Address,
				Primary: "Apply", Secondary: "Dismiss", PrimaryAction: act(p, "accept"), SecondaryAction: act(p, "dismiss"), Version: p.Version})
		}
	}
	out := append(news, moves...)
	return out[:min(len(out), dashProposalsMax)]
}

// dashAttention lists the devices that need a look, worst first: offline, never answered,
// refusing the login, silent, a boot loop, weak Wi-Fi, then policy deviations; at most dashAttentionMax.
func dashAttention(base string, now time.Time, rows []deviceRow, f fleetFacts, window time.Duration) []ui.AttItem {
	type cand struct {
		rank int
		id   model.DeviceID
		it   ui.AttItem
	}
	var cs []cand
	for _, r := range rows {
		it := ui.AttItem{Name: r.Device.Name, Href: fmt.Sprintf("%s/devices/%d", base, r.Device.ID)}
		rank := -1
		switch shownState(r) {
		case model.HealthOffline:
			rank, it.Tone, it.Text = 0, ui.ToneDanger, "Offline"
			if seen := lastContact(r.Device, r.Health); !seen.IsZero() {
				it.Text += " · last seen " + agoWords(now, seen)
			}
		case model.HealthUnknown:
			rank, it.Tone, it.Text = 1, ui.ToneNeutral, "Has not answered since it was added"
		case model.HealthAuthFailed:
			rank, it.Tone, it.Text = 2, ui.ToneDanger, "Refuses the stored web login"
		case model.HealthStale:
			rank, it.Tone = 3, ui.ToneWarn
			it.Text = "No message since " + agoWords(now, r.Health.LastSeen)
			if ch := channelsOf(now, window, r.Health); len(ch) == 2 && ch[0].Tone == "bad" && ch[1].Tone == "ok" {
				it.Text = "MQTT unreachable · HTTP works"
			}
		case model.HealthOnline:
			// answers: only a policy deviation can put it on the list (below)
		}
		if sp := stabProblem(r.Stab); rank < 0 && sp != nil {
			rank, it.Tone, it.Text = 4, ui.ToneDanger, stabAttention(*sp)
			if sp.Level == stability.WeakWiFi {
				rank, it.Tone = 5, ui.ToneWarn
			}
		}
		if rank < 0 && r.Deviations > 0 {
			rank, it.Tone, it.Text = 6, ui.ToneWarn, deviationText(f.Details[r.Device.ID], r.Deviations)
		}
		if rank >= 0 {
			cs = append(cs, cand{rank, r.Device.ID, it})
		}
	}
	// Within a reason, the order the devices were added in (the prototype's list order); the
	// rows come sorted by name.
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].id < cs[j].id })
	var out []ui.AttItem
	for rank := 0; rank <= 6 && len(out) < dashAttentionMax; rank++ {
		for _, c := range cs {
			if c.rank == rank && len(out) < dashAttentionMax {
				out = append(out, c.it)
			}
		}
	}
	return out
}

// lastContact is when the device last answered: in this run, else when its identity was last
// read (stored, so it survives a restart). Zero when it never answered.
func lastContact(d model.Device, hl model.Health) time.Time {
	if !hl.LastSeen.IsZero() {
		return hl.LastSeen
	}
	return d.Identity.DetectedAt
}

// deviationText words a device's deviations like the prototype ("TelePeriod 60 → 300"), or
// counts them when the values are not known.
func deviationText(details []string, n int) string {
	if len(details) > 0 {
		return strings.Join(details, ", ")
	}
	return itoa(n) + " policy " + noun(n, "deviation", "deviations")
}

// dashTxnTitle names a transaction the way the prototype does: "Update to Tasmota 14.3.0", or
// what it does when it is not an update.
func dashTxnTitle(t txView) string {
	if t.Update {
		return "Update to Tasmota " + t.Release
	}
	return t.Release
}

// toneOf maps a transaction tone to a component tone.
func toneOf(t string) ui.Tone {
	switch t {
	case "ok":
		return ui.ToneOK
	case "info":
		return ui.ToneAccent
	case "warn":
		return ui.ToneWarn
	case "bad":
		return ui.ToneDanger
	}
	return ui.ToneNeutral
}

func dashTxnCard(base string, t txView) ui.TxnData {
	parts := []string{fmt.Sprintf("%d of %d done", t.Done, t.Total)}
	for _, x := range []struct {
		n    int
		word string
	}{{t.Failed, "failed"}, {t.Canceled, "cancelled"}, {t.Running, "in progress"}, {t.Queued, "waiting"}} {
		if x.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", x.n, x.word))
		}
	}
	state, tone := t.Outcome, toneOf(t.Tone)
	if t.Running+t.Queued == 0 {
		state, tone = dashResult(t)
	}
	return ui.TxnData{ID: txID(t.ID), Title: dashTxnTitle(t), State: state, StateTone: tone, Summary: strings.Join(parts, " · "),
		OK: t.Done, Failed: t.Failed, Active: t.Running, Total: t.Total, Href: fmt.Sprintf("%s/transactions/%d", base, t.ID)}
}

// dashResult is a transaction's result in the prototype's words: Completed, Cancelled, Failed,
// or the counts when it ended mixed ("30 done · 1 failed").
func dashResult(t txView) (string, ui.Tone) {
	switch {
	case t.Running+t.Queued > 0:
		return "Running", ui.ToneAccent
	case t.Done == t.Total:
		return "Completed", ui.ToneOK
	case t.Canceled == t.Total:
		return "Cancelled", ui.ToneNeutral
	case t.Done == 0 && t.Canceled == 0:
		return "Failed", ui.ToneDanger
	}
	parts := []string{fmt.Sprintf("%d done", t.Done)}
	if t.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", t.Failed))
	}
	if t.Canceled > 0 {
		parts = append(parts, fmt.Sprintf("%d cancelled", t.Canceled))
	}
	tone := ui.ToneWarn
	if t.Failed > 0 {
		tone = ui.ToneDanger
	}
	return strings.Join(parts, " · "), tone
}

func dashTxnRow(base string, now time.Time, t txView) ui.TxnRowData {
	res, tone := dashResult(t)
	return ui.TxnRowData{ID: txID(t.ID), Title: dashTxnTitle(t), Result: res, Tone: tone, When: ui.StampDayText(now, t.Started), WhenAt: t.Started,
		Href: fmt.Sprintf("%s/transactions/%d", base, t.ID)}
}

// dashStacks is the number of card columns of the dashboard: the transactions card sits beside
// "Needs attention" when it is shown, otherwise that card takes the width alone.
func dashStacks(d dashView) int {
	if d.TxnCard {
		return 2
	}
	return 1
}
