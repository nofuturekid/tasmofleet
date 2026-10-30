package tasmota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

func bootSwitcher(t *testing.T, o Options) BootSwitcher {
	t.Helper()
	b, ok := newClient(o).(BootSwitcher)
	if !ok {
		t.Fatal("the production client must implement BootSwitcher")
	}
	return b
}

// The dual-partition page as Tasmota v16.0.0 writes HTTP_FORM_RST_UPG
// (xdrv_01_9_webserver.ino:367): the button submits the form itself.
const dualUpPage = `<form method='post' action='u2?fsz=' enctype='multipart/form-data'><br><input type='file' name='u2'><br>` +
	`<br><button type='submit' onclick='eb("f1").style.display="none";eb("f2").style.display="block";` +
	`this.form.action+=this.form["u2"].files[0].size;this.form.submit();'>Start upgrade</button></form>`

// Why: the push into safeboot starts a restart, so the layout test may only
// say "safeboot" for the page that really calls upl(this). The captured page
// of a real single-OTA ESP32 (office-monitor-guest) is the positive case, the
// dual page as the v16.0.0 source writes it the negative one, and a page with
// no upload form must not be guessed at.
func TestParseUpLayout(t *testing.T) {
	real, err := os.ReadFile(filepath.Join(fixturesDir, "office-monitor-guest", "up.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		page string
		want UpLayout
		err  error
	}{
		{"captured single-OTA page", string(real), UpLayoutSafeboot, nil},
		{"dual-partition page", dualUpPage, UpLayoutDirect, nil},
		// The script defines upl(t) on every ESP32 page; only a call upl(this) counts.
		{"dual page with the script that defines upl", "<script>function upl(t){return false;}</script>" + dualUpPage, UpLayoutDirect, nil},
		{"no upload form", "<html>Firmware Upgrade</html>", "", ErrUploadUnsupported},
		// Review #297: anything that is not exactly one of the two forms proves nothing.
		{"both buttons", dualUpPage + strings.Replace(dualUpPage, "this.form.submit();", "return upl(this);", 1), "", ErrUpLayoutAmbiguous},
		{"two plain forms", dualUpPage + dualUpPage, "", ErrUpLayoutAmbiguous},
		{"plain form that also calls upl", strings.Replace(dualUpPage, "this.form.submit();", "this.form.submit();upl(this);", 1), "", ErrUpLayoutAmbiguous},
		{"safeboot call and plain submit", strings.Replace(dualUpPage, "this.form.submit();", "this.form.submit();return upl(this);", 1), "", ErrUpLayoutAmbiguous},
		{"form without either marker", strings.Replace(dualUpPage, "this.form.submit();", "go();", 1), "", ErrUpLayoutAmbiguous},
		{"plain marker twice", strings.Replace(dualUpPage, "this.form.submit();", "this.form.submit();this.form.submit();", 1), "", ErrUpLayoutAmbiguous},
		{"upl call without return", strings.Replace(dualUpPage, "this.form.submit();", "upl(this);", 1), "", ErrUpLayoutAmbiguous},
		{"marker only in a comment, no form", "<!-- this.form.submit(); -->", "", ErrUploadUnsupported},
		{"truncated page", dualUpPage[:len(dualUpPage)-40], "", ErrUpLayoutAmbiguous},
		{"upper-case marker", strings.Replace(dualUpPage, "this.form.submit();", "THIS.FORM.SUBMIT();", 1), "", ErrUpLayoutAmbiguous},
		{"empty", "", "", ErrUploadUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseUpLayout([]byte(tc.page))
			if got != tc.want || !errors.Is(err, tc.err) {
				t.Fatalf("got %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

// Why: reading the layout must be a plain GET of /up and nothing else; the
// simulator profiles stand for the two real layouts.
func TestReadUpLayoutAgainstTheSimulator(t *testing.T) {
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for _, tc := range []struct {
		profile simulator.Profile
		want    UpLayout
	}{
		{simulator.ProfileESP32, UpLayoutSafeboot},
		{simulator.ProfileESP32Dual, UpLayoutDirect},
	} {
		dev, tg := uploadSimIn(t, f, string(tc.profile), tc.profile)
		got, err := bootSwitcher(t, Options{}).ReadUpLayout(context.Background(), tg)
		if err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.profile, got, err, tc.want)
		}
		if n := len(dev.Commands()); n != 0 || dev.Boots() != 0 {
			t.Errorf("%s: reading the layout reached the device: %v", tc.profile, dev.Commands())
		}
	}
}

// Why: /u4 restarts the device; its reply tells whether it does. The client
// must pass the three answers on exactly, sending the request once.
func TestSwitchBootReplies(t *testing.T) {
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	c, tg := uploadSimIn(t, f, "single", simulator.ProfileESP32)
	sw := bootSwitcher(t, Options{})
	ctx := context.Background()
	if r, err := sw.SwitchBoot(ctx, tg, BootApp); err != nil || r != BootAlready {
		t.Fatalf("ota in the app: %q, %v", r, err)
	}
	if r, err := sw.SwitchBoot(ctx, tg, BootSafeboot); err != nil || r != BootRestarting {
		t.Fatalf("fct in the app: %q, %v", r, err)
	}
	waitBoots(t, c, 1)
	if c.Image() != "safeboot" {
		t.Fatalf("image %s", c.Image())
	}
	if r, err := sw.SwitchBoot(ctx, tg, BootSafeboot); err != nil || r != BootAlready {
		t.Fatalf("fct in safeboot: %q, %v", r, err)
	}
	if c.Boots() != 1 {
		t.Errorf("fct in safeboot restarted the device (%d boots)", c.Boots())
	}
	_, dtg := uploadSimIn(t, f, "dual", simulator.ProfileESP32Dual)
	if r, err := sw.SwitchBoot(ctx, dtg, BootSafeboot); err != nil || r != BootNone {
		t.Fatalf("fct on a dual layout: %q, %v", r, err)
	}
	if _, err := sw.SwitchBoot(ctx, tg, "bogus"); err == nil {
		t.Error("an unknown target must not reach the device")
	}
}

// Why: an error status or an unknown answer must not be taken for "restarting",
// and only the status case may claim that nothing happened: a 200 with a body
// the client does not know leaves it open whether the device acted.
func TestSwitchBootCertainAndUncertainErrors(t *testing.T) {
	var hits atomic.Int32
	status, body := http.StatusOK, "false"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/u4" || r.URL.Query().Get("u4") != "fct" || !r.URL.Query().Has("api") {
			t.Errorf("unexpected request %s", r.URL)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	sw := bootSwitcher(t, Options{})
	tg := target(t, srv, 1, "", "")

	status = http.StatusNotFound // a firmware without /u4
	if _, err := sw.SwitchBoot(context.Background(), tg, BootSafeboot); !errors.Is(err, ErrSwitchNotDone) {
		t.Errorf("404: %v, want ErrSwitchNotDone", err)
	}
	status = http.StatusUnauthorized
	if _, err := sw.SwitchBoot(context.Background(), tg, BootSafeboot); !errors.Is(err, ErrAuth) {
		t.Errorf("401: %v, want ErrAuth", err)
	}
	status, body = http.StatusOK, "maybe"
	_, err := sw.SwitchBoot(context.Background(), tg, BootSafeboot)
	if err == nil || errors.Is(err, ErrSwitchNotDone) {
		t.Errorf("unknown answer: %v, must be an uncertain error", err)
	}
	if hits.Load() != 3 {
		t.Errorf("%d requests for 3 calls", hits.Load())
	}
}

// Why: a reply lost after the device restarted must not read as "nothing
// happened": the error is a transport error, and the caller probes.
func TestSwitchBootLostReplyIsUncertain(t *testing.T) {
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	d, err := f.Add(context.Background(), simulator.DeviceSpec{Name: "lost", Profile: simulator.ProfileESP32,
		Faults: []simulator.Fault{{Kind: simulator.FaultSwitchLostReply}}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	tg := simTarget(t, d)
	_, err = bootSwitcher(t, Options{}).SwitchBoot(context.Background(), tg, BootSafeboot)
	if err == nil || errors.Is(err, ErrSwitchNotDone) || errors.Is(err, ErrAuth) {
		t.Fatalf("got %v, want an uncertain transport error", err)
	}
	waitBoots(t, c, 1)
	if c.Image() != "safeboot" {
		t.Errorf("the device did restart into safeboot regardless, image %s", c.Image())
	}
}

// Why: the SSRF rule holds for the switch and the layout read: an address
// outside the device networks is never contacted.
func TestSafebootCallsHonourTheDeviceNetworks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	sw := NewHTTPClient(Options{AllowedNetworks: nil}).(BootSwitcher)
	tg := target(t, srv, 1, "", "")
	if _, err := sw.SwitchBoot(context.Background(), tg, BootSafeboot); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("SwitchBoot: %v", err)
	}
	if _, err := sw.ReadUpLayout(context.Background(), tg); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("ReadUpLayout: %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached a forbidden address", hits.Load())
	}
}

// Why: the device's web password is sent as Basic auth to the bound target only.
func TestSwitchBootSendsCredentials(t *testing.T) {
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		_, _ = w.Write([]byte("true"))
	}))
	defer srv.Close()
	if _, err := bootSwitcher(t, Options{Timeout: time.Second}).SwitchBoot(context.Background(), target(t, srv, 1, "", testPassword), BootApp); err != nil {
		t.Fatal(err)
	}
	if user != "admin" || pass != testPassword || strings.Contains(user+pass, "wrong") {
		t.Errorf("credentials %q/%q", user, pass)
	}
}

