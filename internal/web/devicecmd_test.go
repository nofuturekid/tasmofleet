package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// fakeCmd stands in for devicecmd.Service. Run applies the real cmdpolicy so
// the web tests see the same refusals the service gives.
type fakeCmd struct {
	mu       sync.Mutex
	powers   []string
	runs     []string
	state    string
	powerErr error
	reply    string
	runErr   error
	consent  []bool // per Run: the call carried the cut-off consent
}

func (f *fakeCmd) Power(_ context.Context, actor string, id model.DeviceID, relay int, action string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.powers = append(f.powers, strings.Join([]string{actor, id64(id), itoa(relay), action}, "|"))
	if f.powerErr != nil {
		return "", f.powerErr
	}
	return f.state, nil
}

func (f *fakeCmd) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := cmdpolicy.Authorize(cmd, p.Role); err != nil {
		return nil, err
	}
	f.runs = append(f.runs, p.Subject+"|"+id64(id)+"|"+cmd)
	f.consent = append(f.consent, transport.HasCutOffConsent(ctx))
	if f.runErr != nil {
		return nil, f.runErr
	}
	return json.RawMessage(f.reply), nil
}

func (f *fakeCmd) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

func id64(id model.DeviceID) string { return itoa(int(id)) }

func cmdEnv(t *testing.T, f *fakeCmd) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) { d.DeviceCmd = f })
}

// Why: operators switch relays from the list; the answer is the new state in
// the same cell, and the actor is the signed-in user, never form data.
func TestPowerToggleReturnsCell(t *testing.T) {
	f := &fakeCmd{state: "ON"}
	e := cmdEnv(t, f)
	resp, body := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"action": {"TOGGLE"}, "actor": {"mallory"}}, "HX-Request", "true")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(f.powers) != 1 || f.powers[0] != "test:operator|1|1|TOGGLE" {
		t.Fatalf("service saw %v", f.powers)
	}
	if !strings.Contains(body, "ON") || !strings.Contains(body, `hx-post="/tf/devices/1/power/1"`) {
		t.Fatalf("cell: %s", body)
	}
}

func TestPowerWithoutHTMXRedirectsBack(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{state: "OFF"})
	resp, _ := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"action": {"OFF"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/tf/devices" {
		t.Fatalf("status %d location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestPowerRoutesAndErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		role, path string
		err        error
		want       int
		text       string
	}{
		"anonymous":          {"", "/devices/1/power/1", nil, 401, ""},
		"relay not a number": {"operator", "/devices/1/power/x", nil, 404, ""},
		"relay zero":         {"operator", "/devices/1/power/0", devicecmd.ErrNoRelay, 422, ""},
		"unknown device":     {"operator", "/devices/99/power/1", nil, 404, ""},
		"no such relay":      {"operator", "/devices/1/power/3", devicecmd.ErrNoRelay, 422, "no such relay"},
		"bad action":         {"operator", "/devices/1/power/1", devicecmd.ErrBadAction, 400, "ON, OFF or TOGGLE"},
		"unconfirmed":        {"operator", "/devices/1/power/1", devicecmd.ErrUnconfirmed, 504, "did not confirm"},
		"device down":        {"operator", "/devices/1/power/1", tasmota.ErrUnreachable, 502, "unreachable"},
	} {
		t.Run(name, func(t *testing.T) {
			e := cmdEnv(t, &fakeCmd{powerErr: tc.err, state: "ON"})
			resp, body := e.do(t, "POST", tc.path, tc.role, url.Values{"action": {"ON"}}, "HX-Request", "true")
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.text != "" && !strings.Contains(body, tc.text) {
				t.Fatalf("want %q in %s", tc.text, body)
			}
		})
	}
}

// Why: a device with a web password must never have it shown, even when the
// power error text comes from the device client.
func TestPowerErrorShowsNoCredentials(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{powerErr: tasmota.ErrAuth})
	_, body := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"action": {"ON"}}, "HX-Request", "true")
	if strings.Contains(body, "s3cret-pw") {
		t.Fatalf("credential leaked: %s", body)
	}
}

