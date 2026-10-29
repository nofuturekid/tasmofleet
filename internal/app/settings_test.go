package app

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
)

// Why: the shipped example is what users deploy; it must parse with the
// same checks the server applies at start, and carry the ESP32 exception.
func TestPolicyExampleParses(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/unraid/policy.example.json")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := policy.ParseRules(raw, settings.Queryable)
	if err != nil {
		t.Fatal(err)
	}
	devs := []model.Device{
		{ID: 1, Name: "plug", Identity: model.Identity{Family: model.ChipESP8266}},
		{ID: 2, Name: "quinled", Identity: model.Identity{Family: model.ChipESP32}},
	}
	ok := map[string]string{"PowerOnState": "0", "SaveData": "0", "Timezone": "99"}
	v1, v2 := map[string]string{"SetOption0": "0"}, map[string]string{"SetOption0": "1"}
	for k, v := range ok {
		v1[k], v2[k] = v, v
	}
	if fs := policy.Check(rules, devs, map[model.DeviceID]map[string]string{1: v1, 2: v2}); len(fs) != 0 {
		t.Fatalf("the example fleet must be compliant: %+v", fs)
	}
}

type fakeLister []model.Device

func (f fakeLister) ListDevices(context.Context) ([]model.Device, error) { return f, nil }

type fakeBackupSvc struct {
	list map[model.DeviceID][]model.Backup
	data map[model.BackupID]string
}

func (f fakeBackupSvc) Backup(context.Context, string, model.DeviceID) ([]model.Backup, error) {
	return nil, errors.New("unused")
}
func (f fakeBackupSvc) List(_ context.Context, id model.DeviceID) ([]model.Backup, error) {
	return f.list[id], nil
}
func (f fakeBackupSvc) Open(_ context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	return []byte(f.data[id]), model.Backup{}, nil
}

type fakeReader struct {
	mu    sync.Mutex
	asked map[model.DeviceID][]string
}

func (f *fakeReader) ReadKeys(_ context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	f.mu.Lock()
	f.asked[id] = keys
	f.mu.Unlock()
	if id == 3 {
		return nil, errors.New("down")
	}
	return map[string]string{"SaveData": "0"}, nil
}

// Why: live values are primary (backups are stale). Per setting, a live
// value wins; a backup only fills a setting with no live value and is
// labelled as such; a dead device stays "unknown" rather than failing the page.
func TestPolicyReporterLiveFirstBackupFallback(t *testing.T) {
	now := time.Now()
	rules := []policy.Rule{{Command: "SaveData", Want: "0"}, {Command: "PowerOnState", Want: "0"}}
	rd := &fakeReader{asked: map[model.DeviceID][]string{}}
	rep := &policyReporter{
		rules:   rules,
		devices: fakeLister{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}},
		backups: fakeBackupSvc{
			list: map[model.DeviceID][]model.Backup{1: {
				{ID: 10, Kind: model.BackupDecoded, TakenAt: now.Add(-time.Hour)},
				{ID: 11, Kind: model.BackupDecoded, TakenAt: now.Add(-24 * time.Hour)},
				{ID: 12, Kind: model.BackupConfigDump, TakenAt: now},
			}},
			data: map[model.BackupID]string{10: `{"save_data":5,"poweronstate":0}`, 11: `{"save_data":0}`, 12: "dmp"},
		},
		reader: rd, now: func() time.Time { return now },
	}
	rep.vals.put(1, map[string]string{"SaveData": "0"}, now.Add(-5*time.Minute))
	r, err := rep.Report(t.Context(), "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.asked) != 0 {
		t.Fatal("a plain page view must not read devices")
	}
	// device 1: SaveData live 0 (the backup's 5 is stale), PowerOnState from the newest backup
	p1 := r.Provenance[1]
	if p1["savedata"].Kind != "live" || !p1["savedata"].At.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("SaveData provenance %+v", p1)
	}
	if p1["poweronstate"].Kind != "backup" || !p1["poweronstate"].At.Equal(now.Add(-time.Hour)) {
		t.Fatalf("PowerOnState provenance %+v", p1)
	}
	for _, f := range r.Findings {
		if f.DeviceID == 1 {
			t.Errorf("device 1 is compliant on live+backup values: %+v", f)
		}
	}
	// device 2 has nothing: unknown for both rules
	n := 0
	for _, f := range r.Findings {
		if f.DeviceID == 2 && f.Unknown {
			n++
		}
	}
	if n != 2 || len(r.Provenance[2]) != 0 {
		t.Fatalf("device 2: %+v prov %v", r.Findings, r.Provenance[2])
	}
}

