package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
)

// fakeBatch records the command transaction the page asks for.
type fakeBatch struct {
	mu   sync.Mutex
	reqs [][]ota.PolicyRequest
	skip []ota.Refusal
	opts []ota.BatchOptions
}

func (f *fakeBatch) Submit(ctx context.Context, _ string, reqs []ota.PolicyRequest) (model.BatchID, []ota.Refusal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, reqs)
	f.opts = append(f.opts, ota.BatchOptionsOf(ctx))
	return 12, f.skip, nil
}

func bulkEnv(t *testing.T) (*env, *fakeBatch) {
	t.Helper()
	fb := &fakeBatch{}
	e := setupWith(t, func(d *Deps) { d.DeviceCmd = &fakeCmd{}; d.CommandBatch = fb })
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline, LastSeen: time.Now()}
	return e, fb
}

func bulkForm(extra ...string) url.Values {
	v := url.Values{"csrf": {"tok"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Add(extra[i], extra[i+1])
	}
	return v
}

// Why: the bulk command is an admin tool, and the preview is the confirmation:
// it names who gets the command and who is left out, sends nothing, and the
// run takes command and targets from the server-held token, not from the form.
func TestBulkCommandPreviewThenRun(t *testing.T) {
	e, fb := bulkEnv(t)
	resp, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2"))
	if resp.StatusCode != 200 || !strings.Contains(body, `name="command"`) {
		t.Fatalf("command box: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "command", "  Status   2 "))
	if resp.StatusCode != 200 || !strings.Contains(body, "Send to 1 device") || !strings.Contains(body, "1 device run · 1 skipped") || strings.Contains(body, `class="banner tone-danger"`) {
		t.Fatalf("preview: %d %s", resp.StatusCode, body)
	}
	if len(fb.reqs) != 0 {
		t.Fatal("the preview must send nothing")
	}
	token := confirmToken(t, body)
	// a forged form cannot add devices or change the command
	resp, _ = e.do(t, "POST", "/devices/bulk/command/run", "admin", bulkForm("token", token, "id", "2", "command", "Reset 1"))
	if resp.StatusCode != http.StatusSeeOther || len(fb.reqs) != 1 || len(fb.reqs[0]) != 1 {
		t.Fatalf("run: %d %v", resp.StatusCode, fb.reqs)
	}
	if loc := resp.Header.Get("Location"); loc != "/tf/devices/bulk/result/12?id=2" {
		t.Errorf("the result page of the transaction, with the posted devices (only the form knows the skipped ones): %q", loc)
	}
	if r := fb.reqs[0][0]; r.DeviceID != 1 || len(r.Commands) != 1 || r.Commands[0] != "Status 2" {
		t.Fatalf("the canonical command to the online device only: %+v", r)
	}
	if resp, _ := e.do(t, "POST", "/devices/bulk/command/run", "admin", bulkForm("token", token)); resp.StatusCode != http.StatusConflict || len(fb.reqs) != 1 {
		t.Fatalf("a token works once: %d", resp.StatusCode)
	}
}

// Why: same policy and confirmation texts as the console: a destructive command
// names its consequence in the preview, a Backlog inherits it from its parts.
func TestBulkCommandPreviewNamesTheConsequence(t *testing.T) {
	e, _ := bulkEnv(t)
	for _, cmd := range []string{"Reset 1", "Backlog Power1 ON; WifiConfig 2", "Status 0"} {
		_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", cmd))
		if !strings.Contains(body, `class="banner tone-danger"`) {
			t.Errorf("%q: no consequence: %s", cmd, body)
		}
	}
}

// Why: refusals happen at the preview, before any token exists.
func TestBulkCommandRefusals(t *testing.T) {
	e, fb := bulkEnv(t)
	for name, tc := range map[string]struct {
		role, cmd string
		ids       []string
		want      int
	}{
		"operator":   {"operator", "Status 2", []string{"id", "1"}, 403},
		"anonymous":  {"", "Status 2", []string{"id", "1"}, 401},
		"malformed":  {"admin", "Power ON;Reset 1", []string{"id", "1"}, 422},
		"credential": {"admin", "MqttPassword hunter2", []string{"id", "1"}, 422},
		"offline":    {"admin", "Status 2", []string{"id", "2"}, 422},
	} {
		t.Run(name, func(t *testing.T) {
			form := bulkForm(append(tc.ids, "command", tc.cmd)...)
			resp, body := e.do(t, "POST", "/devices/bulk/command", tc.role, form)
			if resp.StatusCode != tc.want || strings.Contains(body, "hunter2") {
				t.Fatalf("%d: %s", resp.StatusCode, body)
			}
		})
	}
	if r, _ := e.do(t, "POST", "/devices/bulk/command/run", "operator", bulkForm("token", "x")); r.StatusCode != 403 {
		t.Fatalf("operator run: %d", r.StatusCode)
	}
	if len(fb.reqs) != 0 {
		t.Fatal("nothing may be submitted")
	}
}

// Why (no implicit "all devices"): the selection is the scope. Without selected devices the
// page says so and offers no way to run; "scope=all" (the old all-devices choice) is no scope.
// A command without a token is never sent, and a redraw keeps the typed command and options.
func TestBulkCommandRunsOnlyOnTheSelection(t *testing.T) {
	e, fb := bulkEnv(t)
	for name, form := range map[string]url.Values{
		"nothing selected":  bulkForm("command", "Status 2"),
		"all is no scope":   bulkForm("scope", "all", "command", "Status 2"),
		"only offline ones": bulkForm("id", "2", "command", "Status 2"),
	} {
		resp, body := e.do(t, "POST", "/devices/bulk/command", "admin", form)
		if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, `name="token"`) {
			t.Errorf("%s: %d, a token must not exist: %s", name, resp.StatusCode, body)
		}
	}
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2"))
	if !strings.Contains(body, "Devices (2)") || !strings.Contains(body, `name="wait" value="1" checked`) || strings.Contains(body, `name="serial" value="1" checked`) {
		t.Errorf("first visit: both devices, the prototype's default options: %s", body)
	}
	_, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "id", "2", "opts", "1", "serial", "1", "command", "Status 2", "drop", "2"))
	if !strings.Contains(body, "Devices (1)") || !strings.Contains(body, ">Status 2</textarea>") || strings.Contains(body, `name="wait" value="1" checked`) || !strings.Contains(body, `name="serial" value="1" checked`) {
		t.Errorf("a removal redraws with the typed command and options: %s", body)
	}
	if len(fb.reqs) != 0 {
		t.Fatal("nothing may be submitted by a preview")
	}
}