// Why: without the service (feature off) there is no route and no button.
func TestPowerAbsentWithoutService(t *testing.T) {
	e := setup(t)
	resp, _ := e.do(t, "POST", "/devices/1/power/1", "operator", url.Values{"action": {"ON"}})
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(body, "/power/") {
		t.Fatalf("buttons without a service: %s", body)
	}
}

// Why: the list shows each relay with its live state and a button per relay.
func TestDeviceListShowsPowerButtons(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "ON", "POWER2": "OFF"}}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	for _, want := range []string{`hx-post="/tf/devices/1/power/1"`, `hx-post="/tf/devices/1/power/2"`, `sse-swap="power-1"`, `sse-swap="power-2"`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, `/devices/2/power/`) {
		t.Errorf("a device without known relays gets no button")
	}
}

func TestSinglePowerKeyIsRelayOne(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER": "ON"}}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if !strings.Contains(body, `hx-post="/tf/devices/1/power/1"`) {
		t.Fatalf("POWER must map to relay 1: %s", body)
	}
}

// Why: device-controlled strings (a hostile device reporting a state) must
// not become markup.
func TestPowerStateIsEscaped(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": `<img src=x onerror=alert(1)>`}}
	_, body := e.do(t, "GET", "/devices", "operator", nil)
	if strings.Contains(body, "<img src=x") {
		t.Fatalf("unescaped: %s", body)
	}
}

func TestSSEPublishesPowerFragment(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	body := readSSE(t, e, "power-1", func() {
		e.hub.Publish(events.Event{Topic: events.TopicHealth, Payload: model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "ON"}}})
	})
	if !strings.Contains(body, "event: power-1\ndata: ") || !strings.Contains(body, `/tf/devices/1/power/1`) {
		t.Fatalf("stream: %q", body)
	}
}

// ---- console ----

func consoleForm(cmd string, extra ...string) url.Values {
	v := url.Values{"command": {cmd}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return v
}

func TestConsoleOperatorReadsStatus(t *testing.T) {
	f := &fakeCmd{reply: `{"StatusFWR":{"Version":"16.0.0"}}`}
	e := cmdEnv(t, f)
	resp, body := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("Status 2"))
	if resp.StatusCode != 200 || !strings.Contains(body, "StatusFWR") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := f.ran(); len(got) != 1 || got[0] != "test:operator|1|Status 2" {
		t.Fatalf("ran %v", got)
	}
}

// Why: device output is untrusted text.
func TestConsoleReplyIsEscaped(t *testing.T) {
	f := &fakeCmd{reply: `{"x":"<script>alert(1)</script>"}`}
	e := cmdEnv(t, f)
	_, body := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("Status 2"))
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatalf("unescaped reply: %s", body)
	}
}

// Why: acceptance criterion: an operator gets 403 on admin commands, and the
// service (which audits the refusal) is still asked, so nothing is silent.
func TestConsoleOperatorForbiddenOnAdminCommand(t *testing.T) {
	f := &fakeCmd{reply: `{}`}
	e := cmdEnv(t, f)
	resp, body := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("SetOption19 1", "confirm", "1"))
	if resp.StatusCode != 403 || !strings.Contains(body, "admin role") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(f.ran()) != 0 {
		t.Fatal("must not run")
	}
}

// confirmToken pulls the single-use token out of a confirmation page.
func confirmToken(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no confirmation token in: %s", body)
	}
	return m[1]
}

// Why (audit row 4): a routine admin command is sent at once; the second
// screen is kept for commands whose consequence needs naming, so admins are
// not clicked through a dialog on every setting.
func TestConsoleRoutineAdminCommandNeedsNoConfirmation(t *testing.T) {
	f := &fakeCmd{reply: `{"TelePeriod":120}`}
	e := cmdEnv(t, f)
	resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("TelePeriod 120"))
	if resp.StatusCode != 200 || strings.Contains(body, "Confirm sending") || !strings.Contains(body, "TelePeriod") {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := f.ran(); len(got) != 1 || got[0] != "test:admin|1|TelePeriod 120" {
		t.Fatalf("ran %v", got)
	}
}

