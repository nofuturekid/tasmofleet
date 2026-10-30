package policy

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
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type fakeCmd struct {
	mu   sync.Mutex
	sent []string
	err  func(cmd string) error
}

func (f *fakeCmd) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, c)
	if f.err != nil {
		if err := f.err(c); err != nil {
			return nil, err
		}
	}
	return json.RawMessage(`{}`), nil
}

type fakeRead struct {
	vals  map[string]string
	errs  []error // returned one per call before values are served
	calls [][]string
}

func (r *fakeRead) ReadKeys(_ context.Context, _ model.DeviceID, keys []string) (map[string]string, error) {
	r.calls = append(r.calls, keys)
	if len(r.errs) > 0 {
		err := r.errs[0]
		r.errs = r.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := r.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func newFixer(c *fakeCmd, r *fakeRead) *Fixer {
	return &Fixer{Client: c, Reader: r, Wait: func(context.Context, time.Duration) error { return nil }}
}

func count(sent []string, prefix string) int {
	n := 0
	for _, s := range sent {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

// Why: every write to flash wears the chip. The whole batch must end in
// exactly one SaveData that re-sends the current value, never one per command.
func TestApplyBatchesIntoBacklogWithOneSave(t *testing.T) {
	c, r := &fakeCmd{}, &fakeRead{vals: map[string]string{"SaveData": "0"}}
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, []string{"PowerOnState 0", "SetOption0 0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Backlog PowerOnState 0; SetOption0 0", "SaveData 0"}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
}

// Why: SaveData's own handler writes everything, and a restart saves too. An
// extra SaveData would be a second flash write for nothing.
func TestApplyAddsNoSecondSave(t *testing.T) {
	for name, lines := range map[string][]string{
		"SaveData rule": {"SetOption0 0", "SaveData 0"},
		"restart":       {"SetOption0 0", "Hostname plug"},
	} {
		c, r := &fakeCmd{}, &fakeRead{}
		if err := newFixer(c, r).Apply(context.Background(), model.Device{}, lines); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(c.sent) != 1 || count(c.sent, "SaveData") > 1 || len(r.calls) != 0 {
			t.Fatalf("%s: sent %q, reads %v", name, c.sent, r.calls)
		}
	}
}

// Why: if the current SaveData value is unknown we cannot save without
// changing it, and a half-applied device is worse than an untouched one.
func TestApplySendsNothingWhenSaveDataIsUnknown(t *testing.T) {
	c := &fakeCmd{}
	err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"PowerOnState 0"})
	if err == nil || len(c.sent) != 0 {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// Why: the plan is stored and replayed (retry); a tampered or stale line must
// be refused before it reaches a device.
func TestApplyRefusesInvalidLines(t *testing.T) {
	for _, lines := range [][]string{nil, {"Restart 1"}, {"PowerOnState 9"}, {"PowerOnState"}, {"PowerOnState 0; Restart 1"}, {"SetOption0 ON"}, {"PowerOnState 0", "PowerOnState 1"}} {
		c := &fakeCmd{}
		if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, lines); err == nil || len(c.sent) != 0 {
			t.Errorf("%q: err %v, sent %q", lines, err, c.sent)
		}
	}
}

// Why: a long plan must not exceed what a Backlog line can carry.
func TestApplySplitsLongPlans(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "SetOption"+string(rune('0'+i%10))+string(rune('0'+i/10))+" 1")
	}
	c := &fakeCmd{}
	if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, lines); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 4 || count(c.sent, "Backlog ") != 3 || c.sent[3] != "SaveData 0" {
		t.Fatalf("sent %q", c.sent)
	}
	for _, s := range c.sent[:3] {
		if strings.Count(s, ";") >= maxChunkCommands || len(s) > maxChunkBytes+len("Backlog ") {
			t.Errorf("chunk too large: %q", s)
		}
	}
}

// Why: a failed send must stop the run; later commands on a half-applied
// device would make it worse, and the error must say what failed.
func TestApplyStopsOnTheFirstFailedSend(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrUnreachable }}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption0 0"})
	if !errors.Is(err, tasmota.ErrUnreachable) || len(c.sent) != 1 {
		t.Fatalf("err %v, sent %q", err, c.sent)
	}
}

