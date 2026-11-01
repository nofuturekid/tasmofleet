package ui

import "strings"

// The navigation model of the shell (design v2, design/v2/README.md):
// four groups in the top bar, the group's pages in the sub-navigation, the same lists in the
// phone drawer and on /nav. One model feeds all of them, so they cannot drift apart.

// NavItem is one page of a group.
type NavItem struct {
	Key, Label, Path string
	Current          bool
	Badge            int // a count that needs attention (new devices on "Add"); 0 = none
}

// NavGroupData is one top-bar group with its pages. Path is where the group button leads: its
// first page the user may open (an operator's System button leads to the style guide).
type NavGroupData struct {
	Key, Label, Path string
	Current          bool
	Items            []NavItem
}

// Crumb is one step of the breadcrumb trail; a crumb without Path is plain text. KeepQuery marks
// a link to a list whose filter and sort listback.js restores from the browser tab (#127).
type Crumb struct {
	Label, Path string
	KeepQuery   bool
}

// NavGroup names the group a page belongs to ("dashboard", "devices", "updates", "system"). Pages
// outside the menu (messages, not found, the setup) belong to Dashboard, as the prototype's error
// screens do, so the top bar keeps its sub-navigation row and the title never jumps.
func NavGroup(nav string) string {
	switch nav {
	case "dashboard", "", "setup":
		return "dashboard"
	case "devices", "discovery", "policy":
		return "devices"
	case "updates", "transactions", "firmware":
		return "updates"
	case "system", "snapshots", "users", "audit", "about", "styleguide", "account":
		return "system"
	}
	return "dashboard"
}

// navItemKey is the sub-navigation entry a page lights up.
func navItemKey(nav string) string {
	switch nav {
	case "discovery":
		return "add"
	case "updates":
		return "transactions"
	case "system":
		return "settings"
	}
	return nav
}

// Groups lists the groups and pages of the navigation. The switched-on features decide (a
// missing service has no page, so it has no entry); the role does not: every role sees the same
// navigation and each page enforces its own permission (design v2 F4, plan S7), so the menu
// reads the same for everybody and the style guide is reachable for all.
func (d ShellData) Groups() []NavGroupData {
	type entry struct {
		key, label, path string
		on               bool
		admin            bool // the page answers 403 to other roles
	}
	cur := navItemKey(d.Nav)
	build := func(key, label string, items []entry) NavGroupData {
		g := NavGroupData{Key: key, Label: label, Current: NavGroup(d.Nav) == key}
		for _, e := range items {
			if e.on {
				it := NavItem{Key: e.key, Label: e.label, Path: e.path, Current: e.key == cur && g.Current}
				if e.key == "add" {
					it.Badge = d.NewDevices
				}
				g.Items = append(g.Items, it)
			}
		}
		// The group button leads to the first page this user may open, so it is never a 403.
		for _, e := range items {
			if e.on && (d.IsAdmin || !e.admin) {
				g.Path = e.path
				break
			}
		}
		if g.Path == "" && len(g.Items) > 0 {
			g.Path = g.Items[0].Path
		}
		return g
	}
	out := []NavGroupData{
		build("dashboard", "Dashboard", []entry{{"dashboard", "Overview", "/", true, false}}),
		build("devices", "Devices", []entry{
			{"devices", "List", "/devices", true, false},
			{"policy", "Policy", "/policy", d.Policy, false},
			{"add", "Add", "/devices/new", true, true},
		}),
	}
	if g := build("updates", "Updates", []entry{
		{"transactions", "Transactions", "/transactions", d.Updates, false},
		{"firmware", "Firmware", "/firmware", d.Firmware, false},
	}); len(g.Items) > 0 {
		out = append(out, g)
	}
	out = append(out, build("system", "System", []entry{
		{"settings", "Settings", "/settings/system", d.SysConfig, true},
		{"snapshots", "Snapshots", "/settings/system/snapshots", d.Snapshots, true},
		{"audit", "Audit log", "/audit", true, true},
		{"users", "Users", "/users", true, true},
		{"about", "About", "/about", true, true},
		{"styleguide", "Styleguide", "/styleguide", true, false},
	}))
	return out
}

// currentGroup is the group of the page, if the user can see it.
func (d ShellData) currentGroup() (NavGroupData, bool) {
	for _, g := range d.Groups() {
		if g.Current {
			return g, true
		}
	}
	return NavGroupData{}, false
}

// Trail is the breadcrumb trail of the page: the page's own (Crumbs) when the handler built
// one, else group / page / title from the navigation model, like the prototype's PATH table.
// Pages outside the groups get Dashboard / title.
func (d ShellData) Trail() []Crumb {
	if d.Crumbs != nil {
		return d.Crumbs
	}
	home := Crumb{Label: "Dashboard", Path: "/"}
	g, ok := d.currentGroup()
	if !ok || len(g.Items) == 0 {
		return []Crumb{home, {Label: d.Title}}
	}
	var item NavItem
	first := false
	for i, it := range g.Items {
		if it.Current {
			item, first = it, i == 0
		}
	}
	onItem := item.Key != "" && strings.TrimSuffix(d.Path, "/") == strings.TrimSuffix(item.Path, "/")
	if g.Key == "dashboard" {
		if onItem {
			return []Crumb{{Label: "Dashboard"}}
		}
		return []Crumb{home, {Label: d.Title}}
	}
	group := Crumb{Label: g.Label}
	if g.Key != "system" { // System has no page of its own
		group.Path = g.Path
	}
	switch {
	case item.Key == "": // a page of the group that is no menu entry (the account page)
		return []Crumb{group, {Label: d.Title}}
	case onItem && first && g.Key != "system":
		return []Crumb{{Label: g.Label}}
	case onItem:
		return []Crumb{group, {Label: item.Label}}
	case first && g.Key != "system":
		return []Crumb{group, {Label: d.Title}}
	}
	if d.Title == item.Label { // the page is the entry's own answer (POST result): no doubled crumb
		return []Crumb{group, {Label: item.Label}}
	}
	return []Crumb{group, {Label: item.Label, Path: item.Path}, {Label: d.Title}}
}

// Initials are the avatar's letters: the first letters of the first two words of the name, or
// the first two letters of a one-word name, upper case.
func Initials(name string) string {
	f := strings.Fields(name)
	var r []rune
	switch {
	case len(f) >= 2:
		r = append([]rune(f[0])[:1], []rune(f[1])[:1]...)
	case len(f) == 1:
		r = []rune(f[0])
		if len(r) > 2 {
			r = r[:2]
		}
	default:
		return "?"
	}
	return strings.ToUpper(string(r))
}

// FirstName is the name on the avatar button: the first word of the display name.
func FirstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

// RoleLine says what the user may do and how they signed in, under their name in the user menu.
func (d ShellData) RoleLine() string {
	role := d.RoleName
	if role != "" {
		role = strings.ToUpper(role[:1]) + role[1:]
	}
	switch d.UserMethod {
	case "local":
		return role + " · Local account"
	case "header":
		return role + " · Signed in by the proxy"
	case "", "oidc":
		return role + " · SSO"
	}
	return role + " · SSO (" + d.UserMethod + ")"
}