// Why (D13, #256): a device that only MQTT reaches cannot be reached again once its topic or broker
// is changed wrongly; the console asks once and says so, and only that confirmation carries the consent the
// router needs to publish it. A device reached over HTTP keeps the no-dialog path, and a command that does
// not touch MQTT is never held back.
func TestConsoleMQTTOnlyDeviceAsksBeforeACutOffChange(t *testing.T) {
	f := &fakeCmd{reply: `{"Topic":"x"}`}
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd = f
		d.MQTTOnly = func(context.Context, ...model.DeviceID) bool { return true }
	})
	resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Topic other"))
	if resp.StatusCode != 200 || !strings.Contains(body, "reached over MQTT only") || len(f.ran()) != 0 {
		t.Fatalf("status %d, ran %v: %s", resp.StatusCode, f.ran(), body)
	}
	resp, body = e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
	if resp.StatusCode != 200 || len(f.ran()) != 1 || !f.consent[0] {
		t.Fatalf("confirmed send: status %d, ran %v, consent %v: %s", resp.StatusCode, f.ran(), f.consent, body)
	}
	if _, body = e.do(t, "POST", "/devices/1/console", "admin", consoleForm("TelePeriod 120")); strings.Contains(body, "reached over MQTT only") {
		t.Errorf("TelePeriod cannot cut the device off: %s", body)
	}
	if len(f.consent) != 2 || f.consent[1] {
		t.Errorf("a command that needs no confirmation of this kind carried the consent: %v", f.consent)
	}
	// a credential command asks anyway (its own consequence); the cut-off consent is not given unless the
	// MQTT-only consequence was shown
	g := &fakeCmd{reply: `{}`}
	e2 := setupWith(t, func(d *Deps) {
		d.DeviceCmd = g
		d.MQTTOnly = func(context.Context, ...model.DeviceID) bool { return false }
	})
	_, body = e2.do(t, "POST", "/devices/1/console", "admin", consoleForm("MqttPassword secret"))
	e2.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
	if len(g.consent) != 1 || g.consent[0] {
		t.Errorf("consent without the MQTT-only consequence: %v", g.consent)
	}
	h := &fakeCmd{reply: `{"Topic":"x"}`}
	e3 := cmdEnv(t, h)
	if _, body := e3.do(t, "POST", "/devices/1/console", "admin", consoleForm("Topic other")); strings.Contains(body, "Confirm sending") || len(h.ran()) != 1 {
		t.Errorf("a device reached over HTTP is sent the change at once: ran %v", h.ran())
	}
}

// Why (audit rows 6-9): formerly refused commands are sent for an admin, and
// the destructive ones ask exactly once, naming the consequence. Nothing is
// sent before the confirmation and the confirmation needs no retyping.
func TestConsoleDestructiveCommandAsksOnceNamingTheConsequence(t *testing.T) {
	for _, cmd := range []string{"Reset 1", "WifiConfig 2", "Wifi 0", "WebServer 0", "Upgrade 1", "OtaUrl http://x/y.bin", "Module 18", "IPAddress1 10.0.0.9", "Restart 2", "Backlog Power ON; Reset 1"} {
		f := &fakeCmd{reply: `{"Command":"done"}`}
		e := cmdEnv(t, f)
		resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(cmd))
		if resp.StatusCode != 200 || !strings.Contains(body, "Confirm sending") || !strings.Contains(body, `class="ucard devconfirm"`) {
			t.Fatalf("%q: status %d: %s", cmd, resp.StatusCode, body)
		}
		if len(f.ran()) != 0 {
			t.Fatalf("%q: nothing may be sent before the confirmation", cmd)
		}
		if strings.Contains(body, `type="password"`) {
			t.Errorf("%q: the confirmation must not ask to retype the command", cmd)
		}
		resp, body = e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
		if resp.StatusCode != 200 || strings.Contains(body, "Confirm sending") {
			t.Fatalf("%q: confirmed send: status %d: %s", cmd, resp.StatusCode, body)
		}
		if got := f.ran(); len(got) != 1 || got[0] != "test:admin|1|"+cmd {
			t.Fatalf("%q: ran %v", cmd, got)
		}
	}
}

