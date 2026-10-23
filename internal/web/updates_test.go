package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

type fakeOTA struct {
	retryCalls []retryCall
	retryRes   ota.RetryResult
	retryErr   error

	previewReqs []ota.Request
	submitReqs  []ota.Request
	submitErr   error
	items       map[model.DeviceID]ota.PreviewItem
	cancelled   []model.JobID

	cancelledBatches []model.BatchID
}

func (f *fakeOTA) Preview(_ context.Context, reqs []ota.Request) ([]ota.PreviewItem, error) {
	f.previewReqs = reqs
	var out []ota.PreviewItem
	for _, r := range reqs {
		it, ok := f.items[r.DeviceID]
		if !ok {
			it = ota.PreviewItem{DeviceID: r.DeviceID, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota.bin.gz"}
		}
		out = append(out, it)
	}
	return out, nil
}
func (f *fakeOTA) Submit(_ context.Context, _ string, reqs []ota.Request) (model.BatchID, error) {
	f.submitReqs = reqs
	return 7, f.submitErr
}
func (f *fakeOTA) Cancel(_ context.Context, _ string, id model.JobID) error {
	f.cancelled = append(f.cancelled, id)
	return nil
}
func (f *fakeOTA) CancelBatch(_ context.Context, _ string, id model.BatchID) error {
	f.cancelledBatches = append(f.cancelledBatches, id)
	return nil
}

type fakeJobs struct{ jobs []model.Job }

func (f *fakeJobs) ListJobs(_ context.Context, fl store.JobFilter) ([]model.Job, error) {
	var out []model.Job
	for _, j := range f.jobs {
		if fl.BatchID != 0 && j.BatchID != fl.BatchID {
			continue
		}
		if fl.DeviceID != 0 && j.DeviceID != fl.DeviceID {
			continue
		}
		if len(fl.States) > 0 && !slices.Contains(fl.States, j.State) {
			continue
		}
		out = append(out, j)
	}
	if fl.Limit > 0 && len(out) > fl.Limit {
		out = out[:fl.Limit]
	}
	return out, nil
}
func (f *fakeJobs) JobEvents(context.Context, model.JobID) ([]model.JobEvent, error) {
	return []model.JobEvent{{Event: "upgrade_issued", Detail: "ok"}}, nil
}
func (f *fakeJobs) GetJob(_ context.Context, id model.JobID) (model.Job, error) {
	for _, j := range f.jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return model.Job{}, store.ErrNotFound
}

// fakeBatches knows two batches: 8 retries 7, and 7 was retried in 8 and 9.
type fakeBatches struct{}

func (fakeBatches) GetBatch(_ context.Context, id model.BatchID) (model.Batch, error) {
	if id == 8 {
		return model.Batch{ID: 8, RetryOf: 7}, nil
	}
	return model.Batch{ID: id}, nil
}
func (fakeBatches) CreateRetryBatch(context.Context, string, model.BatchID, []model.Job) (model.BatchID, []model.JobID, error) {
	return 0, nil, errors.New("not used")
}
func (fakeBatches) BatchRetries(_ context.Context, id model.BatchID) ([]model.Batch, error) {
	if id == 7 {
		return []model.Batch{{ID: 8, RetryOf: 7}, {ID: 9, RetryOf: 7}}, nil
	}
	return nil, nil
}

type fakeFW struct{ refreshed bool }

func (f *fakeFW) Releases(context.Context) ([]firmware.Release, error) {
	return []firmware.Release{{Version: "16.1.0", Files: []string{"tasmota.bin.gz"}}, {Version: "16.0.0"}}, nil
}
func (f *fakeFW) Ensure(_ context.Context, rel, file string) (model.Artifact, error) {
	return model.Artifact{Release: rel, Filename: file}, nil
}
func (f *fakeFW) URLFor(model.Artifact) string            { return "" }
func (f *fakeFW) Prune(context.Context, int) (int, error) { return 0, nil }
func (f *fakeFW) Refresh(context.Context) error           { f.refreshed = true; return nil }

func setupOTA(t *testing.T) (*env, *fakeOTA, *fakeJobs, *fakeFW) {
	t.Helper()
	e := setup(t)
	// devices: 1 kitchen (esp8266 tasmota), 2 attic (esp32), 3 stuck on minimal
	e.inv.devs[3] = model.Device{ID: 3, Name: "stuck", Address: "10.0.0.3", Identity: model.Identity{Family: model.ChipESP8266, Version: "16.0.0", Variant: "minimal"}}
	o := &fakeOTA{items: map[model.DeviceID]ota.PreviewItem{}}
	j := &fakeJobs{jobs: []model.Job{{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobAwaitReboot, TargetRelease: "16.1.0", CreatedAt: time.Now()}}}
	fw := &fakeFW{}
	h, err := Handler(Deps{Guard: fakeGuard{}, Inventory: e.inv, Health: e.health, Backups: e.bk, Prober: e.pr, Audit: e.aud, Recorder: e.aud,
		Hub: e.hub, OTA: o, Jobs: j, Firmware: fw, Batches: fakeBatches{}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv.Close()
	e.srv = httptest.NewServer(http.NewServeMux())
	mux := http.NewServeMux()
	mux.Handle("/tf/", httpx.Proxy{BasePath: "/tf"}.Middleware(h))
	e.srv.Config.Handler = mux
	t.Cleanup(e.srv.Close)
	return e, o, j, fw
}

// Why: the engine's Submit is all-or-nothing, so the preview page must only
// carry devices that would really be updated into the submit form; one
// refused device would otherwise block the whole batch.
func TestPreviewSubmitsOnlyActionableDevices(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[2] = ota.PreviewItem{DeviceID: 2, Decision: "refuse", Reason: "unknown hardware"}
	form := url.Values{"id": {"1", "2"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	resp, body := e.do(t, "POST", "/updates/preview", "operator", form)
	if resp.StatusCode != 200 || !strings.Contains(body, "unknown hardware") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	start, _, _ := strings.Cut(body[strings.Index(body, `<form id="start"`):], "</form>")
	if !strings.Contains(start, `name="id" value="1"`) || strings.Contains(start, `name="id" value="2"`) {
		t.Fatalf("submit form must carry device 1 only: %s", start)
	}
}

// Why: the admin typed the variant on purpose; a second confirmation checkbox
// only added a click (over-restriction audit #21), the preview still shows it.
func TestVariantOverrideNeedsNoConfirmation(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	form := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}, "variant": {"sensors"}}
	resp, body := e.do(t, "POST", "/updates/preview", "operator", form)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "confirm") {
		t.Fatalf("got %d: %s", resp.StatusCode, body)
	}
	if len(o.previewReqs) != 1 || !o.previewReqs[0].AllowVariantChange || o.previewReqs[0].Variant != "sensors" {
		t.Fatalf("request: %+v", o.previewReqs)
	}
}

// Why: a device stuck on minimal has no variant of its own, so the UI must
// prefill the default build (and allow that change) instead of failing in the guard.
func TestMinimalDeviceGetsDefaultVariant(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	_, body := e.do(t, "POST", "/updates/preview", "operator", url.Values{"id": {"3", "1"}, "release": {"16.1.0"}, "policy": {"only-newer"}})
	byID := map[model.DeviceID]ota.Request{}
	for _, r := range o.previewReqs {
		byID[r.DeviceID] = r
	}
	if r := byID[3]; r.Variant != "tasmota" || !r.AllowVariantChange {
		t.Fatalf("minimal device: %+v", r)
	}
	if r := byID[1]; r.Variant != "" || r.AllowVariantChange {
		t.Fatalf("normal device must keep its variant: %+v", r)
	}
	if !strings.Contains(body, "installs the default tasmota build") {
		t.Fatal("preview must say that the build changes")
	}
}

func TestSubmitRedirectsToBatchAndShowsRefusals(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	form := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"reinstall"}, "skip_backup": {"1"}}
	resp, _ := e.do(t, "POST", "/updates", "operator", form)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tf/transactions/7" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(o.submitReqs) != 1 || !o.submitReqs[0].SkipBackup || o.submitReqs[0].Policy != model.PolicyReinstall {
		t.Fatalf("%+v", o.submitReqs)
	}
	o.submitErr = &ota.RefusedError{Refused: []ota.Refusal{{DeviceID: 1, Reason: "already has active job 4"}}}
	resp, body := e.do(t, "POST", "/updates", "operator", form)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "already has active job 4") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	o.submitErr = errors.New("boom")
	if resp, _ = e.do(t, "POST", "/updates", "operator", form); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestBatchPageAndCancel(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	resp, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Rebooting") || !strings.Contains(body, "Flashing ok") || !strings.Contains(body, "sse:job") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if resp, _ = e.do(t, "GET", "/transactions/99", "operator", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown batch: %d", resp.StatusCode)
	}
	e.do(t, "POST", "/transactions/7/cancel", "operator", url.Values{})
	e.do(t, "POST", "/jobs/1/cancel", "operator", url.Values{})
	// Batch cancel goes through Engine.CancelBatch (one call cancels every
	// active job of the batch); job cancel through Engine.Cancel.
	if len(o.cancelledBatches) != 1 || o.cancelledBatches[0] != 7 || len(o.cancelled) != 1 || o.cancelled[0] != 1 {
		t.Fatalf("batches %v jobs %v", o.cancelledBatches, o.cancelled)
	}
}

