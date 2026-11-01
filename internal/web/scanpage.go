package web

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The network scan results page (/discovery/scan, /discovery/scan/{id}): the ranges field with Scan
// now (Stop scan while one runs), the progress of a running scan and the result of one scan: what
// answered as Tasmota, sorted into New, Known and Ignored, with the actions each row allows. The
// Add page's Network scan box holds the same bar and a one-line summary that links here.

// scanTab is the page around the scan body.
type scanTab struct {
	Body scanBodyView
}

// scanBodyView is the part the event stream replaces while a scan runs.
type scanBodyView struct {
	Base, CSRF string
	CIDRs      string // the ranges field
	Prefilled  string // the sentence under the field when DEVICE_NETWORKS ranges were left out of the prefill
	Error      string // a refused range: the sentence under the field
	NoNetworks bool   // Scan now is off: no device network is configured
	HasScan    bool
	ID         string
	Running    bool
	Ranges     string // the scanned ranges, as typed
	// The line beside the button: StatusLead, the moment (StatusAt, shown as a day stamp, since the last
	// scan can be days old) and StatusTail. StatusAt is zero when no moment belongs to it.
	StatusLead, StatusAtText, StatusTail string
	StatusAt                             time.Time
	Done                                 int
	Total                                int
	Found                                int
	Summary                              string
	BoxSummary                           string // the summary as the Add page's box words it: new devices wait in Discovered
	Filter                               string
	Chips                                []scanChip
	Rows                                 []scanRow
	More                                 int    // known devices left out of the All view
	MoreHref                             string // the Known view, which lists them all
	Selectable                           bool   // at least one row can be selected for "Add selected"
	HasDefault                           bool
	// Scheduled is the Add page box's line about the scheduled scan ("off", "every 24h"); SettingsHref
	// is where it is set ("" without the settings page).
	Scheduled, SettingsHref string
}

// Percent is the progress of a running scan.
func (b scanBodyView) Percent() int {
	if b.Total == 0 {
		return 0
	}
	return b.Done * 100 / b.Total
}

// scanChip is one filter of the result: All, New, Known, Ignored with its count.
type scanChip struct {
	Label, Href string
	N           int
	Current     bool
}

// scanRow is one Tasmota device that answered.
type scanRow struct {
	Name, Address, MAC, Firmware string
	Label                        string
	Tone                         ui.Tone
	Kind                         string // new, known, ignored or other (the filter it belongs to)
	ProposalID                   int64
	Version                      string
	AddHref, OpenHref, ReviewRef string
}

// scanKnownShown is how many known devices the All view lists before "and N more".
const scanKnownShown = 6

func (h *handler) scanRender(w http.ResponseWriter, r *http.Request, status int, t scanTab) {
	h.show(w, r, status, "discovery_scan", h.view(r, "Network scan", "discovery", t), scanPage(t))
}

// scanTabData builds the page for one scan (nil: the latest one, if any).
func (h *handler) scanTabData(r *http.Request, s *discovery.ScanStatus) scanTab {
	if s == nil {
		if all := h.d.Discovery.Scans(); len(all) > 0 {
			latest := all[0]
			for _, x := range all[1:] {
				if x.StartedAt.After(latest.StartedAt) {
					latest = x
				}
			}
			s = &latest
		}
	}
	var t scanTab
	if s != nil {
		t.Body = h.scanBodyData(r, *s)
	} else {
		v := h.view(r, "", "", nil)
		t.Body = scanBodyView{Base: v.Base, CSRF: v.CSRF, NoNetworks: v.Banner != "", StatusLead: "No scan yet"}
		// nothing to show yet: start from the networks TasmoFleet already knows (#270)
		if h.d.SysConfig != nil {
			t.Body.CIDRs, t.Body.Prefilled = scanPrefill(h.d.SysConfig.State())
		}
	}
	return t
}

// scanPrefill is the value of the ranges field before any scan ran: the stored DEVICE_NETWORKS
// that one scan accepts (each at most /22, all together at most MaxScanAddresses), in the
// separator the field takes. The sentence names every range that was left out and why, so a
// /16 is not dropped silently.
func scanPrefill(state []sysconfig.FieldState) (ranges, note string) {
	var raw string
	for _, st := range state {
		if st.Key == "DEVICE_NETWORKS" {
			raw = st.Value
		}
	}
	nets, err := parseCIDRs(raw)
	if err != nil {
		return "", "" // a value the settings page would refuse: leave the field empty
	}
	var use, tooBig, tooMany []string
	var total uint64
	for _, p := range nets {
		p = p.Masked()
		host := p.Addr().BitLen() - p.Bits()
		switch {
		case host > 10:
			tooBig = append(tooBig, p.String())
		case total+1<<host > discovery.MaxScanAddresses:
			tooMany = append(tooMany, p.String())
		default:
			total += 1 << host
			use = append(use, p.String())
		}
	}
	var parts []string
	if len(tooBig) > 0 {
		parts = append(parts, strings.Join(tooBig, ", ")+" spans more than "+strconv.Itoa(discovery.MaxScanAddresses)+" addresses (a scan covers at most /22 per range)")
	}
	if len(tooMany) > 0 {
		parts = append(parts, strings.Join(tooMany, ", ")+" would take one scan past "+strconv.Itoa(discovery.MaxScanAddresses)+" addresses in total")
	}
	if len(parts) > 0 {
		note = "Left out of the prefilled ranges: " + strings.Join(parts, "; ") + ". Type a smaller range inside it to scan part of it."
	}
	return strings.Join(use, ", "), note
}

