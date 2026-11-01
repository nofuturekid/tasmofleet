package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/backup"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Why: e2e starts devenv per run; it must come up with the whole mixed fleet
// and stop within 2 s on SIGTERM, or CI hangs.
func TestDevenvStartsFleetAndStopsQuickly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02"}, pw, io.Discard)
		_ = pw.Close()
	}()
	var lines []string
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	text := strings.Join(lines, "\n")
	for _, want := range []string{"TASMOFLEET_MQTT_URL=tcp://127.0.0.1:", "DEVENV_UPSTREAM_URL=http://127.0.0.1:", "name=lamp-1m", "name=mystery-chip", "name=esp32s3-http", "http-only"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	// the fake upstream serves a real-looking image by file name
	var up string
	for _, l := range lines {
		if strings.HasPrefix(l, "DEVENV_UPSTREAM_URL=") {
			up = strings.TrimPrefix(l, "DEVENV_UPSTREAM_URL=")
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, up+"/tasmota32/release/tasmota32c3.bin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(b) < 32 || b[0] != 0xE9 {
		t.Errorf("upstream image: %d len %d", resp.StatusCode, len(b))
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("devenv did not stop")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("shutdown took %v", d)
	}
}

func TestDevenvHelp(t *testing.T) {
	var errOut bytes.Buffer
	if err := run(context.Background(), []string{"--help"}, io.Discard, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "time-scale") {
		t.Error(errOut.String())
	}
}

// Why: the OIDC e2e needs a mock IdP whose next login can be chosen per test
// (operator, admin, user without groups); without the control endpoint every
// test would log in as the same user and the role checks could not be e2e-tested.
func TestDevenvMockIdPControl(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02", "-oidc-redirect", "http://127.0.0.1:1/oidc/callback"}, pw, io.Discard)
		_ = pw.Close()
	}()
	vals := map[string]string{}
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			vals[k] = v
		}
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	if !strings.HasSuffix(vals["DEVENV_OIDC_ISSUER"], "/application/o/tasmofleet/") || vals["DEVENV_CONTROL_URL"] == "" {
		t.Fatalf("missing IdP output: %v", vals)
	}
	for who, want := range map[string]int{"admin": 204, "operator": 204, "stranger": 404} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, vals["DEVENV_CONTROL_URL"]+"/login-as/"+who, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("login-as %s: %d, want %d", who, resp.StatusCode, want)
		}
	}
}