// Why: the bulk bar offers the button to admins only.
func TestBulkCommandButtonIsAdminOnly(t *testing.T) {
	e, _ := bulkEnv(t)
	if _, body := e.do(t, "GET", "/devices", "admin", nil); !strings.Contains(body, "Send command") {
		t.Error("admin: button missing")
	}
	if _, body := e.do(t, "GET", "/devices", "operator", nil); strings.Contains(body, "Send command") {
		t.Error("operator: button shown")
	}
}

// Why (security review): a command transaction is retried by an admin only,
// and a retry that holds a destructive command asks again on its own page.
func TestCommandTransactionRetryNeedsAnAdminAndANewConfirmation(t *testing.T) {
	e, o, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Reset 1", State: model.JobFailed, CreatedAt: time.Now()}}
	if r, _ := e.do(t, "GET", "/transactions/7/retry", "operator", nil); r.StatusCode != 403 {
		t.Fatalf("operator form: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "operator", url.Values{}); r.StatusCode != 403 || len(o.retryCalls) != 0 {
		t.Fatalf("operator retry: %d", r.StatusCode)
	}
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{}); r.StatusCode != 409 || len(o.retryCalls) != 0 {
		t.Fatalf("a destructive retry without the confirmation: %d", r.StatusCode)
	}
	_, body := e.do(t, "GET", "/transactions/7/retry", "admin", nil)
	if !strings.Contains(body, `class="warning"`) {
		t.Fatalf("the consequence must be named: %s", body)
	}
	o.retryRes = ota.RetryResult{Batch: 10, Devices: []model.DeviceID{1}}
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{"token": {confirmToken(t, body)}}); r.StatusCode != 303 || len(o.retryCalls) != 1 {
		t.Fatalf("confirmed retry: %d %v", r.StatusCode, o.retryCalls)
	}
	// a routine command needs no new confirmation
	j.jobs[0].Commands = "Status 2"
	o.retryCalls = nil
	if r, _ := e.do(t, "POST", "/transactions/7/retry", "admin", url.Values{}); r.StatusCode != 303 || len(o.retryCalls) != 1 {
		t.Fatalf("routine retry: %d", r.StatusCode)
	}
}

