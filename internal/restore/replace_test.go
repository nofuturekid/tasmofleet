package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// replaceEnv is a flow with a second, unbound simulator device: the
// replacement hardware.
type replaceEnv struct {
	*flowEnv
	newSim  simulator.Controller
	newHost string
	newPort int
	newMAC  string
}

func (e *flowEnv) describe(ctx context.Context, d model.Device) (model.Device, error) {
	raw5, err := e.cl.Command(ctx, tasmota.TargetFor(d), "Status 5")
	if err != nil {
		return d, err
	}
	var s5 struct{ StatusNET struct{ Mac string } }
	if err := json.Unmarshal(raw5, &s5); err != nil {
		return d, err
	}
	raw2, err := e.cl.Command(ctx, tasmota.TargetFor(d), "Status 2")
	if err != nil {
		return d, err
	}
	fw, err := tasmota.ParseStatus2(raw2)
	if err != nil {
		return d, err
	}
	d.MAC = strings.ToLower(s5.StatusNET.Mac)
	d.Identity = model.Identity{Version: fw.Version, Family: model.ChipESP8266}
	if strings.Contains(fw.Hardware, "ESP32") {
		d.Identity.Family = model.ChipESP32
	}
	return d, nil
}

func newReplaceFlow(t *testing.T, oldVersion, newVersion string, profile simulator.Profile) *replaceEnv {
	t.Helper()
	e := newFlow(t, oldVersion)
	sd, err := e.fleet.Add(t.Context(), simulator.DeviceSpec{Name: "spare", Profile: profile, Version: newVersion})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(sd)
	host, portStr, _ := net.SplitHostPort(sd.Addr())
	port, _ := strconv.Atoi(portStr)
	return &replaceEnv{flowEnv: e, newSim: ctl, newHost: host, newPort: port, newMAC: ctl.MAC()}
}

// breakOld makes the old device go silent, the way a dead board does.
func (e *replaceEnv) breakOld(t *testing.T) {
	t.Helper()
	if err := e.sim.Close(); err != nil {
		t.Fatal(err)
	}
}

func (e *replaceEnv) req(backupID model.BackupID) ReplaceRequest {
	return ReplaceRequest{Device: e.dev, Address: e.newHost, Port: e.newPort, Backup: backupID}
}

