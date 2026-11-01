package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// Why: a fresh "New update" starts with every device that has an update ticked (the prototype's
// default), so the common case is one click; a device that cannot be updated (offline) is never
// ticked, and a page opened with a selection (the device list's bulk bar) keeps exactly that one.
func TestNewUpdatePreselectsTheOutdatedDevicesThatCanBeUpdated(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = nil
	e.health[2] = model.Health{DeviceID: 2, State: model.HealthOffline}
	_, body := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(body, `value="1" aria-label="Select kitchen" checked`) {
		t.Errorf("the outdated online device is not preselected: %s", body)
	}
	if strings.Contains(body, `aria-label="Select attic" checked`) || !strings.Contains(body, `aria-label="Select attic" disabled`) {
		t.Errorf("an offline device must be neither selectable nor preselected")
	}
	if !strings.Contains(body, `<dd data-plan-devices>2</dd>`) {
		t.Errorf("the plan counts the preselected devices without JavaScript: %s", body)
	}
	_, body = e.do(t, "POST", "/updates/new", "operator", url.Values{"id": {"3"}})
	if strings.Contains(body, `aria-label="Select kitchen" checked`) || !strings.Contains(body, `aria-label="Select stuck" checked`) {
		t.Errorf("a posted selection replaces the default")
	}
}

// Why: the engine refuses a device that is in a running transaction; the picker says so before
// the preview instead of offering a device that cannot be updated now.
func TestDeviceInARunningTransactionIsNotSelectable(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, CreatedAt: time.Now()},
		{ID: 2, BatchID: 6, DeviceID: 3, State: model.JobSucceeded, CreatedAt: time.Now()},
	}
	_, body := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(body, `aria-label="Select kitchen" disabled`) || !strings.Contains(body, "In a running transaction") {
		t.Errorf("the device that is being updated is offered: %s", body)
	}
	if !strings.Contains(body, `aria-label="Select stuck" checked`) {
		t.Errorf("a finished job must not block its device")
	}
}

// Why: the picker's checkbox says "Back up each device before flashing"; leaving it unticked is
// the explicit choice to skip the backup, and it must reach the engine. A form without the
// checkbox (the preview's hidden fields) keeps meaning "back up" unless it says skip_backup=1.
func TestBackupCheckboxDecidesTheBackup(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	base := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	for _, c := range []struct {
		name string
		add  url.Values
		skip bool
	}{
		{"ticked", url.Values{"backup_shown": {"1"}, "backup": {"1"}}, false},
		{"unticked", url.Values{"backup_shown": {"1"}}, true},
		{"hidden fields, default", url.Values{}, false},
		{"hidden fields, skip", url.Values{"skip_backup": {"1"}}, true},
	} {
		form := url.Values{}
		for k, v := range base {
			form[k] = v
		}
		for k, v := range c.add {
			form[k] = v
		}
		_, body := e.do(t, "POST", "/updates/preview", "operator", form)
		if o.previewReqs[0].SkipBackup != c.skip {
			t.Errorf("%s: SkipBackup = %v, want %v", c.name, o.previewReqs[0].SkipBackup, c.skip)
		}
		if strings.Contains(body, `name="skip_backup" value="1"`) != c.skip {
			t.Errorf("%s: the choice does not survive the preview", c.name)
		}
	}
}

// Why: the preview's "Change" goes back to the picker with the same devices and options, so a
// second look does not mean picking everything again.
func TestPreviewChangeKeepsTheChoice(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	form := url.Values{"id": {"2"}, "release": {"16.0.0"}, "policy": {"reinstall"}, "power_after": {"leave"}, "skip_backup": {"1"}}
	_, body := e.do(t, "POST", "/updates/new", "operator", form)
	for _, want := range []string{`aria-label="Select attic" checked`, `value="16.0.0" checked`, `value="reinstall" title="Flash the release even if the device already runs it." selected`, `value="leave" selected`} {
		if !strings.Contains(body, want) {
			t.Errorf("the picker lost %q", want)
		}
	}
	if strings.Contains(body, `name="backup" value="1" checked`) {
		t.Errorf("the skipped backup came back ticked")
	}
}

