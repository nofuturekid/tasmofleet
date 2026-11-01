// Package ui holds the templ components of the web UI: the page shell and, as pages are
// ported, the design system's components with typed props. The package knows nothing about
// handlers or the domain; internal/web builds the data and renders the components.
package ui

// ShellData is what the page shell needs from the request: who is looking, which page is
// current, which features are on and the notices to show.
type ShellData struct {
	Title string
	// Nav is the current page's key ("dashboard", "devices", "policy", ...); NavGroup maps it to its menu group.
	Nav  string
	Path string // request path below Base: tells a group's page from a page deeper in it (crumbs)
	Base string // URL path prefix when served below a path
	CSRF string
	// Crumbs is the page's own breadcrumb trail; nil derives one from the navigation (Trail).
	// Not-yet-ported pages that draw their own trail get an empty, non-nil slice.
	Crumbs []Crumb
	// Theme is the user's choice, "light" or "dark"; "" follows the system (Auto).
	Theme string
	// NewDevices is the badge on "Add": discovered devices waiting to be added.
	NewDevices int
	// News is how many releases are new since the user last opened What's new (N5): the avatar's
	// dot and "N new since last visit" in the user menu; 0 draws neither.
	News int
	// Version is the running version as the About page prints it; the header shows it as a small
	// link to What's new. "" draws nothing.
	Version string

	UserName   string
	UserMethod string // how the user signed in: "header", "local" or an identity provider
	RoleName   string
	IsAdmin    bool

	Flash    string // confirmation after a redirect ("Saved.")
	Banner   string // standing notice for admins, e.g. no device networks yet
	Live     bool   // open the SSE stream
	EventSeq uint64 // Hub.Seq when the page was built: the stream asks for a resync when more events followed

	// Status is the bar fixed to the bottom of the page.
	Status StatusBar

	// Features switch menu entries on; a missing service has no page.
	Updates, Firmware, Policy, Discovery, SysConfig, Snapshots bool

	// Asset maps a static file name to its hashed name (file?v=hash), so a new build changes the URL.
	Asset func(name string) string
}

// static is the URL of a static file.
func (d ShellData) static(name string) string {
	if d.Asset != nil {
		name = d.Asset(name)
	}
	return d.Base + "/static/" + name
}