// Why: a rule on a setting that cannot be read without side effects must say
// so instead of looking like missing data.
func TestPolicyReportMarksNotReadableLive(t *testing.T) {
	rep := &policyReporter{rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, devices: fakeLister{{ID: 1, Name: "a"}}, backups: fakeBackupSvc{}}
	r, _ := rep.Report(t.Context(), "x", false)
	if r.NotLive["savedata"] {
		t.Error("SaveData is readable live through Status")
	}
	if settings.LiveReadable("Reset") || !settings.LiveReadable("SaveData") {
		t.Error("LiveReadable contract")
	}
}

type countingReader struct {
	mu    sync.Mutex
	calls int
}

func (c *countingReader) ReadKeys(context.Context, model.DeviceID, []string) (map[string]string, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return map[string]string{"SaveData": "0"}, nil
}

type auditSink struct{ events []model.AuditEvent }

func (a *auditSink) Record(_ context.Context, e model.AuditEvent) { a.events = append(a.events, e) }

// Why: the on-demand check reaches every device; an operator pressing the
// button in a loop must not turn that into fleet-wide polling.
func TestPolicyLiveCheckIsRateLimitedAndAudited(t *testing.T) {
	clock := time.Now()
	rd, aud := &countingReader{}, &auditSink{}
	rep := &policyReporter{
		rules:   []policy.Rule{{Command: "SaveData", Want: "0"}},
		devices: fakeLister{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}},
		backups: fakeBackupSvc{}, reader: rd, audit: aud, now: func() time.Time { return clock },
	}
	r, err := rep.Report(t.Context(), "alice", true)
	if err != nil || rd.calls != 2 || r.LiveThrottled || r.Provenance[1]["savedata"].Kind != "live" {
		t.Fatalf("first run: calls %d throttled %v err %v", rd.calls, r.LiveThrottled, err)
	}
	clock = clock.Add(10 * time.Second)
	r, _ = rep.Report(t.Context(), "alice", true)
	if rd.calls != 2 || !r.LiveThrottled || len(r.Findings) != 0 {
		t.Fatalf("second run must use the cache: calls %d throttled %v", rd.calls, r.LiveThrottled)
	}
	clock = clock.Add(2 * time.Minute) // past the global gap, inside the device cooldown
	if _, _ = rep.Report(t.Context(), "alice", true); rd.calls != 2 {
		t.Fatalf("per-device cooldown: calls %d", rd.calls)
	}
	clock = clock.Add(10 * time.Minute)
	if _, _ = rep.Report(t.Context(), "alice", true); rd.calls != 4 {
		t.Fatalf("after the cooldown the devices are read again: calls %d", rd.calls)
	}
	if len(aud.events) != 4 || aud.events[0].Action != "policy.live" || aud.events[0].Actor != "alice" {
		t.Fatalf("every live request is audited: %+v", aud.events)
	}
}

// Why: a rule on a secret would make every live read fail silently; it must
// stop the start instead.
func TestPolicyRulesRefuseSecretsLoudly(t *testing.T) {
	for _, cmd := range []string{"MqttPassword", "Reset", "Restart"} {
		in := `[{"command":"` + cmd + `","want":"x"}]`
		if _, err := policy.ParseRules([]byte(in), settings.Queryable); err == nil {
			t.Errorf("%s accepted", cmd)
		}
	}
}

// Why: saving a rule must refuse a SetOption32..49 value a one-byte parameter
// cannot hold, instead of failing only when Make compliant sends it.
func TestValidateWantSetOptionParameter(t *testing.T) {
	if err := validateWant("SetOption45", "40"); err != nil {
		t.Errorf("SetOption45 40: %v", err)
	}
	if err := validateWant("SetOption45", "256"); err == nil {
		t.Error("SetOption45 256 must be refused")
	}
}
