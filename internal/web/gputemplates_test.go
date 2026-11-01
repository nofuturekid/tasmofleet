package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/templates"
)

// fakeTemplates is the service of the GPIO templates tab with canned answers. It records what the
// pages asked for, because the pages' job is to pass the person's choice on unchanged.
type fakeTemplates struct {
	ov        templates.Overview
	prev      templates.Preview
	applyErr  error
	refusals  []templates.Refusal
	gotRefs   []templates.Refs
	refreshed bool
	gotPrev   *templates.Request
	gotApply  *templates.Request
	gotDigest string
	actor     string
}

func (f *fakeTemplates) Overview(_ context.Context, refs templates.Refs, refresh bool) (templates.Overview, error) {
	f.gotRefs = append(f.gotRefs, refs)
	f.refreshed = f.refreshed || refresh
	return f.ov, nil
}

func (f *fakeTemplates) Preview(_ context.Context, req templates.Request) (templates.Preview, error) {
	f.gotPrev = &req
	return f.prev, nil
}

func (f *fakeTemplates) Apply(_ context.Context, actor string, req templates.Request, digest string) (model.BatchID, []templates.Refusal, error) {
	f.gotApply, f.gotDigest, f.actor = &req, digest, actor
	if f.applyErr != nil {
		return 0, nil, f.applyErr
	}
	return 42, f.refusals, nil
}

// gpio8266 is a 14-slot ESP8266 template: relay on GPIO14 (slot 13 is ADC0... slot 12 = GPIO14).
func gpio8266(relaySlot int) []int {
	g := make([]int, 14)
	g[0] = 32*0 + 1 // User on GPIO0: shown as "User"
	g[relaySlot] = 224
	return g
}

func tdev(id model.DeviceID, name string, tpl policy.GPIOTemplate, live bool) templates.DeviceView {
	t := tpl
	return templates.DeviceView{ID: id, Name: name, Family: model.ChipESP8266, Source: templates.SourceLive, ReadAt: time.Date(2026, 11, 1, 15, 58, 0, 0, time.UTC),
		Reading: policy.TemplateDevice{ID: id, Family: model.ChipESP8266, Live: live, Module: &policy.ModuleReading{Number: 0, Name: "Generic"}, Template: &t}}
}

// gtFixture: three plugs share "Gosund SP111"; two have the reference layout, "balcony-plug" has its
// relay on another slot. attic is not judged.
func gtFixture() *fakeTemplates {
	ref := policy.GPIOTemplate{Name: "Gosund SP111", GPIO: gpio8266(12), Base: 45}
	odd := policy.GPIOTemplate{Name: "Gosund SP111", GPIO: gpio8266(11), Base: 45}
	a, b, c := tdev(10, "tv-plug", ref, true), tdev(11, "router-plug", ref, true), tdev(12, "balcony-plug", odd, true)
	key := policy.GroupKey{Family: model.ChipESP8266, Name: "Gosund SP111"}
	refr := policy.Reference{Device: a.Reading, How: policy.RefMostCommon, Count: 2, Of: 3}
	diff, _ := policy.DiffTemplates(model.ChipESP8266, odd, ref)
	a.Plan = &policy.ApplyPlan{Device: 10, Matches: true}
	b.Plan = &policy.ApplyPlan{Device: 11, Matches: true}
	c.Plan = &policy.ApplyPlan{Device: 12, Diff: diff, Commands: []string{`Template {"NAME":"Gosund SP111"}`}, Restarts: true}
	return &fakeTemplates{ov: templates.Overview{
		Groups:    []templates.GroupView{{Key: key, Devices: []templates.DeviceView{a, b, c}, Reference: &refr}},
		NotJudged: []templates.DeviceView{{ID: 13, Name: "attic-plug", NotJudgedReason: "Offline, no backup yet."}},
		ReadAt:    time.Date(2026, 11, 1, 15, 58, 0, 0, time.UTC),
	}, prev: templates.Preview{
		Group: key, Reference: a, ReferenceTemplate: ref, Digest: "abc123",
		Rows: []templates.PreviewRow{{Device: func() templates.DeviceView {
			d := c
			d.PowerOnStateText = "After the restart it follows PowerOnState 1 (on)."
			return d
		}(), Plan: *c.Plan}},
		Excluded:   []templates.Excluded{{Device: b, Why: "matches the reference"}},
		Applicable: 1, BackupLine: "A backup is taken first; nothing is sent if it fails.", RestartLine: "Their relays can switch while the pins change.",
	}}
}