func (h *handler) scanBodyData(r *http.Request, s discovery.ScanStatus) scanBodyView {
	v := h.view(r, "", "", nil)
	now := h.d.Clock.Now()
	b := scanBodyView{Base: v.Base, CSRF: v.CSRF, NoNetworks: v.Banner != "", HasScan: true, ID: string(s.ID),
		Running: s.State == discovery.ScanRunning, Done: s.Done, Total: s.Total, Found: len(s.Hits),
		HasDefault: h.d.DefaultDevicePassword != nil && h.d.DefaultDevicePassword() != ""}
	var ranges []string
	for _, c := range s.CIDRs {
		ranges = append(ranges, c.String())
	}
	b.Ranges = strings.Join(ranges, ", ")
	b.CIDRs = b.Ranges
	// The moment is a day stamp: the last scan can be older than today, so a bare clock time would lie.
	switch {
	case b.Running:
		b.StatusLead, b.StatusAt, b.StatusTail = "Started ", s.StartedAt, " by "+h.pr.Text(s.Actor)
	case s.FinishedAt.IsZero():
		b.StatusLead, b.StatusAt = "Last scan started ", s.StartedAt
	case s.State == discovery.ScanDone:
		b.StatusLead, b.StatusAt, b.StatusTail = "Last scan ", s.FinishedAt, " · took "+humanDuration(s.FinishedAt.Sub(s.StartedAt).Round(time.Second))
	default:
		b.StatusLead, b.StatusAt = "Last scan "+string(s.State)+" ", s.FinishedAt
	}
	b.StatusAtText = ui.StampDayText(now, b.StatusAt)

	byMAC := h.proposalsByMAC(r)
	devs, err := h.d.Inventory.List(r.Context())
	if err != nil {
		h.log.WarnContext(r.Context(), "scan page: inventory", "err", err)
	}
	devByMAC := map[string]model.Device{}
	for _, d := range devs {
		for _, m := range append([]string{d.MAC}, d.MACs...) {
			if m != "" {
				devByMAC[m] = d
			}
		}
	}
	var rows []scanRow
	counts := map[string]int{}
	for _, hit := range s.Hits {
		row := h.scanRowOf(b.Base, hit, byMAC, devByMAC)
		counts[row.Kind]++
		rows = append(rows, row)
	}
	order := map[string]int{"new": 0, "ignored": 1, "other": 2, "known": 3}
	sort.SliceStable(rows, func(i, j int) bool { return order[rows[i].Kind] < order[rows[j].Kind] })

	b.Filter = r.URL.Query().Get("f")
	if _, ok := counts[b.Filter]; !ok && b.Filter != "new" && b.Filter != "known" && b.Filter != "ignored" {
		b.Filter = ""
	}
	chip := func(label, f string, n int) scanChip {
		href := v.Base + "/discovery/scan/" + string(s.ID) // the chips filter this scan, also in a streamed fragment
		if f != "" {
			href += "?f=" + url.QueryEscape(f)
		}
		return scanChip{Label: label, Href: href, N: n, Current: b.Filter == f}
	}
	b.Chips = []scanChip{chip("All", "", len(rows)), chip("New", "new", counts["new"]), chip("Known", "known", counts["known"]), chip("Ignored", "ignored", counts["ignored"])}
	known := 0
	for _, row := range rows {
		switch {
		case b.Filter != "" && row.Kind != b.Filter:
			continue
		case b.Filter == "" && row.Kind == "known":
			known++
			if known > scanKnownShown {
				b.More++
				continue
			}
		}
		b.Selectable = b.Selectable || row.Kind == "new"
		b.Rows = append(b.Rows, row)
	}
	if b.More > 0 {
		b.MoreHref = chip("Known", "known", 0).Href
	}
	if !b.Running {
		addrs := strconv.Itoa(s.Total) + " addresses"
		if s.Total == 1 {
			addrs = "1 address"
		}
		found := plural(len(s.Hits), "Tasmota device")
		b.Summary = fmt.Sprintf("%s · %s: %d already known, %d new, %d ignored.", addrs, found, counts["known"], counts["new"], counts["ignored"])
		b.BoxSummary = fmt.Sprintf("%s · %s: %d already known, %d new (in Discovered), %d ignored.", addrs, found, counts["known"], counts["new"], counts["ignored"])
	}
	return b
}