// Why: the open log is the one that needs attention: a failed device first, else one that is
// running; ?job= keeps the reader's choice across the page's refreshes.
func TestTransactionOpensTheLogThatNeedsAttention(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: now},
		{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobUpgradeIssued, TargetRelease: "16.1.0", CreatedAt: now},
		{ID: 3, BatchID: 7, DeviceID: 3, State: model.JobFailed, TargetRelease: "16.1.0", LastError: "device did not come back", CreatedAt: now},
	}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	if !strings.Contains(body, "Log · stuck") || !strings.Contains(body, `id="job-3" aria-current="true"`) {
		t.Errorf("the failed device's log is not the open one: %s", body)
	}
	if !strings.Contains(body, "Error: device did not come back") {
		t.Errorf("the failure is not in the log")
	}
	_, body = e.do(t, "GET", "/transactions/7?job=2", "operator", nil)
	if !strings.Contains(body, "Log · attic") || !strings.Contains(body, `/tf/transactions/7/live?job=2`) {
		t.Errorf("the chosen log is not kept for the refresh: %s", body)
	}
}

// Why: "Retry failed" while devices still run would only be refused (the engine retries a
// finished transaction); the button says so instead of leading to the refusal.
func TestRetryWaitsForTheTransactionToFinish(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobFailed, TargetRelease: "16.1.0", CreatedAt: now},
		{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobQueued, TargetRelease: "16.1.0", CreatedAt: now},
	}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	if !strings.Contains(body, `disabled title="Retry when every device has finished">Retry failed (1)`) || strings.Contains(body, `/transactions/7/retry"`) {
		t.Errorf("a running transaction offers a working retry: %s", body)
	}
	if !strings.Contains(body, "The 1 queued device is skipped.") || !strings.Contains(body, `data-dlg-verb="Cancel remaining"`) {
		t.Errorf("Cancel remaining does not say what it skips: %s", body)
	}
}

// Why: a "Make compliant" transaction is about settings, not firmware: it shows each device with
// the commands it got and whether they were applied, and leads back to the policy.
func TestMakeCompliantTransactionShowsTheChanges(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, Kind: model.KindPolicy, Commands: "TelePeriod 300", State: model.JobSucceeded, CreatedBy: "adam", CreatedAt: now},
		{ID: 2, BatchID: 7, DeviceID: 2, Kind: model.KindPolicy, Commands: "TelePeriod 300\nPowerOnState 0", State: model.JobFailed, LastError: "the device is unreachable", CreatedAt: now},
	}
	_, body := e.do(t, "GET", "/transactions/7", "admin", nil)
	for _, want := range []string{"<h1>Make compliant</h1>", "started from Policy by adam · 2 devices · 3 changes", "PowerOnState 0", ">Applied<", ">Failed<", "The device is unreachable", ">Back to policy<", "<b>1</b> applied"} {
		if !strings.Contains(body, want) {
			t.Errorf("the compliant page lacks %q", want)
		}
	}
	if strings.Contains(body, "Log · ") {
		t.Errorf("a policy transaction has no firmware log panel")
	}
}

