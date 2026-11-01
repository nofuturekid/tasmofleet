package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// fakeUploader stands in for the Router's FileUpload Type 2: with apply it does what the device
// does after a matching MD5 (takes the dump and restarts), then returns err.
type fakeUploader struct {
	e     *flowEnv
	apply bool
	err   error
	calls int
	dump  []byte
	bind  transport.UploadBinding
}

func (u *fakeUploader) UploadConfig(ctx context.Context, t tasmota.Target, dump []byte, bind transport.UploadBinding) error {
	u.calls++
	u.dump, u.bind = dump, bind
	u.e.cl.mu.Lock()
	u.e.cl.cmds = append(u.e.cl.cmds, "UPLOAD")
	u.e.cl.mu.Unlock()
	if u.apply {
		u.e.sim.SetConfigDump(dump)
		if _, err := u.e.cl.Client.Command(ctx, t, "Restart 1"); err != nil {
			return err
		}
	}
	return u.err
}

// idGuard fails every command whose target has no device ID: the Router sends such a call over raw
// HTTP (scans and address checks), so a restore over MQTT must never make one.
type idGuard struct{ *hookClient }

func (g idGuard) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if t.DeviceID() == 0 {
		return nil, errors.New("call without a device ID would go over raw HTTP: " + c)
	}
	return g.hookClient.Command(ctx, t, c)
}

// mqttManager is a restore service without a firmware base URL whose restores go over MQTT.
func (e *flowEnv) mqttManager(t *testing.T, u *fakeUploader, path transport.Path) *Manager {
	t.Helper()
	m, err := New(Options{
		Devices: e.st, Backups: e.hb, Store: e.st, Client: idGuard{e.cl}, Files: e.bk, Uploader: u,
		PathFor: func(context.Context, model.Device) (transport.Path, string) {
			return path, "over MQTT (FileUpload), because HTTP fails"
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Hardware: e.st,
		PollInterval: 20 * time.Millisecond, RebootTimeout: 3 * time.Second, Context: t.Context(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.m = m
	return m
}

func (e *flowEnv) cmdLog() []string {
	e.cl.mu.Lock()
	defer e.cl.mu.Unlock()
	return append([]string(nil), e.cl.cmds...)
}

// Why (#430): the service needs one way to restore: the HTTP restore upload of the client or the
// MQTT upload; nothing depends on FIRMWARE_BASE_URL any more. A restore the facts send over HTTP
// with a client that cannot upload says so at the start instead of failing late.
func TestNewNeedsOnePathAndHTTPNeedsTheRestoreUpload(t *testing.T) {
	base := Options{Devices: nopDevices{}, Backups: nopBackups{}, Store: nopStore{}, Client: nopClient{}}
	if _, err := New(base); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("neither path: %v", err)
	}
	base.Uploader = &fakeUploader{}
	if _, err := New(base); err != nil {
		t.Fatalf("MQTT only: %v", err)
	}
	base.Uploader = nil
	base.Client = restoringClient{}
	if _, err := New(base); err != nil {
		t.Fatalf("HTTP only: %v", err)
	}
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e}, transport.HTTP)
	m.o.Client = commandOnly{e.cl} // the facts say HTTP, but the client has no restore upload
	if _, err := m.Start(t.Context(), "alice", a.ID); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("HTTP without the restore upload: %v", err)
	}
	if got := e.cl.sent("RestoreConfig"); len(got) != 0 {
		t.Fatalf("restore upload sent: %v", got)
	}
}

// commandOnly hides every method of the wrapped client but tasmota.Client's.
type commandOnly struct{ tasmota.Client }

type restoringClient struct{ nopClient }

func (restoringClient) RestoreConfig(context.Context, tasmota.Target, []byte) error { return nil }