func (e *replaceEnv) newDump(t *testing.T) []byte {
	t.Helper()
	b, err := e.cl.DownloadConfig(t.Context(), tasmota.TargetFor(model.Device{Address: e.newHost, Port: e.newPort}))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Why: the end-to-end promise of "Replace device". The new board ends up with
// the old board's configuration, its own factory configuration is kept as a
// backup, and the inventory entry keeps its name, tags and notes but now
// stands for the new MAC. The old MAC is in the audit trail.
func TestReplaceRoundTripAgainstSimulator(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	old := e.device
	old.Tags, old.Notes = []string{"kitchen"}, "above the sink"
	if err := e.st.UpdateDevice(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	e.dumpBackup(t, e.dumpA)
	e.newSim.SetConfigDump(e.dumpB) // the factory configuration of the new board
	e.breakOld(t)

	job, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || done.Kind != store.KindReplace || !done.Verified {
		t.Fatalf("job: %+v", done)
	}
	if got := e.newDump(t); !bytes.Equal(got, e.dumpA) {
		t.Error("the new board does not hold the old board's configuration")
	}
	pre, preB, err := e.bk.Open(t.Context(), done.PreBackupID)
	if err != nil || !bytes.Equal(pre, e.dumpB) || !backup.IsReplacementHardware(preB) {
		t.Errorf("the factory configuration of the new board must be kept, marked as replacement hardware: %v %+v", err, preB)
	}
	d, err := e.st.GetDevice(t.Context(), e.dev)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "plug" || d.Notes != "above the sink" || len(d.Tags) != 1 || d.MAC != e.newMAC || d.Address != e.newHost || d.Port != e.newPort {
		t.Errorf("entry after the replace: %+v (new MAC %s)", d, e.newMAC)
	}
	if _, err := e.st.GetDeviceByMAC(t.Context(), old.MAC); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the old MAC still finds the entry: %v", err)
	}
	fin := e.audit.waitFor(t, "replace.finish")
	if fin.Result != "ok" || !strings.Contains(fin.Detail, old.MAC) || !strings.Contains(fin.Detail, e.newMAC) {
		t.Errorf("replace.finish must name old and new MAC: %+v", fin)
	}
	cmds := e.cl.sent("RestoreConfig")
	if len(cmds) != 1 {
		t.Fatalf("commands: %v", cmds)
	}
}

// Why: two identical devices on one network clash (same hostname, MQTT topic,
// static IP). While the old device still answers at its address, replacing it
// must be refused before anything is sent to the new board.
func TestReplaceRefusedWhileTheOldDeviceIsReachable(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	e.newSim.SetConfigDump(e.dumpB)
	_, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
	if !errors.Is(err, ErrOldReachable) {
		t.Fatalf("err = %v, want ErrOldReachable", err)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
	if got := e.newDump(t); !bytes.Equal(got, e.dumpB) {
		t.Error("the new board was touched")
	}
}

// Why: a dump from one chip family on another is not a risk to accept but a
// wrong device: the settings layout and the GPIO map differ. The refusal is
// final (no tick helps) and nothing is sent.
func TestReplaceWithAnotherChipFamilyIsRefusedForGood(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP32)
	e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	_, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowMajorChange: true, AllowUnknownFamily: true})
	if !errors.Is(err, ErrFamilyMismatch) {
		t.Fatalf("err = %v, want ErrFamilyMismatch", err)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: hardware that is already an inventory entry cannot take over another
// entry; the swap would steal its MAC. Refused before anything is sent.
func TestReplaceRefusesHardwareThatIsAlreadyInTheInventory(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	if _, err := e.st.CreateDevice(t.Context(), model.Device{Name: "spare", Address: e.newHost, Port: e.newPort, MAC: e.newMAC}); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
	if !errors.Is(err, ErrCandidateBound) {
		t.Fatalf("err = %v, want ErrCandidateBound", err)
	}
	// ... and an entry that merely sits on the new board's address clashes too
	e2 := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e2.dumpBackup(t, e2.dumpA)
	e2.breakOld(t)
	if _, err := e2.st.CreateDevice(t.Context(), model.Device{Name: "squatter", Address: e2.newHost, Port: e2.newPort}); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.m.StartReplace(t.Context(), "alice", e2.req(0), StartOptions{AllowUnknownFamily: true}); !errors.Is(err, ErrCandidateBound) {
		t.Fatalf("address taken by another entry: %v", err)
	}
	if n := len(e.cl.sent("RestoreConfig")) + len(e2.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: the entry is not a candidate for itself: if the "new" board is the
// device being replaced, the admin wants a plain restore.
func TestReplaceRefusesTheDeviceItself(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	r := ReplaceRequest{Device: e.dev, Address: e.device.Address, Port: e.device.Port}
	if _, err := e.m.PreviewReplace(t.Context(), r); !errors.Is(err, ErrCandidateIsOld) {
		t.Fatalf("err = %v", err)
	}
}

// Why: a dump from newer firmware is never applied to older firmware, and a
// different major version is a risk only the admin can accept.
func TestReplaceFirmwareRules(t *testing.T) {
	t.Run("newer backup refused", func(t *testing.T) {
		e := newReplaceFlow(t, "16.0.0", "15.6.0", simulator.ProfileESP8266_4M)
		e.dumpBackup(t, e.dumpA)
		e.breakOld(t)
		if _, err := e.m.PreviewReplace(t.Context(), e.req(0)); !errors.Is(err, ErrNewerBackup) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("other major version needs a tick", func(t *testing.T) {
		e := newReplaceFlow(t, "14.4.1", "15.6.0", simulator.ProfileESP8266_4M)
		e.dumpBackup(t, e.dumpA)
		e.breakOld(t)
		p, err := e.m.PreviewReplace(t.Context(), e.req(0))
		if err != nil || !p.Major {
			t.Fatalf("preview: %+v %v", p, err)
		}
		var need *OverrideError
		if _, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true}); !errors.As(err, &need) || !need.Major {
			t.Fatalf("without the tick: %v", err)
		}
		if n := len(e.cl.sent("RestoreConfig")); n != 0 {
			t.Errorf("restore upload sent %d times before the tick", n)
		}
		if _, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowMajorChange: true, AllowUnknownFamily: true}); err != nil {
			t.Fatalf("with the tick: %v", err)
		}
		e.m.Wait()
	})
}

// Why: the failure that must not leave a half-done inventory. The new board
// was restored, but the MAC was bound to another entry in the meantime (the
// UNIQUE constraint): the job fails loudly and the entry still has all of its
// old hardware data.
func TestReplaceSwapConflictFailsAndLeavesTheEntryAlone(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	before, _ := e.st.GetDevice(t.Context(), e.dev)
	e.hb.hookOf = func(call int) {
		if call == 2 { // the verification backup: the new board is back and holds the dump
			if _, err := e.st.CreateDevice(t.Context(), model.Device{Name: "thief", Address: "10.9.9.9", MAC: e.newMAC}); err != nil {
				t.Error(err)
			}
		}
	}
	job, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "could not take it over") {
		t.Fatalf("job: %+v", done)
	}
	after, _ := e.st.GetDevice(t.Context(), e.dev)
	if after.MAC != before.MAC || after.Address != before.Address || len(after.MACs) != len(before.MACs) {
		t.Errorf("partial inventory state:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Why (#432 review M1): only a proven restart of the new board may let the entry take it over.
// An upload whose outcome is unknown and that never restarted the board, followed by one failed
// poll (a Wi-Fi hiccup), must not count as a restart: the entry would move onto a board that
// still runs its factory configuration. A refused upload is certain and never swaps either.
func TestReplaceWithoutAProvenRestartNeverSwaps(t *testing.T) {
	for name, uploadErr := range map[string]error{
		"unknown outcome, no restart, one failed poll": tasmota.ErrTimeout,
		"refused upload": fmt.Errorf("%w: File invalid", tasmota.ErrUploadRefused),
	} {
		t.Run(name, func(t *testing.T) {
			e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
			e.dumpBackup(t, e.dumpA)
			e.newSim.SetConfigDump(e.dumpB)
			e.breakOld(t)
			before, _ := e.st.GetDevice(t.Context(), e.dev)
			e.cl.swallowErr = uploadErr // nothing reaches the board
			var failedOnce atomic.Bool
			e.cl.failCmd = func(c string) error {
				if c == "Status 5" && len(e.cl.sent("RestoreConfig")) > 0 && failedOnce.CompareAndSwap(false, true) {
					return tasmota.ErrUnreachable
				}
				return nil
			}
			e.m.o.RebootTimeout = 600 * time.Millisecond
			job, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
			if err != nil {
				t.Fatal(err)
			}
			done := e.finish(t, job.ID)
			if done.State != store.RestoreFailed {
				t.Fatalf("job: %+v", done)
			}
			after, _ := e.st.GetDevice(t.Context(), e.dev)
			if after.MAC != before.MAC || after.Address != before.Address {
				t.Errorf("the entry moved onto the board without a proven restart:\nbefore %+v\nafter  %+v", before, after)
			}
			if got := e.newDump(t); !bytes.Equal(got, e.dumpB) {
				t.Error("the new board's configuration changed")
			}
		})
	}
}

// Why: the expected identity is the NEW board's MAC recorded before the
// start. If another host takes over the address after the preview, the dump
// must not go to it.
func TestReplaceChecksTheNewBoardsMACAgainBeforeSending(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	e.hb.hookOf = func(call int) {
		if call == 1 {
			e.setStatus("aa:bb:cc:dd:ee:ff", "")
		}
	}
	job, err := e.m.StartReplace(t.Context(), "alice", e.req(0), StartOptions{AllowUnknownFamily: true})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !strings.Contains(done.Detail, "nothing was changed") {
		t.Fatalf("job: %+v", done)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: a backup of replacement hardware (an earlier, aborted replace) is filed
// under the entry, but it is not the broken device's configuration. "Newest"
// must skip it, and an explicit choice of it is refused.
func TestReplaceNeverRestoresReplacementHardwareBackups(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	a := e.dumpBackup(t, e.dumpA)
	foreign, err := e.bk.BackupOf(t.Context(), "alice", e.dev, model.Device{Address: e.newHost, Port: e.newPort}, backup.NoteReplacementHardware)
	if err != nil {
		t.Fatal(err)
	}
	e.breakOld(t)
	p, err := e.m.PreviewReplace(t.Context(), e.req(0))
	if err != nil || p.Backup.ID != a.ID {
		t.Fatalf("newest = %+v (%v), want the entry's own dump %d", p.Backup, err, a.ID)
	}
	for _, b := range foreign {
		if b.Kind == model.BackupConfigDump {
			if _, err := e.m.PreviewReplace(t.Context(), e.req(b.ID)); !errors.Is(err, ErrNotDump) {
				t.Errorf("explicit replacement-hardware backup: %v", err)
			}
		}
	}
}

// Why: a new board that took over the old board's address (DHCP gives it the
// same lease) must not be mistaken for "the old device is still there".
func TestReplaceAllowsTheNewBoardOnTheOldAddress(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	d := e.device
	d.Address, d.Port = e.newHost, e.newPort
	if err := e.st.UpdateDevice(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.PreviewReplace(t.Context(), e.req(0)); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// Why: the candidate list must offer only boards that answer, are reachable
// inside the device networks and are not in the inventory.
func TestCandidatesAreOnlineAndUnbound(t *testing.T) {
	e := newReplaceFlow(t, "15.6.0", "15.6.0", simulator.ProfileESP8266_4M)
	eps := []Endpoint{
		{Address: e.newHost, Port: e.newPort},            // the spare
		{Address: e.device.Address, Port: e.device.Port}, // bound
		{Address: "127.0.0.1", Port: 1},                  // nothing there
	}
	got := e.m.Candidates(t.Context(), eps)
	if len(got) != 1 || got[0].Port != e.newPort || got[0].MAC != e.newMAC {
		t.Fatalf("candidates = %+v", got)
	}
}

// Why: a restore of a device whose MAC was never stored used to be refused
// outright. Reading the live MAC and storing it is enough to make the checks
// that follow meaningful, and it must be audited.
func TestRestoreStoresAMissingMAC(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	dev := e.device
	dev.MAC = ""
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	// Assessment is read-only: it must not store anything.
	as, err := e.m.Assess(t.Context(), a.ID)
	if err != nil || !as.MACUnknown {
		t.Fatalf("assess: %+v %v", as, err)
	}
	if d, _ := e.st.GetDevice(t.Context(), e.dev); d.MAC != "" {
		t.Fatal("Assess wrote the MAC")
	}
	if as.LiveMAC != e.sim.MAC() || as.Address != e.device.Address {
		t.Errorf("the page must be able to show the MAC the device reports: %+v", as)
	}
	// Trust on first use is explicit: without the MAC the admin confirmed,
	// nothing is recorded and nothing is sent.
	var need *OverrideError
	if _, err := e.m.Start(t.Context(), "alice", a.ID); !errors.As(err, &need) || need.MAC != e.sim.MAC() {
		t.Fatalf("start without a confirmed MAC: %v", err)
	}
	if d, _ := e.st.GetDevice(t.Context(), e.dev); d.MAC != "" || len(e.cl.sent("RestoreConfig")) != 0 {
		t.Fatal("recorded or sent without confirmation")
	}
	// A different host answering now than the one the admin confirmed.
	if _, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ExpectMAC: "aa:bb:cc:dd:ee:ff"}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("swapped device: %v", err)
	}
	if d, _ := e.st.GetDevice(t.Context(), e.dev); d.MAC != "" {
		t.Fatal("recorded the MAC of a device the admin did not confirm")
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ExpectMAC: e.sim.MAC()})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
	if d, _ := e.st.GetDevice(t.Context(), e.dev); d.MAC != e.sim.MAC() {
		t.Errorf("stored MAC = %q, want %q", d.MAC, e.sim.MAC())
	}
	if !strings.Contains(strings.Join(e.audit.actions(), " "), "device.mac_adopted:ok") {
		t.Errorf("adoption not audited: %v", e.audit.actions())
	}
	for _, ev := range e.audit.events() {
		if ev.Action == "device.mac_adopted" && !strings.Contains(ev.Detail, "identity recorded on first restore") {
			t.Errorf("audit detail: %s", ev.Detail)
		}
	}
}

// Why: the recorded identity is the one re-checked right before the upload is
// sent, so a host swapped in after the confirmation gets nothing.
func TestAdoptedMACIsRecheckedBeforeTheUpload(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	dev := e.device
	dev.MAC = ""
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	e.hb.hook = func(call int) {
		if call == 1 {
			e.setStatus("aa:bb:cc:dd:ee:ff", "")
		}
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ExpectMAC: e.sim.MAC()})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreFailed || len(e.cl.sent("RestoreConfig")) != 0 {
		t.Fatalf("job: %+v, restore uploads %d", done, len(e.cl.sent("RestoreConfig")))
	}
}

// Why: adopting must not paper over a mix-up: if the host at the address has
// the MAC of another entry, it is not this device.
func TestRestoreRefusesToAdoptTheMACOfAnotherEntry(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	dev := e.device
	dev.MAC = ""
	if err := e.st.UpdateDevice(t.Context(), dev); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateDevice(t.Context(), model.Device{Name: "other", Address: "10.8.8.8", MAC: e.sim.MAC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ExpectMAC: e.sim.MAC()}); !errors.Is(err, ErrIdentity) {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.cl.sent("RestoreConfig")); n != 0 {
		t.Errorf("restore upload sent %d times", n)
	}
}

// Why: the confirmation page shows the risks before the click, so one
// confirmation covers them all; a hard refusal shows up there too.
func TestAssessNamesTheRisksAndTheRefusals(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	as, err := e.m.Assess(t.Context(), a.ID)
	if err != nil || as.Major || as.Family || as.MACUnknown {
		t.Fatalf("plain case: %+v %v", as, err)
	}
	e.setStatus("", "14.0.0") // the device runs older firmware than the backup
	if _, err := e.m.Assess(t.Context(), a.ID); !errors.Is(err, ErrNewerBackup) {
		t.Errorf("newer backup: %v", err)
	}
	e.setStatus("aa:bb:cc:dd:ee:ff", "")
	if _, err := e.m.Assess(t.Context(), a.ID); !errors.Is(err, ErrIdentity) {
		t.Errorf("wrong device: %v", err)
	}
}
