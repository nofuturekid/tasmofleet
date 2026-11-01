package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
)

// The Add page (#410): one page with four boxes, one per way to add devices: Discovered and
// Network scan in the wide column, Add manually and Import from TasmoAdmin in the narrow one. The
// page is /devices/new; the boxes post to the routes of their own service (/discovery/..., the
// import fragments). A box whose service is not configured is left out. The network scan's full
// result lives on its own page (/discovery/scan/{id}).

// addPage is the view of the Add page.
type addPage struct {
	Base, CSRF string
	Manual     addManualView
	Disc       *discView     // nil without discovery
	Scan       *scanBodyView // nil without discovery
	Import     *importBody   // nil without the web import
}

// addTitle names the page in the window title and the phone header.
const addTitle = "Add devices"

// renderAdd draws the Add page. The manual form comes from the caller (an empty one, a prefilled
// one, or the one a refused add came back with), and so does the discovery list when an action on
// it was refused; what is nil is read afresh.
func (h *handler) renderAdd(w http.ResponseWriter, r *http.Request, status int, f addManualView, d *discView) {
	v := h.view(r, addTitle, "discovery", nil)
	f.Base, f.CSRF = v.Base, v.CSRF
	f.Device.HTTPPassword, f.Device.MQTTPassword = "", "" // never rendered
	f.TagsText = strings.Join(f.Device.Tags, ", ")
	f.HasDefault = h.d.DefaultDevicePassword != nil && h.d.DefaultDevicePassword() != ""
	f.NoNetworks = v.Banner != ""
	page := addPage{Base: v.Base, CSRF: v.CSRF, Manual: f}
	if h.d.Discovery != nil {
		if d == nil {
			x := h.discoveryData(r)
			d = &x
		}
		d.Base, d.CSRF = v.Base, v.CSRF
		page.Disc = d
		b := h.scanBoxData(r)
		page.Scan = &b
	}
	if h.d.Import != nil {
		b := h.importBodyOf(r)
		page.Import = &b
	}
	switch r.URL.Query().Get("ok") {
	case "accepted":
		v.Flash = "Device added."
	case "moved":
		v.Flash = "Address updated."
	case "dismissed":
		v.Flash = "Dismissed."
	case "undismissed":
		v.Flash = "Restored."
	}
	h.show(w, r, status, "add", v, addPageView(page))
}

// addManualView is the Add manually box: the prototype's address and web password, the fixed
// web user, and the remaining device fields folded under "More options".
type addManualView struct {
	Base, CSRF string
	Device     model.Device
	TagsText   string
	Error      string // a problem that belongs to no single field (banner)
	ErrField   string // the field Error belongs to
	Fallback   bool   // offer "Add without probing" after a failed probe
	// Tested is the device a "Test connection" found (nothing stored).
	Tested *model.Device
	// Prefill names the discovery proposal the form was filled from (Clear empties it).
	Prefill *addPrefill
	// HasDefault: a fleet default device password is set (N20). The value never reaches the page.
	HasDefault bool
	// NoNetworks: no device network is configured, every address would be refused.
	NoNetworks bool
}

// addPrefill is the banner of a form opened from a discovered device.
type addPrefill struct {
	Source, Name, Host, Address string
}

// MoreOpen keeps the "More options" fold open when one of its fields has a value or an error.
func (v addManualView) MoreOpen() bool {
	switch v.ErrField {
	case "name", "port", "tags", "notes":
		return true
	}
	return v.Device.Name != "" || v.Device.Port != 0 || v.TagsText != "" || v.Device.Notes != "" || v.Device.OTAExcluded
}

// FieldError is the sentence under the named input, or "".
func (v addManualView) FieldError(name string) string {
	if v.ErrField == name {
		return v.Error
	}
	return ""
}

// PasswordHelp is the line under the web password: where a prefilled password comes from, or
// what the field is for.
func (v addManualView) PasswordHelp() string {
	switch {
	case v.HasDefault && v.Prefill == nil:
		return "Prefilled from Settings → Devices."
	case v.HasDefault:
		return "The default password is not sent to a discovered address; type it if the device has one."
	}
	return "Only if the device has one. Stored encrypted."
}

// PasswordPlaceholder shows that the default password fills the empty field (the dots stand
// for it; the value itself never reaches the page).
func (v addManualView) PasswordPlaceholder() string {
	if v.HasDefault && v.Prefill == nil {
		return strings.Repeat("•", 16)
	}
	return ""
}