// Why: the confirmation is not an authority. A token is single-use, bound to
// the session that asked and to the device it was issued for.
func TestConsoleConfirmationTokenIsSingleUseAndBound(t *testing.T) {
	f := &fakeCmd{reply: `{}`}
	e := cmdEnv(t, f)
	_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Reset 1"))
	token := confirmToken(t, body)
	if resp, _ := e.do(t, "POST", "/devices/2/console", "admin", url.Values{"token": {token}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("another device must not accept the token: %d", resp.StatusCode)
	}
	_, body = e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Reset 1"))
	token = confirmToken(t, body)
	if resp, _ := e.do(t, "POST", "/devices/1/console", "operator", url.Values{"token": {token}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("another principal must not use the token: %d", resp.StatusCode)
	}
	if len(f.ran()) != 0 {
		t.Fatalf("ran %v", f.ran())
	}
	if resp, _ := e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {token}}); resp.StatusCode != 200 {
		t.Fatalf("owner: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {token}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("a token works once: %d", resp.StatusCode)
	}
	if len(f.ran()) != 1 {
		t.Fatalf("ran %v", f.ran())
	}
}

// Why (audit row 1/4): an operator never reaches a destructive command, with or
// without a token: the role check is in the service, not in the screen.
func TestConsoleOperatorCannotSendDestructiveCommands(t *testing.T) {
	f := &fakeCmd{reply: `{}`}
	e := cmdEnv(t, f)
	for _, cmd := range []string{"Reset 1", "Status 0", "Backlog Power ON", "FooBar 1", "Password1 x"} {
		resp, _ := e.do(t, "POST", "/devices/1/console", "operator", consoleForm(cmd, "confirm", "1"))
		if resp.StatusCode != 403 {
			t.Errorf("%q: status %d", cmd, resp.StatusCode)
		}
	}
	if len(f.ran()) != 0 {
		t.Fatalf("ran %v", f.ran())
	}
}

// Why (audit row 10): a manual Status 0 is the admin's call, with the heap
// warning on the confirmation step. Automatic code never sends it (see the
// snapshot and probe tests).
func TestConsoleStatus0IsAllowedWithAWarning(t *testing.T) {
	f := &fakeCmd{reply: `{}`}
	e := cmdEnv(t, f)
	_, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Status 0"))
	if !strings.Contains(body, "very large reply") {
		t.Fatalf("warning missing: %s", body)
	}
	e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
	if got := f.ran(); len(got) != 1 || got[0] != "test:admin|1|Status 0" {
		t.Fatalf("ran %v", got)
	}
}

// Why: confirming does not turn an operator-level command into a two-step
// one, and a forged confirm flag does not lift the role check.
func TestConsoleConfirmFlagDoesNotBypassRole(t *testing.T) {
	f := &fakeCmd{reply: `{}`}
	e := cmdEnv(t, f)
	resp, _ := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("TelePeriod 120", "confirm", "1"))
	if resp.StatusCode != 403 || len(f.ran()) != 0 {
		t.Fatalf("status %d ran %v", resp.StatusCode, f.ran())
	}
}

// Why: P1 stays whatever the role: a line that could chain or hide a command is
// refused before anything is sent, even with a confirmation flag.
func TestConsoleMalformedIsRefused(t *testing.T) {
	for name, cmd := range map[string]string{
		"injection": "Power ON;Reset 1",
		"encoded":   "Power%3BReset",
		"newline":   "Power ON\nReset 1",
		"lookalike": "Backlog Power ON\u037eReset 1",
		"invisible": "Reset\u200b 1",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeCmd{reply: `{}`}
			e := cmdEnv(t, f)
			resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(cmd, "confirm", "1"))
			if resp.StatusCode != 422 {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			if len(f.ran()) != 0 {
				t.Fatal("must not run")
			}
		})
	}
}