// Why: the stored command may hold a secret (a Rule, a Color value); the
// transaction pages, which operators can read too, show only its redacted form.
func TestCommandTransactionShowsOnlyTheRedactedCommand(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindCommand, Commands: "Rule1 ON x DO Publish a hunter2 ENDON", State: model.JobSucceeded, CreatedAt: time.Now()}}
	for _, p := range []string{"/transactions", "/transactions/7", "/devices/1"} {
		_, body := e.do(t, "GET", p, "operator", nil)
		if strings.Contains(body, "hunter2") {
			t.Errorf("%s shows the raw command", p)
		}
	}
	if _, body := e.do(t, "GET", "/transactions/7", "operator", nil); !strings.Contains(body, "Send Rule1 ***") {
		t.Errorf("the transaction page names the redacted command: %s", body)
	}
	if _, body := e.do(t, "GET", "/transactions", "operator", nil); !strings.Contains(body, "Send Rule1 ***") {
		t.Errorf("the redacted command is the target: %s", body)
	}
}

// Why (design v2, N15): the run form's options reach the transaction: "Wait up
// to 5 s for each response", "One device at a time" and the restart page's
// "Devices at a time". A value the page does not offer is refused before the
// token is used, so the confirmed preview can still be sent.
func TestBulkRunPassesOptions(t *testing.T) {
	cases := []struct {
		path   string
		form   []string
		status int
		want   ota.BatchOptions
	}{
		{"/devices/bulk/command", nil, http.StatusSeeOther, ota.BatchOptions{}},
		{"/devices/bulk/command", []string{"wait", "1", "serial", "1"}, http.StatusSeeOther, ota.BatchOptions{Parallel: 1, ReplyTimeout: 5 * time.Second}},
		{"/devices/bulk/restart", []string{"parallel", "3"}, http.StatusSeeOther, ota.BatchOptions{Parallel: 3}},
		{"/devices/bulk/restart", []string{"parallel", "5", "serial", "1"}, http.StatusSeeOther, ota.BatchOptions{Parallel: 1}},
		{"/devices/bulk/restart", []string{"parallel", "50"}, http.StatusUnprocessableEntity, ota.BatchOptions{}},
	}
	for _, c := range cases {
		e, fb := bulkEnv(t)
		preview := bulkForm("id", "1", "command", "Status 5")
		_, body := e.do(t, "POST", c.path, "admin", preview)
		token := confirmToken(t, body)
		resp, _ := e.do(t, "POST", c.path+"/run", "admin", bulkForm(append([]string{"token", token}, c.form...)...))
		if resp.StatusCode != c.status {
			t.Errorf("%s %v: status %d, want %d", c.path, c.form, resp.StatusCode, c.status)
			continue
		}
		if c.status != http.StatusSeeOther {
			if len(fb.reqs) != 0 {
				t.Errorf("%v: sent although refused", c.form)
			}
			if resp, _ := e.do(t, "POST", c.path+"/run", "admin", bulkForm("token", token)); resp.StatusCode != http.StatusSeeOther {
				t.Errorf("%v: the refusal used up the token (%d)", c.form, resp.StatusCode)
			}
			continue
		}
		if len(fb.opts) != 1 || fb.opts[0] != c.want {
			t.Errorf("%s %v: options %+v, want %+v", c.path, c.form, fb.opts, c.want)
		}
	}
}

// Why (D13): a bulk command runs as jobs that carry no person's consent, so a change that can cut off a
// device reached over MQTT only is refused in the preview, before anything is queued; the same command
// goes through when no selected device is MQTT-only.
func TestBulkCommandRefusesACutOffChangeForMQTTOnlyDevices(t *testing.T) {
	fb := &fakeBatch{}
	only := true
	e := setupWith(t, func(d *Deps) {
		d.DeviceCmd, d.CommandBatch = &fakeCmd{}, fb
		d.MQTTOnly = func(context.Context, ...model.DeviceID) bool { return only }
	})
	_, body := e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", "Topic other"))
	if !strings.Contains(body, "can cut a device off") || strings.Contains(body, `name="token"`) || len(fb.reqs) != 0 {
		t.Fatalf("preview: %.300s", body)
	}
	only = false
	if _, body = e.do(t, "POST", "/devices/bulk/command", "admin", bulkForm("id", "1", "command", "Topic other")); !strings.Contains(body, `name="token"`) {
		t.Fatalf("a device reached over HTTP: %.300s", body)
	}
}