// Why: Hostname and Topic restart the device, so the reply may never arrive.
// That is not a failure; the read-back decides.
func TestApplyToleratesTheRestartCuttingTheReply(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"Hostname plug"}); err != nil {
		t.Fatal(err)
	}
}

// Why: a SaveData that fails leaves the changes in RAM only; that must be
// reported, not swallowed.
func TestApplyReportsAFailedSave(t *testing.T) {
	c := &fakeCmd{err: func(cmd string) error {
		if strings.HasPrefix(cmd, "SaveData") {
			return tasmota.ErrTimeout
		}
		return nil
	}}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption0 0"})
	if err == nil || !strings.Contains(err.Error(), "flash") {
		t.Fatalf("err %v", err)
	}
}

// Why: success means the device now reports the wanted values, not that the
// commands were accepted.
func TestVerifyComparesWithTheDevice(t *testing.T) {
	lines := []string{"PowerOnState 0", "SetOption0 0"}
	got, err := newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0", "SetOption0": "0"}}).Verify(context.Background(), model.Device{}, lines)
	if err != nil || got["PowerOnState"] != "0" {
		t.Fatalf("%v %v", got, err)
	}
	got, err = newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0", "SetOption0": "1"}}).Verify(context.Background(), model.Device{}, lines)
	if err == nil || !strings.Contains(err.Error(), "SetOption0 is 1, expected 0") || got["SetOption0"] != "1" {
		t.Fatalf("mismatch must fail and return what was read: %v %v", got, err)
	}
	if _, err = newFixer(nil, &fakeRead{vals: map[string]string{"PowerOnState": "0"}}).Verify(context.Background(), model.Device{}, lines); err == nil || !strings.Contains(err.Error(), "not reported") {
		t.Fatalf("a missing value is not compliance: %v", err)
	}
}

// Why: a device that restarts needs a moment; giving up on the first miss
// would fail a fix that worked.
func TestVerifyRetriesWhileTheDeviceRestarts(t *testing.T) {
	r := &fakeRead{vals: map[string]string{"Hostname": "plug"}, errs: []error{tasmota.ErrUnreachable, tasmota.ErrUnreachable, nil}}
	waits := 0
	f := newFixer(nil, r)
	f.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	if _, err := f.Verify(context.Background(), model.Device{}, []string{"Hostname plug"}); err != nil || waits != 2 {
		t.Fatalf("err %v after %d waits", err, waits)
	}
	r = &fakeRead{errs: []error{tasmota.ErrUnreachable, tasmota.ErrUnreachable, tasmota.ErrUnreachable}}
	f = newFixer(nil, r)
	f.VerifyAttempts = 3
	if _, err := f.Verify(context.Background(), model.Device{}, []string{"Hostname plug"}); !errors.Is(err, tasmota.ErrUnreachable) || len(r.calls) != 3 {
		t.Fatalf("err %v after %d reads", err, len(r.calls))
	}
}

// Why: SetOption68 (and the other restarting SetOptions) set restart_flag = 2 like Hostname, so
// the reply may never arrive and the restart saves everything. Without that knowledge a
// following SaveData would hit the reboot and come back as an error.
func TestApplyTreatsARestartingSetOptionLikeHostname(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"SetOption68 1"}); err != nil {
		t.Fatal(err)
	}
	if len(c.sent) != 1 || c.sent[0] != "Backlog SetOption68 1" {
		t.Fatalf("sent %q, want only the Backlog (the restart saves, no SaveData after it)", c.sent)
	}
}

// Why: SetOption55 restarts the device only when switched off. Switched on it must keep the
// normal path: SaveData is sent, and a timeout is a real failure, not a restart.
func TestApplyDoesNotTreatSetOption55OnAsARestart(t *testing.T) {
	c := &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption55 1"})
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err %v, want the timeout reported", err)
	}
	c = &fakeCmd{}
	if err := newFixer(c, &fakeRead{vals: map[string]string{"SaveData": "0"}}).Apply(context.Background(), model.Device{}, []string{"SetOption55 1"}); err != nil || count(c.sent, "SaveData") != 1 {
		t.Fatalf("err %v, sent %q, want one SaveData", err, c.sent)
	}
	c = &fakeCmd{err: func(string) error { return tasmota.ErrTimeout }}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, []string{"SetOption55 0"}); err != nil {
		t.Fatalf("SetOption55 0 restarts, its timeout is tolerated: %v", err)
	}
}

