package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// sideEffectGuard is a device client that fails the test when a command with a
// side effect goes out without a value: a bare SaveData calls
// SettingsSaveAll() (flash write), a bare TelePeriod forces a telemetry
// publish. The Status blocks answer like a plug.
type sideEffectGuard struct {
	t    *testing.T
	mu   sync.Mutex
	sent []string
}

func (g *sideEffectGuard) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	g.mu.Lock()
	g.sent = append(g.sent, cmnd)
	g.mu.Unlock()
	switch cmnd {
	case "SaveData", "TelePeriod":
		g.t.Errorf("side-effect query sent bare: %q", cmnd)
		return nil, errors.New("forbidden")
	case "Status":
		return json.RawMessage(`{"Status":{"PowerOnState":0,"SaveData":0,"SaveState":0}}`), nil
	case "Status 3":
		return json.RawMessage(`{"StatusLOG":{"TelePeriod":300}}`), nil
	case "Timezone":
		return json.RawMessage(`{"Timezone":99}`), nil
	}
	return nil, errUnknownCmd
}

var errUnknownCmd = tasmota.ErrUnknownCommand

func (*sideEffectGuard) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, nil
}
func (*sideEffectGuard) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

type devGetter struct{}

func (devGetter) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id, Address: "10.0.0.5"}, nil
}

// The example fleet policy through the real settings service and the guard.
func flashWear() []policy.Rule {
	return []policy.Rule{
		{Command: "PowerOnState", Want: "0"}, {Command: "SetOption0", Want: "0"},
		{Command: "SaveData", Want: "0"}, {Command: "TelePeriod", Want: "300"}, {Command: "Timezone", Want: "99"},
	}
}

// Why: the poller reads every device every hour. If any path sent a bare
// SaveData, the whole fleet would write flash each hour.
func TestPollerNeverSendsSideEffectQueries(t *testing.T) {
	g := &sideEffectGuard{t: t}
	rep := &policyReporter{
		rules: flashWear(), devices: fakeLister{{ID: 1, Name: "a"}},
		backups: fakeBackupSvc{}, reader: settings.New(settings.Options{Devices: devGetter{}, Client: g}),
	}
	if !rep.readDevice(t.Context(), 1) {
		t.Fatal("read failed")
	}
	vals := rep.vals.get(1)
	for cmd, want := range map[string]string{"PowerOnState": "0", "SetOption0": "0", "SaveData": "0", "TelePeriod": "300", "Timezone": "99"} {
		if vals[cmd].value != want {
			t.Errorf("%s = %q, want %q", cmd, vals[cmd].value, want)
		}
	}
	if len(g.sent) != 3 { // Status, Status 3, Timezone: one per block
		t.Errorf("sent %v", g.sent)
	}
	// and a report built from it is compliant, with live provenance
	r, _ := rep.Report(t.Context(), "x", false)
	if len(r.Findings) != 0 {
		t.Fatalf("%+v", r.Findings)
	}
}

// Why: reads are spread out in time and skip offline devices, otherwise a
// restart makes every device answer at once and a dead one is hammered.
func TestPollerSpreadsAndSkipsOffline(t *testing.T) {
	now := time.Now()
	clock := now
	rd := &countingReader{}
	rep := &policyReporter{
		rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, backups: fakeBackupSvc{}, reader: rd,
		devices: fakeLister{{ID: 1, Name: "a"}, {ID: 2, Name: "off"}}, now: func() time.Time { return clock },
	}
	j := 0.5
	p := &policyPoller{rep: rep, interval: func() time.Duration { return time.Hour }, jitter: func() float64 { return j },
		offline: func(id model.DeviceID) bool { return id == 2 }}
	p.once(t.Context()) // first sight: only schedules (inside the first window)
	if rd.calls != 0 {
		t.Fatalf("nothing is read at start, got %d", rd.calls)
	}
	if got := p.due[1].Sub(now); got != 5*time.Minute {
		t.Fatalf("first read due in %v, want the middle of the 10 min window", got)
	}
	clock = clock.Add(6 * time.Minute)
	p.once(t.Context())
	if rd.calls != 1 { // device 1 only; 2 is offline
		t.Fatalf("calls %d", rd.calls)
	}
	if next := p.due[1].Sub(clock); next != time.Hour { // jitter 0.5 -> factor 1.0
		t.Fatalf("next read in %v", next)
	}
	if next := p.due[2].Sub(clock); next != pollRetryAfter {
		t.Fatalf("an offline device is looked at again in %v", next)
	}
	clock = clock.Add(30 * time.Minute)
	p.once(t.Context())
	if rd.calls != 1 {
		t.Fatalf("not due yet, calls %d", rd.calls)
	}
	j = 0
	clock = clock.Add(31 * time.Minute)
	p.once(t.Context())
	if rd.calls != 2 || p.due[1].Sub(clock) != 48*time.Minute { // 0.8 * 1h
		t.Fatalf("calls %d next %v", rd.calls, p.due[1].Sub(clock))
	}
}