// Why: the history is one row per transaction, newest first, with what it did and how it ended;
// a running one is also on top as a card with its progress.
func TestTransactionsListRunningCardAndHistory(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	j.jobs = []model.Job{
		{ID: 1, BatchID: 4, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.0.0", CreatedAt: now.Add(-time.Hour)},
		{ID: 2, BatchID: 5, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: now},
		{ID: 3, BatchID: 5, DeviceID: 2, State: model.JobQueued, TargetRelease: "16.1.0", CreatedAt: now},
	}
	_, page := e.do(t, "GET", "/transactions", "operator", nil)
	body := between(page, `<main id="main">`, "</main>") // the status bar below it names the running transaction too
	card := strings.Index(body, `class="txn-card"`)
	tx5, tx4 := strings.LastIndex(body, ">TX-0005<"), strings.Index(body, ">TX-0004<")
	if card < 0 || tx5 < 0 || tx4 < 0 || tx5 > tx4 || card > tx5 {
		t.Fatalf("want the running card, then TX-0005 before TX-0004: %s", body)
	}
	if !strings.Contains(body, "1 of 2 done") || !strings.Contains(body, ">Update to Tasmota 16.1.0<") || !strings.Contains(body, ">2 devices<") {
		t.Errorf("the running transaction's card or row lacks its facts: %s", body)
	}
}

func TestTxnWording(t *testing.T) {
	if got := txID(143); got != "TX-0143" {
		t.Errorf("txID = %q", got)
	}
	for _, c := range []struct {
		done, run, q, fail, canc int
		want                     string
		tone                     ui.Tone
	}{
		{5, 3, 3, 1, 0, "Running", ui.ToneAccent},
		{6, 0, 0, 0, 0, "Completed", ui.ToneOK},
		{30, 0, 0, 1, 0, "30 done · 1 failed", ui.ToneDanger}, // a failure is never green
		{0, 0, 0, 0, 4, "Cancelled", ui.ToneNeutral},
		{0, 0, 0, 2, 0, "Failed", ui.ToneDanger},
		{2, 0, 0, 0, 1, "2 done · 1 cancelled", ui.ToneNeutral},
	} {
		got, tone := txResult(c.done, c.run, c.q, c.fail, c.canc)
		if got != c.want || tone != c.tone {
			t.Errorf("txResult%v = %q %s, want %q %s", c, got, tone, c.want, c.tone)
		}
	}
	for f, want := range map[model.ChipFamily]string{model.ChipESP8266: "ESP8266", model.ChipESP32: "ESP32", model.ChipESP32C3: "ESP32-C3"} {
		if got := chipName(f); got != want {
			t.Errorf("chipName(%s) = %q", f, got)
		}
	}
	// 19 devices, 3 at a time: 7 rounds of a minute
	if got := updateEstimate(19, 3); got != "7 min" {
		t.Errorf("updateEstimate = %q", got)
	}
	if got := policySettings("TelePeriod 300\nPowerOnState 0\nTelePeriod 60"); got != "TelePeriod, PowerOnState" {
		t.Errorf("policySettings = %q", got)
	}
	if l, _ := jobBadge(model.Job{State: model.JobUpgradeIssued, CancelRequested: true}); l != "Cancelling" {
		t.Errorf("a pending cancel reads %q", l)
	}
}

// Why: an "Apply GPIO template" transaction (#327) stops at the first failure and is never re-run, so
// its page must show for each device how far it got (a device whose Verify failed has been sent the
// command; a stopped one has been sent nothing), word the counts as applied/failed/stopped, offer
// "Check again…" instead of "Retry failed", and refuse the retry route.
func TestTemplateTransactionShowsStepsStopAndCheckAgain(t *testing.T) {
	e, _, j, _ := setupOTA(t)
	now := time.Now()
	cmd := `Template {"NAME":"Gosund SP111","GPIO":[1],"FLAG":0,"BASE":45}` + "\nhave a|0|1\nwant b|0|1"
	j.jobs = []model.Job{
		{ID: 1, BatchID: 9, DeviceID: 1, Kind: model.KindTemplate, Commands: cmd, State: model.JobSucceeded, CreatedBy: "adam", CreatedAt: now},
		{ID: 2, BatchID: 9, DeviceID: 2, Kind: model.KindTemplate, Commands: cmd, State: model.JobFailed, LastError: "the device did not answer in time", CreatedAt: now},
		{ID: 3, BatchID: 9, DeviceID: 3, Kind: model.KindTemplate, Commands: cmd, State: model.JobCancelled, LastError: "Nothing was sent: the transaction stopped after job 2 failed", CreatedAt: now},
	}
	e.aud.Record(t.Context(), model.AuditEvent{Action: "policy.template.start", Target: "transaction:9", Detail: `{"group":"esp8266 Gosund SP111","reference":1,"how":"chosen"}`})
	_, body := e.do(t, "GET", "/transactions/9", "admin", nil)
	for _, want := range []string{"Gosund SP111, reference kitchen (chosen) · 3 devices, one at a time", "<h1>Apply GPIO template</h1>", "started from Policy by adam on ", "<b>1</b> applied", "<b>1</b> stopped", "Check again…", ">Applied<", ">Not started<",
		"Nothing was sent to this device", "Its backup from this run can be restored", `Template {&#34;NAME&#34;:&#34;Gosund SP111&#34;,…}`, "1 applied · 1 failed · 1 stopped"} {
		if !strings.Contains(body, want) {
			t.Errorf("the template page lacks %q", want)
		}
	}
	if strings.Contains(body, "Retry failed") {
		t.Error("a template transaction is checked again, not retried")
	}
	r, _ := e.do(t, "GET", "/transactions/9/retry", "admin", nil)
	if r.StatusCode != 409 {
		t.Errorf("retry of a template transaction: %d, want 409", r.StatusCode)
	}
}

// Why: operators are read-only for GPIO templates (#327), and a transaction of a kind this build does
// not know must not be open to them either (default-deny). Every route that cancels or re-runs it
// decides on the role first, whatever the kind-specific branches do afterwards; an update (the
// original operator case) stays open.
func TestCancelAndRetryOfTemplateAndUnknownKindsAreAdminOnly(t *testing.T) {
	for _, kind := range []model.JobKind{model.KindTemplate, "mystery"} {
		t.Run(string(kind), func(t *testing.T) {
			e, o, j, _ := setupOTA(t)
			j.jobs = []model.Job{{ID: 31, BatchID: 9, DeviceID: 1, Kind: kind, Commands: "Template {}", State: model.JobApplying, CreatedBy: "adam", CreatedAt: time.Now()}}
			routes := []struct{ method, path string }{
				{"GET", "/transactions/9/cancel"}, {"POST", "/transactions/9/cancel"},
				{"GET", "/jobs/31/cancel"}, {"POST", "/jobs/31/cancel"},
				{"GET", "/transactions/9/retry"}, {"POST", "/transactions/9/retry"},
			}
			for _, rt := range routes {
				var form url.Values
				if rt.method == "POST" {
					form = url.Values{"csrf": {"tok"}}
				}
				if r, _ := e.do(t, rt.method, rt.path, "operator", form); r.StatusCode != 403 {
					t.Errorf("operator %s %s: %d, want 403", rt.method, rt.path, r.StatusCode)
				}
			}
			if len(o.cancelled) != 0 || len(o.cancelledBatches) != 0 {
				t.Fatalf("an operator cancelled something: %v %v", o.cancelled, o.cancelledBatches)
			}
			for _, rt := range routes {
				var form url.Values
				if rt.method == "POST" {
					form = url.Values{"csrf": {"tok"}}
				}
				if r, _ := e.do(t, rt.method, rt.path, "admin", form); r.StatusCode == 403 {
					t.Errorf("admin %s %s: refused", rt.method, rt.path)
				}
			}
			if kind == model.KindTemplate {
				if r, _ := e.do(t, "GET", "/transactions/9/retry", "admin", nil); r.StatusCode != 409 {
					t.Errorf("admin retry of a template transaction: %d, want 409 (checked again, never re-run)", r.StatusCode)
				}
			}
		})
	}
	e, _, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 32, BatchID: 10, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedAt: time.Now()}}
	if r, _ := e.do(t, "POST", "/transactions/10/cancel", "operator", url.Values{"csrf": {"tok"}}); r.StatusCode == 403 {
		t.Error("an operator may still cancel a firmware update")
	}
}

// Why: if the engine refuses a retry because the transaction is a GPIO template one (which is never
// replayed), the person gets the pointer to "Check again…", not a 500 page.
func TestRetryRefusedByTheEngineAsNotRetryableSaysCheckAgain(t *testing.T) {
	e, o, j, _ := setupOTA(t)
	j.jobs = []model.Job{{ID: 40, BatchID: 11, DeviceID: 1, State: model.JobFailed, CreatedAt: time.Now()}}
	o.retryErr = ota.ErrNotRetryable
	r, body := e.do(t, "POST", "/transactions/11/retry", "admin", url.Values{"csrf": {"tok"}})
	if r.StatusCode != 409 || !strings.Contains(body, "Check again") {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
}