// Why: e2e has no decode-config. The shim must let the real ExecDecoder
// decode a dump of a simulated device after a setting was changed: this is
// the chain "change TelePeriod, back up, see the drift" runs on.
func TestDecoderShimWithRealExecDecoder(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "devenv")
	if out, err := exec.CommandContext(context.Background(), "go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-fixtures", "../../testdata/fixtures", "-time-scale", "0.02")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	var shim, addr string
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		l := sc.Text()
		if v, ok := strings.CutPrefix(l, "DEVENV_DECODER_PATH="); ok {
			shim = v
		}
		if strings.HasPrefix(l, "DEVENV_DEVICE name=plug-4m ") {
			addr = strings.Fields(l)[2][len("addr="):]
		}
		if l == "DEVENV_READY=1" {
			break
		}
	}
	if shim == "" || addr == "" {
		t.Fatalf("shim %q addr %q", shim, addr)
	}
	get := func(path string) []byte {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	get("/cm?cmnd=TelePeriod%20120")
	dec := backup.NewExecDecoder(shim, t.TempDir())
	if v, err := dec.Version(ctx); err != nil || v != "16.0.0.0" {
		t.Fatalf("version %q %v", v, err)
	}
	out, err := dec.Decode(ctx, get("/dl"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil || doc["tele_period"] != float64(120) {
		t.Errorf("decoded dump should carry the changed TelePeriod: %v %v", doc["tele_period"], err)
	}
	if _, err := dec.Decode(ctx, make([]byte, 4096)); err == nil {
		t.Error("foreign dumps must be refused")
	}
}

// Why: the pixel comparison against the prototype (make visual) is only
// meaningful when the app shows the prototype's fleet. The fixture devenv must
// bring up all 60 devices with the prototype's names (spaces included), serve
// the prototype's release list, and give the seed a way to turn the "offline"
// devices off after the app has seen them online ("Offline" needs a previous
// contact, a device never reached is "Unknown").
func TestDevenvDesignFixture(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02", "-design-fixture", "../../testdata/design/v2-fixture.json"}, pw, io.Discard)
		_ = pw.Close()
	}()
	vals := map[string]string{}
	devices := map[string]string{} // unescaped name -> line
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "DEVENV_DEVICE name="); ok {
			name, _, _ := strings.Cut(rest, " ")
			un, err := url.PathUnescape(name)
			if err != nil {
				t.Fatal(err)
			}
			devices[un] = line
		} else if k, v, ok := strings.Cut(line, "="); ok {
			vals[k] = v
		}
		if line == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	if len(devices) != 60 {
		t.Fatalf("%d devices, want the prototype's 60", len(devices))
	}
	if !strings.Contains(devices["Kitchen Plug"], "version=14.3.0") || !strings.Contains(devices["Kitchen Plug"], " mqtt") {
		t.Errorf("Kitchen Plug: %s", devices["Kitchen Plug"])
	}
	if !strings.HasSuffix(devices["Bedroom Lamp"], "version=14.2.0 mqtt") {
		t.Errorf("a device on an older firmware keeps it: %s", devices["Bedroom Lamp"])
	}
	// d31 (unknown) is an inventory entry on a port nothing listens on.
	var unknown string
	for name, l := range devices {
		if strings.HasSuffix(l, "unreachable") {
			unknown = name
			addr := regexp.MustCompile(`addr=(\S+)`).FindStringSubmatch(l)[1]
			// a topic keeps the app from polling it over HTTP and calling it Offline after three failures
			if strings.Contains(l, " topic= ") {
				t.Errorf("unknown device %s has no topic: %s", name, l)
			}
			if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
				_ = c.Close()
				t.Errorf("%s (unknown) answers on %s", name, addr)
			}
		}
	}
	if unknown == "" {
		t.Error("no unreachable device: the fixture has two with status unknown")
	}
	// the fake upstream offers the prototype's releases, newest first
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, vals["DEVENV_UPSTREAM_GITHUB_URL"]+"/releases", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var rel []struct {
		Tag       string `json:"tag_name"`
		Body      string `json:"body"`
		Published string `json:"published_at"`
	}
	err = json.NewDecoder(resp.Body).Decode(&rel)
	_ = resp.Body.Close()
	if err != nil || len(rel) != 11 || rel[0].Tag != "v14.3.0" || rel[1].Tag != "v14.2.0" || rel[10].Tag != "v12.5.0" {
		t.Fatalf("releases %v (%v)", rel, err)
	}
	// The firmware page shows the newest releases' dates and notes: they are the prototype's, so
	// the pixel comparison of the page compares the same text.
	if !strings.HasPrefix(rel[0].Published, "2026-10-06") || !strings.Contains(rel[0].Body, "- Wi-Fi reconnects more reliably after an access point restart\n") {
		t.Errorf("14.3.0: published %s, body %q", rel[0].Published, rel[0].Body)
	}
	// the feed can be made to fail and recover (the page's "not reachable" state)
	feed := func(state string) int {
		post, _ := http.NewRequestWithContext(ctx, http.MethodPost, vals["DEVENV_CONTROL_URL"]+"/fixture/feed/"+state, nil)
		r, err := http.DefaultClient.Do(post)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusNoContent {
			t.Fatalf("feed %s: %d", state, r.StatusCode)
		}
		get, _ := http.NewRequestWithContext(ctx, http.MethodGet, vals["DEVENV_UPSTREAM_GITHUB_URL"]+"/releases", nil)
		r, err = http.DefaultClient.Do(get)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if got := feed("down"); got != http.StatusServiceUnavailable {
		t.Errorf("feed down: GET releases = %d", got)
	}
	if got := feed("up"); got != http.StatusOK {
		t.Errorf("feed up: GET releases = %d", got)
	}
	// downloads fail on their own switch, the release list keeps working
	dl := func() int {
		get, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(vals["DEVENV_UPSTREAM_GITHUB_URL"], "/github")+"/github/dl/14.3.0/tasmota32c3.bin", nil)
		r, err := http.DefaultClient.Do(get)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	if got := feed("dl-down"); got != http.StatusOK || dl() != http.StatusServiceUnavailable {
		t.Errorf("dl-down: releases %d, download %d", got, dl())
	}
	if feed("up"); dl() != http.StatusOK {
		t.Error("downloads did not come back with up")
	}
	// an offline device answers until the control endpoint takes it away
	var off string
	for name, l := range devices {
		if name == "Hallway Dimmer" {
			off = regexp.MustCompile(`addr=(\S+)`).FindStringSubmatch(l)[1]
		}
	}
	if c, err := net.DialTimeout("tcp", off, time.Second); err != nil {
		t.Fatalf("Hallway Dimmer (offline in the prototype) must start online: %v", err)
	} else {
		_ = c.Close()
	}
	post, _ := http.NewRequestWithContext(ctx, http.MethodPost, vals["DEVENV_CONTROL_URL"]+"/fixture/offline", nil)
	r2, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	_ = r2.Body.Close()
	if r2.StatusCode != http.StatusNoContent {
		t.Fatalf("offline control: %d", r2.StatusCode)
	}
	if c, err := net.DialTimeout("tcp", off, time.Second); err == nil {
		_ = c.Close()
		t.Error("Hallway Dimmer still answers after /fixture/offline")
	}
}