// deviceNewForm is the Add page. ?proposal=<id> fills the manual form from a discovered device
// (the "Add…" of the Discovered box); ?address= and ?port= from a scan result.
func (h *handler) deviceNewForm(w http.ResponseWriter, r *http.Request) {
	var f addManualView
	q := r.URL.Query()
	if id, err := strconv.ParseInt(q.Get("proposal"), 10, 64); err == nil && h.d.Discovery != nil {
		if p, ok := h.proposalByID(r, id); ok && p.Kind == discovery.KindNew {
			f.Device = model.Device{Name: p.Device.Name, Address: p.Device.Address, Port: p.Device.Port}
			if f.Device.Port == 80 {
				f.Device.Port = 0
			}
			f.Prefill = &addPrefill{Source: viaText(p.Via), Name: p.Device.Name, Host: p.Device.Topic, Address: netaddr.Display(p.Device.Address, p.Device.Port, "http")}
		}
	} else if a := strings.TrimSpace(q.Get("address")); a != "" && h.d.Discovery != nil {
		port, _ := strconv.Atoi(q.Get("port")) // a bad port is just no port
		if port == 80 || port < 0 || port > 65535 {
			port = 0
		}
		f.Device = model.Device{Address: a, Port: port}
		f.Prefill = &addPrefill{Source: "network scan", Address: netaddr.Display(a, port, "http")}
	}
	h.renderAdd(w, r, http.StatusOK, f, nil)
}

// proposalByID finds a listed (not dismissed) proposal.
func (h *handler) proposalByID(r *http.Request, id int64) (discovery.Proposal, bool) {
	props, err := h.d.Discovery.Proposals(r.Context())
	if err != nil {
		h.log.WarnContext(r.Context(), "discovery proposals", "err", err)
		return discovery.Proposal{}, false
	}
	for _, p := range props {
		if p.ID == id {
			return p, true
		}
	}
	return discovery.Proposal{}, false
}

// testedText is what "Test connection" found: the name and what it runs.
func testedText(d model.Device) string {
	s := d.Name
	if s == "" {
		s = "The device"
	}
	var parts []string
	if d.Identity.Hardware != "" {
		parts = append(parts, d.Identity.Hardware)
	}
	if d.Identity.Version != "" {
		parts = append(parts, "Tasmota "+d.Identity.Version)
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s + " answered."
}

// portText is the Port field: empty for the default.
func portText(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

// dash shows a value that is not known as a dash.
func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// metaTail is " · a · b" without the parts that are empty: a phone card reads "127.0.0.1"
// rather than "127.0.0.1 · —" (the table keeps its dash per column).
func metaTail(parts ...string) string {
	var b strings.Builder
	for _, p := range parts {
		if p != "" {
			b.WriteString(" · " + p)
		}
	}
	return b.String()
}

// sortHeader is the link of a sortable header of the New devices table: the current column
// flips its direction, any other starts ascending; filter and ignored switch are kept.
func sortHeader(p discView, key string) (href, class, aria string) {
	dir, class, aria := "asc", "sortlink", "none"
	if key == p.Sort {
		class += " is-" + p.Dir
		aria = map[string]string{"asc": "ascending", "desc": "descending"}[p.Dir]
		if p.Dir == "asc" {
			dir = "desc"
		}
	}
	return discHref(p.Base, p.State, key, dir, p.ShowIgn), class, aria
}

// showIgnoredHref is the "Show ignored" switch: a link to the other state.
func showIgnoredHref(p discView) string {
	return discHref(p.Base, p.State, p.Sort, p.Dir, !p.ShowIgn)
}

// listSep is the separator before item i of n in running text: ", " or " and ".
func listSep(i, n int) string {
	if i == n-1 {
		return " and "
	}
	return ", "
}

// nameTip is the tooltip of the name cell: the name with the MQTT topic, which has no column.
func nameTip(r propRow) string {
	if r.Topic == "" {
		return dash(r.Name)
	}
	return dash(r.Name) + " · topic " + r.Topic
}

// cardHost is the hostname line of a phone card; it is left out when it only repeats the name.
func cardHost(r propRow) string {
	if r.Hostname == r.Name {
		return ""
	}
	return r.Hostname
}