// Why: a restore over MQTT is the same promise as over HTTP (the device holds the backup, the
// old state is kept, the result is verified), with the same order: safety backup, identity,
// upload. It never uses the HTTP restore upload and records its transport.
func TestRestoreOverMQTTRoundTrip(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	u := &fakeUploader{e: e, apply: true}
	m := e.mqttManager(t, u, transport.MQTT)
	e.hb.hook = func(call int) {
		if call == 1 {
			e.cl.mu.Lock()
			e.cl.cmds = append(e.cl.cmds, "BACKUP")
			e.cl.mu.Unlock()
		}
	}
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Transport != model.TransportMQTT {
		t.Errorf("transport %q", job.Transport)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified || done.Transport != model.TransportMQTT {
		t.Fatalf("job: %+v", done)
	}
	if !bytes.Equal(e.deviceDump(t), e.dumpA) || !bytes.Equal(u.dump, e.dumpA) {
		t.Error("the device does not hold the restored configuration")
	}
	if pre, _, err := e.bk.Open(t.Context(), done.PreBackupID); err != nil || !bytes.Equal(pre, e.dumpB) {
		t.Errorf("the pre-restore backup must hold the former configuration: %v", err)
	}
	if got := e.cl.sent("RestoreConfig"); len(got) != 0 {
		t.Errorf("a restore over MQTT used the HTTP restore upload: %v", got)
	}
	// the upload is bound to the identity the restore checked: the MAC and the topic of this device
	dev, _ := e.st.GetDevice(t.Context(), e.dev)
	if len(u.bind.MACs) == 0 || u.bind.MACs[0] != dev.MAC || u.bind.MACs[0] == "" || u.bind.Topic != dev.Topic || u.bind.Name != dev.Name {
		t.Errorf("binding %+v does not carry the checked device %q %q", u.bind, dev.MAC, dev.Topic)
	}
	log := e.cmdLog()
	up := -1
	for i, c := range log {
		if c == "UPLOAD" {
			up = i
		}
	}
	bk := -1
	for i, c := range log {
		if c == "BACKUP" {
			bk = i
		}
	}
	// the identity check (Status 5) and the boot count (Status 1) come right before the upload,
	// after everything the safety backup read
	if bk < 0 || up < bk+3 || log[up-2] != "Status 5" || log[up-1] != "Status 1" {
		t.Errorf("order must be safety backup, identity, upload: %v", log)
	}
}

// Why: the upload is refused before the device changes anything (wrong password, no room, link
// lost): the job fails at the upload step, the device keeps its configuration and does not restart.
// The uploader is a stand-in here; that a refused or aborted FileUpload really leaves the device's
// settings alone is proved against the simulator in mqttcmd (TestUploadCutMidTransferFailsAndFlashesNothing,
// TestUploadType1SafebootNoRoomAndDualSuccess, TestUploadErrorCodes).
func TestRestoreOverMQTTFailedUploadLeavesTheDeviceUnchanged(t *testing.T) {
	for name, err := range map[string]error{"password": mqttcmd.ErrPasswordRefused, "no room": mqttcmd.ErrNoRoom, "transfer": mqttcmd.ErrTransfer} {
		t.Run(name, func(t *testing.T) {
			e := newFlow(t, "15.6.0")
			a := e.dumpBackup(t, e.dumpA)
			e.sim.SetConfigDump(e.dumpB)
			m := e.mqttManager(t, &fakeUploader{e: e, err: err}, transport.MQTT)
			job, serr := m.Start(t.Context(), "alice", a.ID)
			if serr != nil {
				t.Fatal(serr)
			}
			done := e.finish(t, job.ID)
			if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepUpload || !strings.Contains(done.Detail, "FileUpload") {
				t.Fatalf("job: %+v", done)
			}
			if !bytes.Equal(e.deviceDump(t), e.dumpB) || e.sim.Boots() != 0 {
				t.Error("a failed upload must leave the settings unchanged and the device running")
			}
		})
	}
}

// Why: a lost "Done" means the device may have applied the dump. The restore must look at the
// device (boot count) instead of failing, and must not upload a second time.
func TestRestoreOverMQTTUnknownOutcomeIsCheckedOnTheDevice(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	u := &fakeUploader{e: e, apply: true, err: mqttcmd.ErrOutcomeUnknown}
	job, err := e.mqttManager(t, u, transport.MQTT).Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone || u.calls != 1 {
		t.Fatalf("job %+v, uploads %d", done, u.calls)
	}
}

// Why: the restart is proven by a larger BootCount from a fresh Status 1, not by anything stale
// (an INFO3 of the earlier boot, an unchanged device that merely answers). An upload that
// returned but never restarted the device is a failure at the restart step.
func TestRestoreOverMQTTNeedsTheBootCountToRise(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: false}, transport.MQTT) // says Done, nothing happens
	m.o.RebootTimeout = 300 * time.Millisecond
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepRestart || !strings.Contains(done.Detail, "boot count") {
		t.Fatalf("job: %+v", done)
	}
}

// Why: a path of None (HTTP failing and MQTT unusable) is a refusal with the reason, before
// anything is sent; the restore never falls back to a path the facts ruled out.
func TestRestoreRefusedWhenNoPathExists(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e}, transport.None)
	before := len(e.cmdLog())
	_, err := m.Start(t.Context(), "alice", a.ID)
	if !errors.Is(err, ErrNoPath) || !strings.Contains(err.Error(), "HTTP fails") {
		t.Fatalf("%v", err)
	}
	if len(e.cmdLog()) != before {
		t.Errorf("commands were sent: %v", e.cmdLog()[before:])
	}
}

// Why: the confirmation page shows the path; Assess reports it.
func TestAssessReportsThePath(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil || !as.ViaMQTT || !strings.Contains(as.PathNote, "FileUpload") {
		t.Fatalf("%+v %v", as, err)
	}
}

// topicDecoder is fakeDecoder whose decoded backups also carry the MQTT settings the lockout
// check compares.
type topicDecoder struct{ topic, user string }