// Why: an "unknown" device must never answer. A port taken from the ephemeral
// range and released again can be handed to one of the simulators started right
// after it (issue #39), so the address has to come from the privileged range,
// which no unprivileged process (the simulators included) can bind, and each
// unknown device needs its own.
func TestClosedLoopbackAddrIsPrivilegedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		addr := closedLoopbackAddr(i)
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatalf("closedLoopbackAddr(%d) = %q: %v", i, addr, err)
		}
		if host != "127.0.0.1" {
			t.Errorf("closedLoopbackAddr(%d) = %q, want a loopback address", i, addr)
		}
		var p int
		if _, err := fmt.Sscan(port, &p); err != nil || p < 1 || p >= 1024 {
			t.Errorf("closedLoopbackAddr(%d) = %q, want a privileged port (1-1023)", i, addr)
		}
		if seen[addr] {
			t.Errorf("closedLoopbackAddr(%d) = %q repeats an earlier address", i, addr)
		}
		seen[addr] = true
	}
}

// Why: simulated devices only download from loopback (the OTA download gadget must not reach the
// network). A devenv that serves devices on a non-loopback address, for example next to a Home
// Assistant app in a devcontainer, needs the app's firmware server host on the allow list, or every
// update ends in "device did not start the upgrade" without a request ever arriving (#442).
func TestSimulatorExtraCarriesTheAllowedDownloadHosts(t *testing.T) {
	x := simulatorExtra(0.1, "172.30.32.1", "16.1.0", multiFlag{"172.30.32.1", "fw.lan"})
	if got := strings.Join(x.AllowDownloadHosts, ","); got != "172.30.32.1,fw.lan" {
		t.Errorf("allowed download hosts %q", got)
	}
	if x.BindAddr != "172.30.32.1" || x.TimeScale != 0.1 || x.FlashVersion != "16.1.0" {
		t.Errorf("other options lost: %+v", x)
	}
	if none := simulatorExtra(1, "127.0.0.1", "16.1.0", nil); len(none.AllowDownloadHosts) != 0 {
		t.Errorf("without the flag the gadget stays loopback-only, got %v", none.AllowDownloadHosts)
	}
}

func TestDevenvHelpListsAllowDownloadHost(t *testing.T) {
	var buf bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, io.Discard, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "-allow-download-host") {
		t.Errorf("help does not list -allow-download-host:\n%s", buf.String())
	}
}

// Why (#327): the GPIO templates tab needs a fleet that shows every state of it: a group with a majority
// and one device that differs in a single pin, a tie, a built-in module, an ESP32 group, a device that can
// be taken offline. The devices must answer Template and Module the way the specs say.
func TestDevenvTemplateShapes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	go func() {
		_ = run(ctx, []string{"-fixtures", "../../testdata/fixtures", "-time-scale", "0.02", "-template-shapes"}, pw, io.Discard)
		_ = pw.Close()
	}()
	addr := map[string]string{}
	re := regexp.MustCompile(`DEVENV_DEVICE name=(\S+) addr=(\S+)`)
	sc := bufio.NewScanner(pr)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			addr[m[1]] = m[2]
		}
		if sc.Text() == "DEVENV_READY=1" {
			break
		}
	}
	go io.Copy(io.Discard, pr) //nolint:errcheck // drain
	ask := func(name, cmnd string) string {
		t.Helper()
		a, ok := addr[name]
		if !ok {
			t.Fatalf("no device %s in %v", name, addr)
		}
		resp, err := http.Get("http://" + a + "/cm?cmnd=" + url.QueryEscape(cmnd))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	gpio := func(name string) []int {
		var v struct{ GPIO []int }
		if err := json.Unmarshal([]byte(ask(name, "Template")), &v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return v.GPIO
	}
	// the NOUS group: three alike, one with Led_i1 (320) instead of Relay1 (224) on GPIO14 (slot 10)
	for _, n := range []string{"tpl-nous-1", "tpl-nous-2", "tpl-nous-3"} {
		if g := gpio(n); len(g) != 14 || g[10] != 224 {
			t.Errorf("%s: %v", n, g)
		}
	}
	if g := gpio("tpl-nous-dev"); g[10] != 320 {
		t.Errorf("tpl-nous-dev: %v", g)
	}
	// the tie: 2 + 2
	a, b := gpio("tpl-tie-1"), gpio("tpl-tie-2")
	c, d := gpio("tpl-tie-3"), gpio("tpl-tie-4")
	if fmt.Sprint(a) != fmt.Sprint(b) || fmt.Sprint(c) != fmt.Sprint(d) || fmt.Sprint(a) == fmt.Sprint(c) {
		t.Errorf("tie group: %v %v %v %v", a, b, c, d)
	}
	if got := ask("tpl-builtin", "Module"); !strings.Contains(got, `"1"`) {
		t.Errorf("tpl-builtin Module: %s", got)
	}
	if g := gpio("tpl-esp32-1"); len(g) != 36 {
		t.Errorf("esp32 template has %d slots", len(g))
	}
	for _, n := range []string{"tpl-offline", "tpl-minimal"} {
		if _, ok := addr[n]; !ok {
			t.Errorf("missing %s", n)
		}
	}
}
