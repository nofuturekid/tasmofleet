package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// prefTheme is the user preference holding the theme choice (D11): "light" or "dark"; no row
// means Auto, which follows the system.
const prefTheme = "theme"

// shellData is what the templ page shell shows of a page view.
func shellData(v view) ui.ShellData {
	return ui.ShellData{
		Title: v.Title, Nav: v.Nav, Path: v.Path, Base: v.Base, CSRF: v.CSRF,
		UserName: v.User.Name, UserMethod: v.User.Method, RoleName: v.RoleName, IsAdmin: v.IsAdmin,
		Flash: v.Flash, Banner: v.Banner, Live: v.Live, EventSeq: v.EventSeq, Theme: v.Theme, NewDevices: v.NewDevices, News: v.News, Version: v.Version,
		Updates: v.Features.Updates, Firmware: v.Features.Firmware, Policy: v.Features.Policy,
		Discovery: v.Features.Discovery, SysConfig: v.Features.SysConfig, Snapshots: v.Features.Snapshots,
		Crumbs: v.Crumbs,
		Asset:  asset,
	}
}

// themeOf reads the stored theme choice; anything but light or dark is Auto.
func themeOf(prefs map[string]string) string {
	var t string
	if json.Unmarshal([]byte(prefs[prefTheme]), &t) == nil && (t == "light" || t == "dark") {
		return t
	}
	return ""
}

// validateTheme accepts the theme preference: a JSON string light, dark or auto. Auto is the
// absence of a choice, so it deletes the row ("").
func validateTheme(raw []byte) (string, error) {
	var t string
	if err := decodeStrict(raw, &t); err != nil {
		return "", err
	}
	switch t {
	case "auto":
		return "", nil
	case "light", "dark":
		return marshalPref(t)
	}
	return "", errPref
}

// page writes a full page: the shell around content. A not-yet-ported page passes its legacy
// content; when that content draws its own breadcrumb trail, the shell draws none.
func (h *handler) page(r *http.Request, v view, prefs map[string]string, content templ.Component) ([]byte, error) {
	v.Theme = themeOf(prefs)
	v.News = newsCount(prefs)
	v.Version = h.d.Version
	if v.IsAdmin && h.d.Discovery != nil { // full pages only: fragments and POST results draw no navigation
		v.NewDevices = h.newDeviceCount(r)
	}
	d := shellData(v)
	d.Status = h.statusBar(r.Context(), v.Base, v.Live)
	var inner bytes.Buffer
	if err := content.Render(r.Context(), &inner); err != nil {
		return nil, err
	}
	// Only a legacy trail (nav.crumb.trail) replaces the shell's; a ui.Crumbs inside the content
	// (the style guide's sample) is content.
	if bytes.Contains(inner.Bytes(), []byte(`<nav class="crumb trail" aria-label="Breadcrumb">`)) {
		d.Crumbs = []ui.Crumb{}
	}
	var buf bytes.Buffer
	if err := ui.Shell(d, rendered(inner.Bytes())).Render(r.Context(), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// bare writes a page without the navigation (ui.Bare): the first-run setup.
func (h *handler) bare(r *http.Request, v view, content templ.Component) ([]byte, error) {
	v.Theme = themeOf(h.userPrefs(r))
	var buf bytes.Buffer
	if err := ui.Bare(shellData(v), content).Render(r.Context(), &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// rendered is markup a component has already produced (escaped by its own renderer).
func rendered(b []byte) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// writePage sends a rendered page with the headers every page has.
func writePage(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b) // #nosec G705 -- the page was rendered by templ and html/template, which escape every value; a failed write means the client went away
}

// navPage is /nav: the phone drawer as a page, for browsers without JavaScript (the menu button
// is a link here).
func (h *handler) navPage(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Menu", "", nil)
	prefs := h.userPrefs(r)
	v.Theme = themeOf(prefs)
	v.Version = h.d.Version // page sets it too, but the drawer content is built from v before that
	if v.IsAdmin && h.d.Discovery != nil {
		v.NewDevices = h.newDeviceCount(r)
	}
	b, err := h.page(r, v, prefs, ui.NavList(shellData(v)))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writePage(w, http.StatusOK, b)
}