func (d topicDecoder) Version(context.Context) (string, error) { return "16.0.0.0", nil }
func (d topicDecoder) Decode(_ context.Context, dmp []byte) ([]byte, error) {
	extra := ""
	if d.user != "" {
		extra = `,"mqtt_user":"` + d.user + `"`
	}
	return []byte(fmt.Sprintf(`{"header":{"data":{"hardware":"ESP82xx"}},"cfg_crc":%d,"tele_period":%d,"mqtt_topic":%q%s}`, dmp[0], dmp[1], d.topic, extra)), nil
}

// Why (H1): a restore over MQTT whose backup changes the topic (or the broker, user, password)
// can cut TasmoFleet off from a device it reaches only over MQTT. It is refused unless the admin
// accepts that risk; the same backup over HTTP, where the device stays reachable, is not.
func TestRestoreOverMQTTRefusesABackupThatChangesTheMQTTSettings(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	_, err := m.Start(t.Context(), "alice", a.ID)
	var need *OverrideError
	if !errors.As(err, &need) || !need.Lockout || !strings.Contains(need.Detail, "changes Topic") || !strings.Contains(need.Detail, "may lose it") {
		t.Fatalf("want a lockout confirmation, got %v", err)
	}
	if strings.Contains(need.Detail, "some-other-topic") {
		t.Errorf("the sentence names a value: %q", need.Detail)
	}
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil || !as.Lockout {
		t.Fatalf("Assess: %+v %v", as, err)
	}
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{AllowMQTTLockout: true})
	if err != nil {
		t.Fatal(err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
}

func TestRestoreOverHTTPAcceptsABackupWithAnotherTopic(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "some-other-topic"})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	job, err := e.m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatalf("the device stays reachable over HTTP: %v", err)
	}
	if done := e.finish(t, job.ID); done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
}

// Why (H1): a backup whose decoded twin is missing cannot be compared, so over MQTT it needs the
// same confirmation.
func TestRestoreOverMQTTWithoutADecodedTwinNeedsTheLockoutConfirmation(t *testing.T) {
	e := newFlowWith(t, "15.6.0", failingDecoder{})
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e, apply: true}, transport.MQTT)
	_, err := m.Start(t.Context(), "alice", a.ID)
	var need *OverrideError
	if !errors.As(err, &need) || !need.Lockout || !strings.Contains(need.Detail, "cannot be read") {
		t.Fatalf("%v", err)
	}
}

type failingDecoder struct{}

func (failingDecoder) Version(context.Context) (string, error) { return "", errors.New("no decoder") }
func (failingDecoder) Decode(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("no decoder")
}

// Why (H1): the user and the password are compared too, without ever naming a value.
func TestMQTTLockoutComparesUserAndPassword(t *testing.T) {
	e := newFlowWith(t, "15.6.0", topicDecoder{topic: "", user: "someone-else"})
	a := e.dumpBackup(t, e.dumpA)
	m := e.mqttManager(t, &fakeUploader{e: e}, transport.MQTT)
	dev, _ := e.st.GetDevice(t.Context(), e.dev)
	why := m.mqttLockout(t.Context(), dev, a)
	if !strings.Contains(why, "MqttUser") || strings.Contains(why, "someone-else") {
		t.Fatalf("%q", why)
	}
	if !differsPassword("a", model.Device{MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "b"}, nil) ||
		differsPassword("a", model.Device{MQTTPasswordMode: model.MQTTPasswordOwn, MQTTPassword: "a"}, nil) ||
		!differsPassword("a", model.Device{MQTTPasswordMode: model.MQTTPasswordBroker}, func() string { return "b" }) {
		t.Error("password comparison")
	}
}

// Why (M2): a failing safety backup stops the MQTT restore before the identity check, the boot
// count and the upload, at the backup step.
func TestFailedPreBackupStopsTheMQTTRestoreBeforeAnyChange(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.cl.failDL.Store(true)
	u := &fakeUploader{e: e, apply: true}
	job, err := e.mqttManager(t, u, transport.MQTT).Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || Step(done) != StepBackup || !strings.Contains(done.Detail, "nothing was changed") {
		t.Fatalf("job: %+v", done)
	}
	if u.calls != 0 || e.sim.Boots() != 0 || !bytes.Equal(e.deviceDumpUnguarded(t), e.dumpB) {
		t.Errorf("uploads %d, boots %d: the device must be untouched", u.calls, e.sim.Boots())
	}
}

func (e *flowEnv) deviceDumpUnguarded(t *testing.T) []byte {
	t.Helper()
	e.cl.failDL.Store(false)
	return e.deviceDump(t)
}

// Why (M2): an upload whose "Done" is lost and after which the device never restarts fails at the
// restart step; the dump was sent exactly once.
func TestRestoreOverMQTTUnknownOutcomeWithoutARestartFailsAtTheRestartStep(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	u := &fakeUploader{e: e, apply: false, err: mqttcmd.ErrOutcomeUnknown}
	m := e.mqttManager(t, u, transport.MQTT)
	m.o.RebootTimeout = 300 * time.Millisecond
	job, err := m.Start(t.Context(), "alice", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || Step(done) != StepRestart || u.calls != 1 {
		t.Fatalf("job %+v, uploads %d", done, u.calls)
	}
}