// proposalsByMAC maps the MAC of every proposal, listed or ignored, to it; Dismissed marks the
// ignored ones.
func (h *handler) proposalsByMAC(r *http.Request) map[string]scanProposal {
	out := map[string]scanProposal{}
	if props, err := h.d.Discovery.Proposals(r.Context()); err == nil {
		for _, p := range props {
			out[p.Device.MAC] = scanProposal{P: p}
		}
	}
	if ign, err := h.d.Discovery.Ignored(r.Context()); err == nil {
		for _, p := range ign {
			out[p.Device.MAC] = scanProposal{P: p, Dismissed: true}
		}
	}
	return out
}

type scanProposal struct {
	P         discovery.Proposal
	Dismissed bool
}

// scanRowOf sorts one hit into the result: a new device can be added or ignored, an ignored one
// restored, a known one opened; an address change or conflict waits in the Discovered box of the Add page.
func (h *handler) scanRowOf(base string, hit discovery.Hit, byMAC map[string]scanProposal, devs map[string]model.Device) scanRow {
	row := scanRow{Name: hit.Name, Address: netaddr.Display(hit.Address, hit.Port, "http"), MAC: strings.ToUpper(hit.MAC), Firmware: hit.Version, Kind: "other"}
	if row.Name == "" {
		row.Name = row.Address
	}
	if d, ok := devs[hit.MAC]; ok && hit.MAC != "" {
		row.Name, row.Label, row.Tone, row.Kind = d.Name, "Already known", ui.ToneNeutral, "known"
		row.OpenHref = base + "/devices/" + strconv.FormatInt(int64(d.ID), 10)
		return row
	}
	sp, ok := byMAC[hit.MAC]
	switch {
	case ok && sp.Dismissed:
		row.Label, row.Tone, row.Kind, row.ProposalID = "Ignored", ui.ToneNeutral, "ignored", sp.P.ID
	case ok && sp.P.Kind == discovery.KindNew:
		row.Label, row.Tone, row.Kind, row.ProposalID, row.Version = "New", ui.ToneAccent, "new", sp.P.ID, sp.P.Version
		row.AddHref = base + "/devices/new?proposal=" + strconv.FormatInt(sp.P.ID, 10) + "#card-manual"
	case ok && sp.P.Kind == discovery.KindAddressChange:
		row.Label, row.Tone, row.ReviewRef = "Address changed", ui.ToneWarn, base+"/devices/new"
	case ok:
		row.Label, row.Tone, row.ReviewRef = "Conflict", ui.ToneDanger, base+"/devices/new"
	case hit.Outcome == "needs password":
		row.Label, row.Tone = "Needs password", ui.ToneWarn
		row.AddHref = base + "/devices/new?address=" + url.QueryEscape(hit.Address) + "&port=" + strconv.Itoa(hit.Port) + "#card-manual"
	case hit.Outcome == "error":
		row.Label, row.Tone = "No valid answer", ui.ToneDanger
	default:
		row.Label, row.Tone = "Not usable", ui.ToneNeutral
	}
	return row
}

// discoveryScanLatest is the results page of the latest scan, or the empty field.
func (h *handler) discoveryScanLatest(w http.ResponseWriter, r *http.Request) {
	h.scanRender(w, r, http.StatusOK, h.scanTabData(r, nil))
}

// discoveryScanPage is the results page of one scan (where Scan now leads).
func (h *handler) discoveryScanPage(w http.ResponseWriter, r *http.Request) {
	s, ok := h.d.Discovery.Scan(discovery.ScanID(r.PathValue("id")))
	if !ok {
		h.notFound(w, r)
		return
	}
	h.scanRender(w, r, http.StatusOK, h.scanTabData(r, &s))
}

// scanStreamURL is the event stream of a running scan. It carries the chosen filter, or the first
// event would reset the chips to "All".
func scanStreamURL(b scanBodyView) string {
	u := b.Base + "/discovery/scan/" + b.ID + "/stream"
	if b.Filter != "" {
		u += "?f=" + url.QueryEscape(b.Filter)
	}
	return u
}

// scanBoxData is the Network scan box of the Add page: the latest scan's bar and summary.
func (h *handler) scanBoxData(r *http.Request) scanBodyView {
	b := h.scanTabData(r, nil).Body
	if h.d.SysConfig != nil {
		b.Scheduled = "off"
		b.SettingsHref = b.Base + sysPath + "?tab=discovery"
		if h.storedValue("DISCOVERY_SCAN") == "true" {
			b.Scheduled = "every " + h.storedValue("DISCOVERY_SCAN_INTERVAL")
		}
	}
	return b
}