// Why (audit row 8): credential commands are the admin's to send, but the
// typed value is shown nowhere: not on the confirmation page, not in the reply
// page, not in any attribute, and the command is held on the server.
func TestConsoleCredentialCommandsAreConfirmedAndRedacted(t *testing.T) {
	for _, c := range []string{"MqttPassword hunter2", "WebPassword hunter2", "Password1 hunter2", "SSId1 hunter2", "Backlog WebPassword hunter2"} {
		f := &fakeCmd{reply: `{}`}
		e := cmdEnv(t, f)
		resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm(c))
		if resp.StatusCode != 200 || !strings.Contains(body, "Confirm sending") || strings.Contains(body, "hunter2") {
			t.Fatalf("%q: confirmation page: %d %s", c, resp.StatusCode, body)
		}
		if len(f.ran()) != 0 {
			t.Fatalf("%q: sent before the confirmation", c)
		}
		resp, body = e.do(t, "POST", "/devices/1/console", "admin", url.Values{"token": {confirmToken(t, body)}})
		if resp.StatusCode != 200 || strings.Contains(body, "hunter2") {
			t.Errorf("%q: result page: %d %s", c, resp.StatusCode, body)
		}
		if got := f.ran(); len(got) != 1 || !strings.HasSuffix(got[0], c) {
			t.Errorf("%q: the device must receive the canonical command, got %v", c, got)
		}
	}
}

func TestConsoleDeviceErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"unreachable": {tasmota.ErrUnreachable, 502},
		"timeout":     {tasmota.ErrTimeout, 504},
		"unknown":     {tasmota.ErrUnknownCommand, 422},
		"auth":        {tasmota.ErrAuth, 502},
	} {
		t.Run(name, func(t *testing.T) {
			e := cmdEnv(t, &fakeCmd{runErr: tc.err})
			resp, body := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("Status 2"))
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d want %d: %s", resp.StatusCode, tc.want, body)
			}
			if strings.Contains(body, "s3cret-pw") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestConsoleRoutes(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	if r, _ := e.do(t, "GET", "/devices/1/console", "", nil); r.StatusCode != 401 {
		t.Errorf("anonymous GET %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/devices/1/console", "", consoleForm("Status 2")); r.StatusCode != 401 {
		t.Errorf("anonymous POST %d", r.StatusCode)
	}
	if r, _ := e.do(t, "GET", "/devices/99/console", "operator", nil); r.StatusCode != 404 {
		t.Errorf("unknown device %d", r.StatusCode)
	}
	if r, body := e.do(t, "GET", "/devices/1/console", "operator", nil); r.StatusCode != 200 || !strings.Contains(body, `name="command"`) {
		t.Errorf("GET %d %s", r.StatusCode, body)
	}
}

// Why: the history comes from the audit log and shows what happened,
// including refusals, with the already-redacted text only.
func TestConsoleHistoryFromAudit(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	at := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	for _, ev := range []model.AuditEvent{
		{At: at, Actor: "oidc:olli", Action: "command.send", Target: "device:1", Detail: `{"command":"Status 2"}`, Result: "ok"},
		{At: at, Actor: "oidc:olli", Action: "command.rejected", Target: "device:1", Detail: `{"command":"Reset 1"}`, Result: "denied"},
		{At: at, Actor: "local:adam", Action: "command.send", Target: "device:1", Detail: `{"command":"MqttPassword ***"}`, Result: "ok"},
		{At: at, Actor: "x", Action: "device.power", Target: "device:1", Detail: `{"relay":1}`, Result: "ok"},
		{At: at, Actor: "x", Action: "command.send", Target: "device:2", Detail: `{"command":"Status 99"}`, Result: "ok"},
		{At: at, Actor: "x", Action: "command.send", Target: "device:1", Detail: `{"command":"<b>x</b>"}`, Result: "ok"},
	} {
		e.aud.Record(t.Context(), ev)
	}
	_, body := e.do(t, "GET", "/devices/1/console", "operator", nil)
	for _, want := range []string{"Status 2", "Reset 1", "MqttPassword ***", "oidc:olli", "&lt;b&gt;x&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("history lacks %q: %s", want, body)
		}
	}
	for _, not := range []string{"Status 99", "device.power", "<b>x</b>"} {
		if strings.Contains(body, not) {
			t.Errorf("history must not contain %q", not)
		}
	}
}

// readSSE opens the event stream, publishes via pub until marker shows up in
// the stream and returns what was read.
func readSSE(t *testing.T, e *env, marker string, pub func()) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/tf/events", nil)
	req.Header.Set("X-Test-Role", "operator")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	go func() {
		for ctx.Err() == nil {
			pub()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, marker) {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, got)
		}
	}
	return got
}