func gtEnv(t *testing.T, f *fakeTemplates) *env {
	t.Helper()
	return setupWith(t, func(d *Deps) {
		d.Policy = &fakePolicy{rep: fleetReport()}
		d.Templates = f
	})
}

// Why: only an admin may change devices. An operator reads the comparison but is offered no Apply,
// no tick box, and the preview refuses them even when the form is posted by hand.
func TestGPIOTemplatesTabIsReadOnlyForOperators(t *testing.T) {
	fake := gtFixture()
	e := gtEnv(t, fake)
	r, body := e.do(t, "GET", "/policy/templates", "operator", nil)
	if r.StatusCode != 200 || !strings.Contains(body, "balcony-plug") || !strings.Contains(body, "Differs") {
		t.Fatalf("operator tab: %d\n%s", r.StatusCode, body)
	}
	if strings.Contains(body, "Apply reference to") || strings.Contains(body, `name="pick"`) {
		t.Error("an operator is offered Apply")
	}
	r, _ = e.do(t, "POST", "/policy/templates/preview", "operator", url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "pick": {"12"}})
	if r.StatusCode != 403 {
		t.Errorf("operator preview: %d, want 403", r.StatusCode)
	}
	// the preview changes nothing, so "Check again…" can link to it; it is still admin-only
	if r, _ := e.do(t, "GET", "/policy/templates/preview?family=esp8266&name=Gosund+SP111&ref=10&pick=12", "operator", nil); r.StatusCode != 403 {
		t.Errorf("operator GET preview: %d, want 403", r.StatusCode)
	}
	if r, body := e.do(t, "GET", "/policy/templates/preview?family=esp8266&name=Gosund+SP111&ref=10&how=chosen&pick=12", "admin", nil); r.StatusCode != 200 || !strings.Contains(body, "Back up, apply and restart 1 device") {
		t.Errorf("admin GET preview: %d\n%s", r.StatusCode, body)
	}
	f := fake
	r, _ = e.do(t, "POST", "/policy/templates/apply", "operator", url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "pick": {"12"}, "confirm": {"1"}, "digest": {"abc123"}})
	if r.StatusCode != 403 || f.gotApply != nil {
		t.Errorf("operator apply: %d, applied=%v, want 403 and nothing applied", r.StatusCode, f.gotApply != nil)
	}
	_, adm := e.do(t, "GET", "/policy/templates", "admin", nil)
	for _, want := range []string{"Apply reference to 1…", `name="pick" value="12"`, "most common, 2 of 3 read live", "Not judged", "Offline, no backup yet.", "GPIO templates"} {
		if !strings.Contains(adm, want) {
			t.Errorf("admin tab misses %q", want)
		}
	}
}

// Why: a timestamp that can be older than today must carry its date (maintainer rule 2026-11-01).
// "Read live" and the backup dates use the dated stamp, never the bare clock.
func TestGPIOTemplatesTimestampsCarryDates(t *testing.T) {
	e := gtEnv(t, gtFixture())
	_, body := e.do(t, "GET", "/policy/templates", "admin", nil)
	if !strings.Contains(body, `data-ts="short"`) {
		t.Error("no dated stamp on the page")
	}
	for _, bad := range []string{`data-ts="time"`, `data-ts="clock-day"`} {
		if strings.Contains(body[strings.Index(body, `gt-bar`):], bad) {
			t.Errorf("a bare clock stamp %s on the tab", bad)
		}
	}
}

// Why: the reference is the person's choice each time (decision Q3): ?ref goes to the service as
// given for the group the device belongs to, and an id that is in no group is dropped, so a forged
// link cannot name a reference the service never offered.
func TestGPIOTemplatesChosenReferenceIsPassedAndForgedIsDropped(t *testing.T) {
	f := gtFixture()
	e := gtEnv(t, f)
	e.do(t, "GET", "/policy/templates?ref=11&ref=999&ref=x", "admin", nil)
	last := f.gotRefs[len(f.gotRefs)-1]
	key := policy.GroupKey{Family: model.ChipESP8266, Name: "Gosund SP111"}
	if len(last) != 1 || last[key] != 11 {
		t.Fatalf("refs passed: %v, want only device 11 for the group", last)
	}
}