// Why: a restart fires once the Backlog queue is empty (support_tasmota.ino:1580,
// CommandsReady), so restarting commands only work if nothing is sent to the device after
// them. They must all sit in the last run(s), never mixed into an earlier run with plain
// commands, and two restarting commands in one Backlog both run before the reboot.
func TestApplyKeepsRestartingCommandsTogetherInTheLastBacklog(t *testing.T) {
	c := &fakeCmd{}
	// seven plain commands fill a run to one short of the limit; plain chunking would put the
	// first restarting command into it and the other two into the next run
	lines := []string{"SetOption1 0", "SetOption2 0", "SetOption4 0", "SetOption5 0", "SetOption6 0", "SetOption7 0", "SaveData 0", "SetOption37 1", "SetOption85 1", "Hostname plug"}
	if err := newFixer(c, &fakeRead{}).Apply(context.Background(), model.Device{}, lines); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Backlog SetOption1 0; SetOption2 0; SetOption4 0; SetOption5 0; SetOption6 0; SetOption7 0; SaveData 0",
		"Backlog SetOption37 1; SetOption85 1; Hostname plug",
	}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
}

// Why (D13): Make compliant never carries a person's consent, so on a device reached over MQTT only the
// router refuses the lines that can cut it off (a new Topic or MqttHost). The rest is applied and saved
// first, the refused lines go last in a chunk of their own, and the result names what was left out so the
// job can verify only what was sent.
func TestApplyLeavesOutWhatCouldCutAnMQTTOnlyDeviceOff(t *testing.T) {
	c := &fakeCmd{err: func(cmd string) error {
		if strings.Contains(cmd, "Topic") || strings.Contains(cmd, "MqttHost") {
			return transport.ErrCutOff
		}
		return nil
	}}
	r := &fakeRead{vals: map[string]string{"SaveData": "0"}}
	err := newFixer(c, r).Apply(context.Background(), model.Device{ID: 1}, []string{"Topic other", "PowerOnState 0", "MqttHost 10.0.0.9"})
	var sk *SkippedError
	if !errors.As(err, &sk) || sk.Applied != 1 || !reflect.DeepEqual(sk.SkippedLines(), []string{"Topic other", "MqttHost 10.0.0.9"}) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "applied 1; skipped Topic, MqttHost: reached over MQTT only (use the console)") {
		t.Errorf("text: %v", err)
	}
	// Topic and MqttHost would have restarted (and so saved) the device; they were left out, so it is saved by hand.
	want := []string{"Backlog Topic other; PowerOnState 0; MqttHost 10.0.0.9", "Backlog PowerOnState 0", "SaveData 0"}
	if !reflect.DeepEqual(c.sent, want) {
		t.Fatalf("sent %q, want %q: the chunk of always, then what the refusal left of it, then the save", c.sent, want)
	}
	// nothing but cut-off lines: nothing is saved and the result says nothing was applied
	c2 := &fakeCmd{err: func(string) error { return transport.ErrCutOff }}
	err = newFixer(c2, r).Apply(context.Background(), model.Device{ID: 1}, []string{"Topic other"})
	if !errors.As(err, &sk) || sk.AppliedAny() || count(c2.sent, "SaveData") != 0 {
		t.Fatalf("err = %v, sent %q", err, c2.sent)
	}
	// over HTTP the same lines go out exactly as they always did: the chunks of chunks(), nothing reordered
	c3 := &fakeCmd{}
	lines := []string{"Topic other", "PowerOnState 0", "MqttHost 10.0.0.9"}
	if err := newFixer(c3, r).Apply(context.Background(), model.Device{ID: 1}, lines); err != nil {
		t.Fatal(err)
	}
	parsed, _ := parseLines(lines)
	var wantHTTP []string
	for _, ch := range chunks(parsed) {
		texts := make([]string, len(ch))
		for i, l := range ch {
			texts[i] = l.text
		}
		wantHTTP = append(wantHTTP, "Backlog "+strings.Join(texts, "; "))
	}
	if !reflect.DeepEqual(c3.sent, wantHTTP) || len(wantHTTP) != 1 {
		t.Fatalf("an HTTP device got %q, want %q", c3.sent, wantHTTP)
	}
}