// Why (audit row 6): "admins may do everything": a command nobody listed is sent,
// without a second screen, and the operator is still refused.
func TestConsoleUnknownCommandIsSentForAdmin(t *testing.T) {
	f := &fakeCmd{reply: `{"Sensor13":"ok"}`}
	e := cmdEnv(t, f)
	resp, body := e.do(t, "POST", "/devices/1/console", "admin", consoleForm("Sensor13 1"))
	if resp.StatusCode != 200 || strings.Contains(body, "Confirm sending") || len(f.ran()) != 1 {
		t.Fatalf("status %d ran %v: %s", resp.StatusCode, f.ran(), body)
	}
	if resp, _ := e.do(t, "POST", "/devices/1/console", "operator", consoleForm("Sensor13 1")); resp.StatusCode != 403 {
		t.Fatalf("operator: %d", resp.StatusCode)
	}
}

// Why: switching devices in bulk is risky, so the bar's post only opens the scope page and sends
// nothing, even for one device. The confirmed post switches every relay of the online devices
// through the normal service (the actor is the signed-in user), leaves relays alone that are
// already in the wanted state, and skips the others with a reason; the result page counts them.
func TestBulkPowerAsksFirstThenSwitchesReachableDevices(t *testing.T) {
	f := &fakeCmd{state: "ON"}
	e := cmdEnv(t, f)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "OFF", "POWER2": "ON", "POWER3": "OFF"}}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthUnknown}
	for _, ids := range [][]string{{"1"}, {"1", "2"}} {
		form := url.Values{"csrf": {"tok"}, "action": {"ON"}, "id": ids}
		resp, body := e.do(t, "POST", "/devices/bulk/power", "operator", form)
		if resp.StatusCode != 200 || !strings.Contains(body, "Switch on 1 device") || len(f.powers) != 0 {
			t.Fatalf("%v: must ask first and send nothing: %d %v", ids, resp.StatusCode, f.powers)
		}
	}
	form := url.Values{"csrf": {"tok"}, "action": {"ON"}, "id": {"1", "2"}, "confirm": {"1"}, "relay": {"all"}}
	_, body := e.do(t, "POST", "/devices/bulk/power", "operator", form)
	if len(f.powers) != 2 || !strings.HasPrefix(f.powers[0], "test:operator|1|1|ON") || !strings.HasPrefix(f.powers[1], "test:operator|1|3|ON") {
		t.Fatalf("relays 1 and 3 of device 1 (2 is on already), none of device 2: %v", f.powers)
	}
	for _, want := range []string{"Power on finished", "Completed", "Has never answered. Not attempted.", "was already on", "Power on finished · 1 succeeded"} {
		if !strings.Contains(body, want) {
			t.Errorf("result page lacks %q: %s", want, body)
		}
	}
	// "Relay 1 only (POWER1)"
	f.powers = nil
	form.Set("action", "OFF")
	form.Set("relay", "1")
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "ON", "POWER2": "ON"}}
	e.do(t, "POST", "/devices/bulk/power", "operator", form)
	if len(f.powers) != 1 || !strings.HasPrefix(f.powers[0], "test:operator|1|1|OFF") {
		t.Fatalf("relay 1 only: %v", f.powers)
	}
	if resp, _ := e.do(t, "POST", "/devices/bulk/power", "operator", url.Values{"csrf": {"tok"}, "action": {"TOGGLE"}, "id": {"1"}}); resp.StatusCode != 400 {
		t.Fatalf("only ON and OFF: %d", resp.StatusCode)
	}
}