// Why: MQTT keeps values fresh for free, but only for devices we know and
// settings a rule asks about.
func TestMQTTUpdatesFillTheLiveStore(t *testing.T) {
	rep := &policyReporter{rules: []policy.Rule{{Command: "SetOption0", Want: "0"}}}
	ch := make(chan policyUpdate, 4)
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ch <- policyUpdate{topic: "plug", vals: settings.ParseReply([]byte(`{"SetOption0":"ON"}`)), at: at}
	ch <- policyUpdate{topic: "stranger", vals: map[string]string{"SetOption0": "0"}, at: at}
	ch <- policyUpdate{topic: "plug", vals: map[string]string{"SaveData": "0"}, at: at} // no rule asks
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		rep.applyUpdates(ctx, ch, func(_ context.Context, topic string) (model.Device, error) {
			if topic == "plug" {
				return model.Device{ID: 7}, nil
			}
			return model.Device{}, errors.New("unknown")
		})
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for len(ch) > 0 {
		select {
		case <-deadline:
			t.Fatal("updates not consumed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	v := rep.vals.get(7)
	if len(v) != 1 || v["SetOption0"].value != "1" || !v["SetOption0"].at.Equal(at) {
		t.Fatalf("store %+v", v)
	}
	if len(rep.vals.get(0)) != 0 {
		t.Fatal("a stranger's message was stored")
	}
}

// Why: POLICY_LIVE_INTERVAL is a live system setting; 0 must pause the
// background reads without a restart (and a restored value must resume them).
func TestPollerPausedByZeroInterval(t *testing.T) {
	now := time.Now()
	rd := &countingReader{}
	rep := &policyReporter{
		rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, backups: fakeBackupSvc{}, reader: rd,
		devices: fakeLister{{ID: 1, Name: "a"}}, now: func() time.Time { return now },
	}
	iv := time.Duration(0)
	p := &policyPoller{rep: rep, interval: func() time.Duration { return iv }, jitter: func() float64 { return 0 },
		offline: func(model.DeviceID) bool { return false }}
	p.once(t.Context())
	if len(p.due) != 0 || rd.calls != 0 {
		t.Fatalf("interval 0 must not schedule or read, due=%v calls=%d", p.due, rd.calls)
	}
	iv = time.Hour
	p.once(t.Context())
	if len(p.due) != 1 {
		t.Fatalf("a restored interval must resume scheduling, due=%v", p.due)
	}
}

// Why (N21): Pause on the policy page must stop the background reads at the
// next tick, and Resume must restart them, without a restart of the process.
// The on-demand check (refresh) is not the poller and stays available.
func TestPollerHonoursLivePause(t *testing.T) {
	now := time.Now()
	rd := &countingReader{}
	rep := &policyReporter{
		rules: []policy.Rule{{Command: "SaveData", Want: "0"}}, backups: fakeBackupSvc{}, reader: rd,
		devices: fakeLister{{ID: 1, Name: "a"}}, now: func() time.Time { return now },
	}
	paused := false
	p := &policyPoller{rep: rep, interval: func() time.Duration { return time.Hour }, jitter: func() float64 { return 0 },
		offline: func(model.DeviceID) bool { return false }, paused: func() bool { return paused }}
	p.once(t.Context()) // schedules the device, due now (jitter 0)
	paused = true
	p.once(t.Context())
	if rd.calls != 0 {
		t.Fatalf("a paused check read a device: %d calls", rd.calls)
	}
	paused = false
	p.once(t.Context())
	if rd.calls != 1 {
		t.Fatalf("resume must read the due device: %d calls", rd.calls)
	}
	paused = true
	if throttled := rep.refresh(t.Context(), "admin", []model.Device{{ID: 2, Name: "b"}}); throttled || rd.calls != 2 {
		t.Fatalf("Run now must still read while paused: throttled=%v calls=%d", throttled, rd.calls)
	}
}

// genericDevice answers a few plain settings and a reply that carries credentials. It
// records every command, so a test can see exactly what the poller sent.
type genericDevice struct {
	mu   sync.Mutex
	sent []string
}

func (g *genericDevice) Command(_ context.Context, _ tasmota.Target, cmnd string) (json.RawMessage, error) {
	g.mu.Lock()
	g.sent = append(g.sent, cmnd)
	g.mu.Unlock()
	switch cmnd {
	case "LedState":
		return json.RawMessage(`{"LedState":1}`), nil
	case "Sleep":
		return json.RawMessage(`{"Sleep":{"50":{"Active":"50"}}}`), nil
	case "Baudrate":
		return json.RawMessage(`{"Baudrate":"https://admin:hunter2@host/x"}`), nil
	}
	return nil, tasmota.ErrUnknownCommand
}
func (*genericDevice) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, nil
}
func (*genericDevice) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, nil
}

// Why: the poller reads every rule's command from every device every hour. A generic rule
// is read with its bare name, one command at a time, and the poller must never send what the
// command policy denies: not Status 0 in any spelling, not a secret, not a command that acts.
// A reply with credentials must not reach the values the policy page shows.
func TestPollerReadsGenericRulesAndNeverSendsADeniedCommand(t *testing.T) {
	g := &genericDevice{}
	rules := []policy.Rule{
		{Command: "LedState", Want: "3"}, {Command: "Sleep", Want: "50"}, {Command: "Baudrate", Want: "x"},
		// A rule file written by hand (or by an older version) must still be harmless at read time.
		{Command: "Status0", Want: "1"}, {Command: "status", Want: "1"}, {Command: "MqttPassword", Want: "x"},
		{Command: "Password", Want: "x"}, {Command: "Reset", Want: "1"}, {Command: "Backlog", Want: "x"}, {Command: "Power1", Want: "ON"},
	}
	rep := &policyReporter{
		rules: rules, devices: fakeLister{{ID: 1, Name: "a"}},
		backups: fakeBackupSvc{}, reader: settings.New(settings.Options{Devices: devGetter{}, Client: g}),
	}
	if !rep.readDevice(t.Context(), 1) {
		t.Fatal("read failed")
	}
	if want := []string{"LedState", "Sleep", "Baudrate"}; !reflect.DeepEqual(g.sent, want) {
		t.Fatalf("sent %v, want exactly %v", g.sent, want)
	}
	vals := rep.vals.get(1)
	if vals["LedState"].value != "1" || vals["Sleep"].value != "50" {
		t.Errorf("values %+v", vals)
	}
	if strings.Contains(vals["Baudrate"].value, "hunter2") || vals["Baudrate"].value == "" {
		t.Errorf("credentials in the live value: %q", vals["Baudrate"].value)
	}
	r, _ := rep.Report(t.Context(), "x", false)
	if len(r.NotLive) != 7 {
		t.Errorf("the denied rules must be reported as not readable live, got %v", r.NotLive)
	}
	for _, f := range r.Findings {
		if strings.Contains(f.Have, "hunter2") {
			t.Errorf("credentials in a finding: %+v", f)
		}
	}
}

// Why: devices announce changes on MQTT; a generic rule's value must be kept from there, under the
// spelling of the rule (a device may answer in another case), and a secret's reply never.
func TestMQTTUpdatesKeepGenericKeys(t *testing.T) {
	rep := &policyReporter{rules: []policy.Rule{{Command: "LedState", Want: "3"}, {Command: "Sleep", Want: "50"}}}
	ch := make(chan policyUpdate, 4)
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	ch <- policyUpdate{topic: "plug", vals: settings.ParseReply([]byte(`{"LEDSTATE":"OFF"}`)), at: at}
	ch <- policyUpdate{topic: "plug", vals: settings.ParseReply([]byte(`{"Sleep":{"20":{"Active":"20"}}}`)), at: at}
	ch <- policyUpdate{topic: "plug", vals: settings.ParseReply([]byte(`{"Password":"s3cret"}`)), at: at}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		rep.applyUpdates(ctx, ch, func(context.Context, string) (model.Device, error) { return model.Device{ID: 7}, nil })
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for len(ch) > 0 {
		select {
		case <-deadline:
			t.Fatal("updates not consumed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	v := rep.vals.get(7)
	if len(v) != 2 || v["LedState"].value != "0" || v["Sleep"].value != "20" {
		t.Fatalf("store %+v", v)
	}
}