// Why: devices restart and relays may switch, so the preview needs the person's tick (decision on
// #327). Without it nothing is applied, however the form arrives.
func TestGPIOTemplatesApplyNeedsTheTick(t *testing.T) {
	f := gtFixture()
	e := gtEnv(t, f)
	form := url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "how": {"most-common"}, "pick": {"12"}, "digest": {"abc123"}}
	r, body := e.do(t, "POST", "/policy/templates/apply", "admin", form)
	if r.StatusCode != 422 || f.gotApply != nil || !strings.Contains(body, "Tick the box") {
		t.Fatalf("untouched form: %d, applied=%v\n%s", r.StatusCode, f.gotApply != nil, body)
	}
	form.Set("confirm", "1")
	r, _ = e.do(t, "POST", "/policy/templates/apply", "admin", form)
	if r.StatusCode != 303 || !strings.HasSuffix(r.Header.Get("Location"), "/transactions/42") {
		t.Fatalf("ticked form: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
	if f.gotApply == nil || f.gotApply.Reference != 10 || len(f.gotApply.Devices) != 1 || f.gotApply.Devices[0] != 12 || f.gotDigest != "abc123" || f.actor != "test:admin" {
		t.Fatalf("apply request %+v digest %q actor %q", f.gotApply, f.gotDigest, f.actor)
	}
}

// Why: the preview is the only look at the exact command before devices restart: it names the
// command, the backup, the restart with PowerOnState, and why the other devices are left out.
func TestGPIOTemplatesPreviewShowsCommandBackupAndRestart(t *testing.T) {
	f := gtFixture()
	e := gtEnv(t, f)
	r, body := e.do(t, "POST", "/policy/templates/preview", "admin", url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "how": {"chosen"}, "pick": {"12", "11"}})
	if r.StatusCode != 200 {
		t.Fatalf("%d\n%s", r.StatusCode, body)
	}
	for _, want := range []string{"Apply the Gosund SP111 template to 1 device", `Template {&#34;NAME&#34;:&#34;Gosund SP111&#34;}`, "A backup is taken first; nothing is sent if it fails.",
		"Restarts once.", "PowerOnState 1 (on)", "Their relays can switch", "matches the reference", `name="confirm"`, `required`, `name="digest" value="abc123"`, "Chosen:"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview misses %q", want)
		}
	}
	// Apply judges the same request as the preview, or the digest (which covers the excluded devices) cannot match.
	if !strings.Contains(body, `name="pick" value="11"`) || !strings.Contains(body, `name="pick" value="12"`) {
		t.Error("the apply form does not carry every device of the previewed request")
	}
	if f.gotPrev == nil || len(f.gotPrev.Devices) != 2 {
		t.Errorf("preview request %+v", f.gotPrev)
	}
	if f.gotApply != nil {
		t.Error("a preview must never apply")
	}
}

// Why: when a device changed between preview and apply the person must see the new plan, not get
// a transaction nobody looked at.
func TestGPIOTemplatesStaleDigestShowsAFreshPreview(t *testing.T) {
	f := gtFixture()
	f.applyErr = templates.ErrStaleDigest
	e := gtEnv(t, f)
	r, body := e.do(t, "POST", "/policy/templates/apply", "admin", url.Values{"csrf": {"tok"}, "family": {"esp8266"}, "name": {"Gosund SP111"}, "ref": {"10"}, "pick": {"12"}, "confirm": {"1"}, "digest": {"old"}})
	if r.StatusCode != 409 || !strings.Contains(body, "changed since you looked") || !strings.Contains(body, `name="digest" value="abc123"`) {
		t.Fatalf("stale: %d\n%s", r.StatusCode, body)
	}
}

// Why: a group with a tie must preselect nothing: no Apply, variants named, and the choice link on
// every live device.
func TestGPIOTemplatesGroupWithoutReferenceAsksForOne(t *testing.T) {
	f := gtFixture()
	f.ov.Groups[0].Reference, f.ov.Groups[0].NeedsReference = nil, true
	for i := range f.ov.Groups[0].Devices {
		f.ov.Groups[0].Devices[i].Plan = nil
	}
	e := gtEnv(t, f)
	_, body := e.do(t, "GET", "/policy/templates", "admin", nil)
	for _, want := range []string{"Choose a reference.", "Variant A", "Variant B", "Use as reference", "?ref=10"} {
		if !strings.Contains(body, want) {
			t.Errorf("misses %q", want)
		}
	}
	if strings.Contains(body, "Apply reference to") {
		t.Error("Apply offered without a reference")
	}
}
