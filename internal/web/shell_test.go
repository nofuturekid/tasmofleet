package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// shellPages are the full pages the test harness can render without extra fixtures: every
// page family (lists, forms, device pages, settings, the style guide, the 404 page).
var shellPages = []string{
	"/", "/devices", "/devices/new", "/devices/1", "/devices/1/console", "/devices/1/backups", "/devices/1/settings",
	"/about", "/styleguide", "/users", "/audit", "/settings", "/settings/system", "/firmware", "/transactions",
	"/updates/new", "/policy", "/policy/rules", "/discovery/scan", "/settings/system/snapshots", "/setup", "/account/password",
	"/no/such/page", "/?ok=saved", "/nav",
}

// Why: pages are being moved to templ one at a time (design/v2/README.md,
// section 4). Until the last one is moved, every page - ported or not - has to come out of the
// one templ shell with exactly one main region, or a page that skips it loses the menu, the
// CSRF header for htmx and the live stream.
func TestEveryPageRendersThroughTheShell(t *testing.T) {
	e := setup(t)
	for _, role := range []string{"admin", "operator", "viewer"} {
		for _, p := range shellPages {
			resp, body := e.do(t, "GET", p, role, nil)
			if resp.StatusCode >= 500 {
				t.Fatalf("%s %s: status %d", role, p, resp.StatusCode)
			}
			if resp.StatusCode != 200 && resp.StatusCode != 404 {
				continue // a redirect or a refusal has no page
			}
			if !strings.HasPrefix(body, "<!doctype html>") || strings.Count(body, `<main id="main">`) != 1 || strings.Count(body, "</main>") != 1 ||
				!strings.Contains(body, `<header class="top">`) || !strings.Contains(body, `hx-headers='{`) && !strings.Contains(body, `hx-headers="{`) {
				t.Errorf("%s %s: not the app shell: %.200s", role, p, body)
			}
		}
	}
}

var inlineCode = regexp.MustCompile(`(?i)(<script\b[^>]*>[^<]|\sstyle=|<style\b|\son[a-z]+=|javascript:)`)

// Why: the content security policy is `default-src 'self'` with no 'unsafe-inline'. templ makes
// inline script and style one keyword away (script components, templ.Raw, style attributes);
// one such page would be blocked by every browser and silently half dead. This renders every
// page family and fails on any inline code.
func TestNoPageCarriesInlineScriptOrStyle(t *testing.T) {
	e := setup(t)
	for _, p := range shellPages {
		_, body := e.do(t, "GET", p, "admin", nil)
		if m := inlineCode.FindString(body); m != "" {
			i := strings.Index(body, m)
			t.Errorf("%s: inline code %q in %.120s", p, m, body[max(0, i-40):])
		}
	}
}

// Why (F4, plan S7): the navigation is the same for every role, so the menu reads the same for
// everybody and the style guide (open to all, D17) is reachable from the System group; each page
// enforces its own permission. The shell still says who is signed in and with which role.
func TestShellMenuMatchesRole(t *testing.T) {
	e := setup(t)
	_, admin := e.do(t, "GET", "/devices", "admin", nil)
	_, operator := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(admin, ">System<") || !strings.Contains(operator, ">System<") {
		t.Fatalf("System group: admin %v, operator %v", strings.Contains(admin, ">System<"), strings.Contains(operator, ">System<"))
	}
	if resp, _ := e.do(t, "GET", "/audit", "operator", nil); resp.StatusCode != 403 {
		t.Fatalf("Audit log is in the operator's menu, so the page itself must refuse: status %d", resp.StatusCode)
	}
	if !strings.Contains(operator, "Operator · ") {
		t.Fatalf("the role is not shown: %.300s", operator)
	}
}

// Why: a page that draws its own breadcrumb trail (not ported yet) must not get a second one
// from the shell; a page without one gets the shell's.
func TestOneTrailPerPage(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/devices/1", "/devices", "/about", "/audit"} {
		_, body := e.do(t, "GET", p, "admin", nil)
		if n := strings.Count(body, `aria-label="Breadcrumb"`); n != 1 {
			t.Errorf("%s: %d breadcrumb trails", p, n)
		}
	}
}

// Why: D11: the theme choice is per user and rendered by the server, so the next page opens in
// it without a flash; Auto removes the choice; anything else is refused, not stored.
func TestThemePreference(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Prefs = newFakePrefs() })
	for _, c := range []struct {
		body string
		want int
	}{{`"dark"`, 204}, {`"auto"`, 204}, {`"light"`, 204}, {`"purple"`, 400}, {`dark`, 400}} {
		if resp, _ := e.doJSON(t, "/prefs/theme", "operator", c.body); resp.StatusCode != c.want {
			t.Errorf("theme %s: status %d, want %d", c.body, resp.StatusCode, c.want)
		}
	}
	// the last accepted choice was light
	if _, page := e.do(t, "GET", "/devices", "operator", nil); !strings.Contains(page, `<html lang="en" data-theme="light">`) {
		t.Errorf("stored theme not rendered: %.120s", page)
	}
	e.doJSON(t, "/prefs/theme", "operator", `"auto"`)
	if _, page := e.do(t, "GET", "/devices", "operator", nil); !strings.Contains(page, `<html lang="en" data-theme-auto>`) {
		t.Errorf("Auto does not follow the system: %.120s", page)
	}
}

// Why: the phone menu button is a link to /nav; without JavaScript that page is the menu, so
// it must list what the drawer lists for the role.
func TestNavPageListsTheMenu(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/nav", "operator", nil)
	main := body[strings.Index(body, `<main id="main">`):]
	for _, want := range []string{">Overview<", ">List<", ">Sign out<", `aria-label="Theme"`} {
		if !strings.Contains(main, want) {
			t.Errorf("/nav lacks %q", want)
		}
	}
	if !strings.Contains(main, ">Styleguide<") {
		t.Error("/nav does not offer an operator the System pages the drawer lists")
	}
}

// productName finds the product name written in lower case as a word of text: not inside a URL,
// path, file name, identifier or environment variable.
var productName = regexp.MustCompile(`(^|[^/\w.\-="])tasmofleet([^\w/\-.]|\.(\s|$|<))`)

// Why: D15: the product is written "TasmoFleet" in every UI text; only identifiers (repository,
// binary, module, URLs, TASMOFLEET_* variables) stay lower case. The check reads the sources the
// UI is made of, so a new page cannot bring the old spelling back.
func TestUITextSaysTasmoFleet(t *testing.T) {
	files, err := filepath.Glob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	more, _ := filepath.Glob("*.templ")
	files = append(files, more...)
	more, _ = filepath.Glob("ui/*.templ")
	files = append(files, append(more, "pages.go")...)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := productName.FindString(line); m != "" {
				t.Errorf("%s:%d: lower-case product name in UI text: %q", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if !productName.MatchString("data from tasmofleet. The") || productName.MatchString("TASMOFLEET_PUBLIC_URL /tasmofleet tasmofleet.db github.com/nofuturekid/tasmofleet") {
		t.Fatal("the check itself is wrong")
	}
}

// Why (#124): without JavaScript the phone's menu is /nav; its content is built before the page
// shell is, so it needs the version of its own.
func TestNavPageShowsTheVersion(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.Version = "9.9.9+dev" })
	_, body := e.do(t, "GET", "/nav", "operator", nil)
	main := body[strings.Index(body, `<main id="main">`):]
	if !strings.Contains(main, `class="ver"`) || !strings.Contains(main, ">9.9.9+dev</a>") {
		t.Error("the no-JS menu page has no version")
	}
}