// Why: "Stagger by 1 s" keeps the inrush currents of many loads from adding up: switching on
// waits between two devices; without it (and always for Power off) nothing waits.
func TestBulkPowerStaggersSwitchingOn(t *testing.T) {
	f := &fakeCmd{state: "ON"}
	e := cmdEnv(t, f)
	e.health[1] = model.Health{DeviceID: 1, State: model.HealthOnline, Power: map[string]string{"POWER1": "OFF"}}
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOnline, Power: map[string]string{"POWER1": "OFF"}}
	run := func(extra ...string) time.Duration {
		form := url.Values{"csrf": {"tok"}, "action": {"ON"}, "id": {"1", "2"}, "confirm": {"1"}}
		for i := 0; i+1 < len(extra); i += 2 {
			form.Set(extra[i], extra[i+1])
		}
		start := time.Now()
		e.do(t, "POST", "/devices/bulk/power", "operator", form)
		return time.Since(start)
	}
	if d := run("stagger", "1"); d < bulkStagger {
		t.Errorf("staggered: took %v, want at least %v", d, bulkStagger)
	}
	if d := run(); d >= bulkStagger {
		t.Errorf("not staggered: took %v", d)
	}
	if d := run("stagger", "1", "action", "OFF"); d >= bulkStagger {
		t.Errorf("switching off never waits: took %v", d)
	}
}

// Why (finding 83): "Switch on selected" with nothing selected is a no-op the UI used to offer
// and then answer with an error; the server stays the authority and refuses it, nothing is sent.
func TestBulkPowerWithoutSelectionIsRefused(t *testing.T) {
	f := &fakeCmd{state: "ON"}
	e := cmdEnv(t, f)
	for _, action := range []string{"ON", "OFF"} {
		resp, body := e.do(t, "POST", "/devices/bulk/power", "operator", url.Values{"csrf": {"tok"}, "action": {action}})
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Nothing switched") {
			t.Fatalf("%s without ids: %d %s", action, resp.StatusCode, body)
		}
	}
	if len(f.powers) != 0 {
		t.Fatalf("nothing may be sent: %v", f.powers)
	}
}

// Why (audit row 15, 2026-10-21): the edit form no longer refuses an address
// change that keeps the stored password; it warns beforehand, only for a
// device that has one.
func TestDeviceEditFormWarnsAboutTheStoredPassword(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	_, with := e.do(t, "GET", "/devices/1/edit", "admin", nil)
	if !strings.Contains(with, "stored web password will be sent to the new address") {
		t.Fatalf("warning missing for a device with a password: %s", with)
	}
	_, without := e.do(t, "GET", "/devices/2/edit", "admin", nil)
	if strings.Contains(without, "will be sent to the new address") {
		t.Fatalf("warning shown for a device without a password: %s", without)
	}
}

// Why (second security review, point 1): the edit confirmation covers exactly
// the address it was issued for. Changing the address after the warning, or
// replaying the token, asks again and sends nothing.
func TestEditConfirmationTokenIsBoundToTheTargetAndSingleUse(t *testing.T) {
	e := cmdEnv(t, &fakeCmd{})
	e.inv.unproven = true
	form := url.Values{"name": {"kitchen"}, "address": {"10.0.0.99"}, "port": {"80"}}
	_, body := e.do(t, "POST", "/devices/1", "admin", form)
	token := confirmToken(t, body)
	other := url.Values{"name": {"kitchen"}, "address": {"10.0.0.98"}, "port": {"80"}, "token": {token}}
	if r, _ := e.do(t, "POST", "/devices/1", "admin", other); r.StatusCode != 409 || e.inv.confirmedMoves != 0 {
		t.Fatalf("a token for .99 must not cover .98: %d moves %d", r.StatusCode, e.inv.confirmedMoves)
	}
	_, body = e.do(t, "POST", "/devices/1", "admin", form)
	form.Set("token", confirmToken(t, body))
	if r, _ := e.do(t, "POST", "/devices/1", "operator", form); r.StatusCode != 403 || e.inv.confirmedMoves != 0 {
		t.Fatalf("operator: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/devices/1", "admin", form); r.StatusCode != 303 || e.inv.confirmedMoves != 1 {
		t.Fatalf("owner: %d moves %d", r.StatusCode, e.inv.confirmedMoves)
	}
	form.Set("address", "10.0.0.99")
	e.inv.devs[1] = func() model.Device { d := e.inv.devs[1]; d.Address = "10.0.0.1"; return d }()
	if r, _ := e.do(t, "POST", "/devices/1", "admin", form); r.StatusCode != 409 || e.inv.confirmedMoves != 1 {
		t.Fatalf("a used token must not work again: %d moves %d", r.StatusCode, e.inv.confirmedMoves)
	}
}