// Why (review #297, M1): a single-OTA ESP32 that RUNS safeboot serves the plain upload form too, so the form
// proves nothing about two app partitions. Only a page whose footer names a normal image, without the
// SAFEBOOT banner, is a proof. The safeboot page is built from the captured single-OTA page the way the
// v16 source writes it (plain form, SAFEBOOT banner, footer image release-safeboot).
func TestUpPageProvesDualOTAOnlyForTheNormalFirmware(t *testing.T) {
	real, err := os.ReadFile(filepath.Join(fixturesDir, "office-monitor-guest", "up.html"))
	if err != nil {
		t.Fatal(err)
	}
	normal := strings.Replace(string(real), "return upl(this);", "this.form.submit();", 1) // dual layout: plain form, normal firmware
	if !strings.Contains(normal, "(release-tasmota32) by Theo Arends") || strings.Contains(normal, safebootBanner) {
		t.Fatal("the fixture does not look as assumed")
	}
	safeboot := strings.Replace(strings.Replace(normal, "(release-tasmota32)", "(release-safeboot)", 1), "<body>", "<body><span><h3>SAFEBOOT</h3></span>", 1)
	for name, tc := range map[string]struct {
		page    string
		layout  UpLayout
		image   string
		banner  bool
		proves  bool
		wantErr bool
	}{
		"normal firmware on two partitions": {normal, UpLayoutDirect, "release-tasmota32", false, true, false},
		"captured single-OTA page":          {string(real), UpLayoutSafeboot, "release-tasmota32", false, false, false},
		"safeboot serving the plain form":   {safeboot, UpLayoutDirect, "release-safeboot", true, false, false},
		"safeboot image without a banner":   {strings.Replace(normal, "(release-tasmota32)", "(release-safeboot)", 1), UpLayoutDirect, "release-safeboot", false, false, false},
		"banner with a normal footer":       {strings.Replace(normal, "<body>", "<body><h3>SAFEBOOT</h3>", 1), UpLayoutDirect, "release-tasmota32", true, false, false},
		"minimal image":                     {strings.Replace(normal, "(release-tasmota32)", "(release-minimal)", 1), UpLayoutDirect, "release-minimal", false, false, false},
		"footer missing":                    {strings.Replace(normal, "Tasmota 16.0.0 (release-tasmota32) by Theo Arends", "", 1), UpLayoutDirect, "", false, false, false},
		"two footers":                       {normal + normal[strings.Index(normal, "<p></p><div style='text-align:right"):], UpLayoutDirect, "", false, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := ParseUpPage([]byte(tc.page))
			if (err != nil) != tc.wantErr || p.Layout != tc.layout || p.Image != tc.image || p.SafebootBanner != tc.banner || p.ProvesDualOTA() != tc.proves {
				t.Fatalf("%+v, %v; want layout %q image %q banner %v proves %v", p, err, tc.layout, tc.image, tc.banner, tc.proves)
			}
		})
	}
}