func TestFirmwarePageAndAdminActions(t *testing.T) {
	e, _, _, fw := setupOTA(t)
	if resp, body := e.do(t, "GET", "/firmware", "operator", nil); resp.StatusCode != 200 || !strings.Contains(body, "16.1.0") {
		t.Fatalf("%d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/firmware/refresh", "operator", url.Values{}); resp.StatusCode != 403 {
		t.Fatalf("operators must not refresh: %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "POST", "/firmware/refresh", "admin", url.Values{}); resp.StatusCode != 200 || !fw.refreshed {
		t.Fatalf("admin refresh: %d", resp.StatusCode)
	}
	resp, body := e.do(t, "POST", "/firmware/fetch", "admin", url.Values{"release": {"16.1.0"}, "filename": {"tasmota.bin.gz"}})
	if resp.StatusCode != 200 || !strings.Contains(body, "tasmota.bin.gz 16.1.0 cached") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestUpdateRoutesAbsentWithoutEngine(t *testing.T) {
	e := setup(t)
	if resp, _ := e.do(t, "GET", "/updates/new", "operator", nil); resp.StatusCode != 404 {
		t.Fatalf("without FIRMWARE_BASE_URL the update pages must not exist: %d", resp.StatusCode)
	}
}

// The transactions page must be reachable at exactly /transactions: a trailing-slash redirect would
// drop the base path behind a prefix or HA ingress.
func TestTransactionsPageUnderBasePathWithoutRedirect(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	resp, body := e.do(t, "GET", "/transactions", "operator", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, `href="/tf/transactions"`) {
		t.Fatalf("%d", resp.StatusCode)
	}
}

// Why: the power restore is on by default and the operator must see before
// starting what it would do per device, including the cases where it cannot
// (unreadable state stops the update; PulseTime/Interlock relays are left),
// and the opt-out must reach the engine and survive the preview round trip.
func TestPreviewShowsPowerRestoreAndOptOut(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[1] = ota.PreviewItem{DeviceID: 1, Decision: "upgrade", Plan: model.PlanStaged, Filename: "tasmota.bin.gz", PowerRestore: "1, 3", PowerSkipped: "2"}
	o.items[2] = ota.PreviewItem{DeviceID: 2, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota32.bin", PowerUnreadable: true}
	form := url.Values{"id": {"1", "2"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	_, body := e.do(t, "POST", "/updates/preview", "operator", form)
	for _, want := range []string{"relay 1, 3 on again", "relay 2 left alone", "relay state unreadable"} {
		if !strings.Contains(strings.ToLower(body), want) {
			t.Fatalf("preview lacks %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `name="power_after" value=""`) || o.previewReqs[0].SkipPowerRestore {
		t.Fatal("the default must restore lights and carry no opt-out")
	}
	form.Set("power_after", "leave")
	_, body = e.do(t, "POST", "/updates/preview", "operator", form)
	if !strings.Contains(body, `name="power_after" value="leave"`) || !o.previewReqs[0].SkipPowerRestore {
		t.Fatalf("opt-out lost: %+v", o.previewReqs)
	}
}

// Why: relays keep their state across an update on this fleet, so restoring
// them is an explicit choice; the preview must say which relays are left and
// the choice must reach the engine and survive the preview round trip.
func TestPreviewRelaysOnlyOnRequest(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[1] = ota.PreviewItem{DeviceID: 1, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota.bin.gz", PowerRestore: "2", PowerRelaysKept: "1"}
	form := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	_, body := e.do(t, "POST", "/updates/preview", "operator", form)
	if !strings.Contains(strings.ToLower(body), "relay 1 not restored (relays keep their state)") {
		t.Fatalf("default preview: %s", body)
	}
	if o.previewReqs[0].RestoreRelays {
		t.Fatal("relays must not be restored unless asked")
	}
	form.Set("power_after", "lights-relays")
	_, body = e.do(t, "POST", "/updates/preview", "operator", form)
	if !o.previewReqs[0].RestoreRelays || o.previewReqs[0].SkipPowerRestore || !strings.Contains(body, `name="power_after" value="lights-relays"`) {
		t.Fatalf("opt-in lost: %+v", o.previewReqs)
	}
}

// Why: with two checkboxes "restore relays" together with "skip the restore"
// was postable and meaningless. One select makes that impossible: every value
// maps to exactly one valid engine combination and anything else is refused.
func TestPowerAfterMapsToValidEngineOptions(t *testing.T) {
	cases := []struct {
		in          string
		skip, relay bool
		bad         bool
	}{
		{"", false, false, false}, {"lights", false, false, false},
		{"lights-relays", false, true, false}, {"leave", true, false, false},
		{"relays", false, false, true}, {"leave,lights-relays", false, false, true},
	}
	for _, c := range cases {
		skip, relay, err := powerAfter(c.in)
		if (err != nil) != c.bad || skip != c.skip || relay != c.relay {
			t.Errorf("powerAfter(%q) = %v %v %v", c.in, skip, relay, err)
		}
		if skip && relay {
			t.Errorf("powerAfter(%q) produced the invalid combination", c.in)
		}
	}
}

type retryCall struct {
	actor   string
	batch   model.BatchID
	devices []model.DeviceID
}

func (f *fakeOTA) SubmitRetry(_ context.Context, actor string, batch model.BatchID, devices []model.DeviceID) (ota.RetryResult, error) {
	f.retryCalls = append(f.retryCalls, retryCall{actor, batch, devices})
	return f.retryRes, f.retryErr
}

// Why: a finished transaction with failed devices offers "Retry failed" and a
// per-device retry; both lead to a confirmation page first (a GET creates
// nothing), the POST asks the engine for a NEW transaction linked to the old
// one, and the links show the relation in both directions.
func TestTransactionRetryFlow(t *testing.T) {
	e, o, j, _ := setupOTA(t)
	j.jobs = []model.Job{
		{ID: 1, BatchID: 7, DeviceID: 1, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: time.Now()},
		{ID: 2, BatchID: 7, DeviceID: 2, State: model.JobFailed, TargetRelease: "16.1.0", LastError: "select: fwselect: unknown hardware, refusing to guess", CreatedAt: time.Now()},
		{ID: 3, BatchID: 8, DeviceID: 2, State: model.JobSucceeded, TargetRelease: "16.1.0", CreatedAt: time.Now()},
	}
	_, body := e.do(t, "GET", "/transactions/7", "operator", nil)
	for _, want := range []string{"Retry failed", `/transactions/7/retry?id=2`, "Retried in", `href="/tf/transactions/8"`, `href="/tf/transactions/9"`, "1 done · 1 failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page lacks %q", want)
		}
	}
	_, body = e.do(t, "GET", "/transactions/8", "operator", nil)
	if !strings.Contains(body, "Retry of") || !strings.Contains(body, `href="/tf/transactions/7"`) || strings.Contains(body, "Retry failed") {
		t.Errorf("a finished clean retry links back and offers no retry: %s", body)
	}
	resp, body := e.do(t, "GET", "/transactions/7/retry", "operator", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "Start retry of 1 device") || len(o.retryCalls) != 0 {
		t.Fatalf("the confirmation page must not retry: %d %v", resp.StatusCode, o.retryCalls)
	}
	o.retryRes = ota.RetryResult{Batch: 10, Devices: []model.DeviceID{2}}
	resp, _ = e.do(t, "POST", "/transactions/7/retry", "operator", url.Values{"csrf": {"tok"}, "id": {"2"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/tf/transactions/10" || len(o.retryCalls) != 1 || o.retryCalls[0].batch != 7 || len(o.retryCalls[0].devices) != 1 {
		t.Fatalf("%d %q %+v", resp.StatusCode, resp.Header.Get("Location"), o.retryCalls)
	}
	o.retryErr = ota.ErrBatchRunning
	if resp, _ = e.do(t, "POST", "/transactions/7/retry", "operator", url.Values{"csrf": {"tok"}}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("running batch: %d", resp.StatusCode)
	}
	o.retryErr = &ota.RefusedError{Refused: []ota.Refusal{{DeviceID: 2, Reason: "already has active job 4"}}}
	if _, body = e.do(t, "POST", "/transactions/7/retry", "operator", url.Values{"csrf": {"tok"}}); !strings.Contains(strings.ToLower(body), "already has active job 4") {
		t.Fatalf("refusal reason: %s", body)
	}
}

// Why: starting an update changes devices; the audit trail must say who started
// which transaction with how many devices (the audit page's "Start update" row).
func TestUpdateSubmitIsAudited(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	e.do(t, "POST", "/updates", "operator", url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}})
	for _, ev := range e.aud.events {
		if ev.Action == "transaction.start" && strings.HasPrefix(ev.Target, "transaction:") && strings.Contains(ev.Detail, `"devices":1`) {
			return
		}
	}
	t.Fatalf("no transaction.start in %+v", e.aud.events)
}

// Why : the dashboard's transactions listed one row per device, so #4 showed five times
// while it ran. A transaction is one row: number, target, how its devices stand, who started it. The
// target is the release alone: the build variant differs per device and made the same release read
// "16.1.0" in one row and "16.1.0 tasmota" in the next.
func TestDashboardListsOneRowPerTransaction(t *testing.T) {
	job := func(id model.JobID, batch model.BatchID, st model.JobState, variant string) model.Job {
		return model.Job{ID: id, BatchID: batch, DeviceID: 1, State: st, TargetRelease: "16.1.0", TargetVariant: variant, CreatedBy: "adam"}
	}
	e := setupWith(t, func(d *Deps) {
		d.Jobs = &fakeJobs{jobs: []model.Job{ // newest first
			job(9, 5, model.JobSucceeded, "tasmota"), job(8, 5, model.JobFailed, ""), job(7, 5, model.JobSucceeded, "tasmota"),
			job(3, 4, model.JobSucceeded, ""),
		}}
	})
	_, body := e.do(t, "GET", "/", "operator", nil)
	if n := strings.Count(body, `href="/tf/transactions/5"`); n != 1 {
		t.Errorf("transaction 5 appears %d times, want one row", n)
	}
	if n := strings.Count(body, `href="/tf/transactions/4"`); n != 1 {
		t.Errorf("transaction 4 appears %d times, want one row", n)
	}
	for _, want := range []string{"2 of 3 done · 1 failed", "Update to Tasmota 16.1.0", ">Completed<"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(body, "16.1.0 tasmota") {
		t.Error("the variant leaked into the target")
	}
}

// Why: the dashboard shows only the latest few transactions, not every one that ever ran.
func TestGroupTransactionsKeepsOrderAndCounts(t *testing.T) {
	now := time.Now()
	jobs := []model.Job{
		{ID: 5, BatchID: 2, State: model.JobQueued, CreatedAt: now},
		{ID: 4, BatchID: 2, State: model.JobSucceeded, CreatedAt: now},
		{ID: 3, BatchID: 1, State: model.JobCancelled, CreatedAt: now},
	}
	got := groupTransactions(jobs)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("order: %+v", got)
	}
	if got[0].Summary() != "1 of 2 succeeded, 1 waiting" || got[0].Outcome != "Running" {
		t.Errorf("batch 2: %q %q", got[0].Summary(), got[0].Outcome)
	}
	if got[1].Summary() != "0 of 1 succeeded, 1 cancelled" || got[1].Outcome != "Cancelled" {
		t.Errorf("batch 1: %q %q", got[1].Summary(), got[1].Outcome)
	}
}

// Why: the delivery is a per-transaction choice. It must reach the engine,
// survive the preview round trip (the hidden field), default to the download,
// and a made-up value must be refused instead of silently falling back.
func TestDeliveryChoiceReachesTheEngineAndSurvivesPreview(t *testing.T) {
	e, o, _, _ := setupOTA(t)
	o.items[1] = ota.PreviewItem{DeviceID: 1, Decision: "upgrade", Plan: model.PlanDirect, Filename: "tasmota32.bin"}
	form := url.Values{"id": {"1"}, "release": {"16.1.0"}, "policy": {"only-newer"}}
	_, body := e.do(t, "POST", "/updates/preview", "operator", form)
	if o.previewReqs[0].Delivery != model.DeliveryPull || !strings.Contains(body, `name="delivery" value=""`) {
		t.Fatalf("default delivery must be the download: %+v", o.previewReqs[0])
	}
	form.Set("delivery", "push")
	_, body = e.do(t, "POST", "/updates/preview", "operator", form)
	if o.previewReqs[0].Delivery != model.DeliveryPush || !strings.Contains(body, `name="delivery" value="push"`) || !strings.Contains(body, "uploads each file to its device") {
		t.Fatalf("push lost: %+v\n%s", o.previewReqs[0], body)
	}
	form.Set("delivery", "ftp")
	resp, body := e.do(t, "POST", "/updates/preview", "operator", form)
	if resp.StatusCode == 200 || !strings.Contains(body, "unknown delivery") {
		t.Fatalf("an unknown delivery was accepted: %d %s", resp.StatusCode, body)
	}
}

// Why: the form offers the choice; the page must carry the select with the
// download as the default so an untouched form behaves as before.
func TestUpdateFormOffersTheDeliveryChoice(t *testing.T) {
	e, _, _, _ := setupOTA(t)
	_, body := e.do(t, "GET", "/updates/new", "operator", nil)
	if !strings.Contains(body, `name="delivery"`) || !strings.Contains(body, `<option value="" selected>Pull`) || !strings.Contains(body, `value="push"`) {
		t.Fatalf("no delivery select: %s", body)
	}
}
