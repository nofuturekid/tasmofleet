//go:build integration

package ota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// The upload over MQTT (FileUpload Type 1) against the simulator: the real engine, router, command
// channel, bus and broker. The device is a dual-OTA ESP32 whose HTTP answers nothing (http_unreachable)
// and that cannot download (fw_unreachable), so the upload is the only way.

const upPW = "mqtt-secret-pw"

// addUpload adds a dual-OTA ESP32 that only MQTT reaches and that cannot reach the firmware server,
// with the device's MQTT password stored. Faults are the simulator's.
func (m *itMQTT) addUpload(faults ...simulator.Fault) model.DeviceID {
	return m.addUploadLayout("direct", faults...)
}

// addUploadLayout is addUpload with the layout an earlier read of /up recorded ("" = never read).
func (m *itMQTT) addUploadLayout(layout string, faults ...simulator.Fault) model.DeviceID {
	m.t.Helper()
	spec := simulator.DeviceSpec{Profile: simulator.ProfileESP32Dual, MQTTPassword: upPW,
		Faults: append([]simulator.Fault{{Kind: simulator.FaultFWUnreachable}}, faults...)}
	id := m.addMQTT(spec)
	if err := m.st.SetMQTTPassword(context.Background(), id, model.MQTTPasswordOwn, upPW); err != nil {
		m.t.Fatal(err)
	}
	// the layout as an earlier read of /up over HTTP proved it (an MQTT-only device cannot be asked)
	// the identity an earlier probe stored (chip, flash sizes): the layout is only proof for that hardware
	d, err := m.st.GetDevice(context.Background(), id)
	if err != nil {
		m.t.Fatal(err)
	}
	ident, _, _, err := m.api.ProbeIdentity(context.Background(), d)
	if err != nil {
		m.t.Fatal(err)
	}
	ident.DetectedAt = d.Identity.DetectedAt
	if err := m.st.UpdateIdentity(context.Background(), id, ident); err != nil {
		m.t.Fatal(err)
	}
	if layout != "" {
		if err := m.st.SetUpLayout(context.Background(), id, layout); err != nil {
			m.t.Fatal(err)
		}
	}
	m.facts.mu.Lock()
	if m.facts.fw == nil {
		m.facts.fw = map[model.DeviceID]model.FWReach{}
	}
	m.facts.fw[id] = model.FWReach{State: model.FWReachNo, At: time.Now()}
	m.facts.mu.Unlock()
	return id
}

// fileUploadCmds counts the FileUpload commands the device received (the simulator masks payloads as
// "<N bytes>"): the aborts ("FileUpload 0") and all of them.
func (e *itEnv) fileUploadCmds(dev model.DeviceID) (aborts, all int) {
	for _, c := range e.ctrl[dev].Commands() {
		if !strings.Contains(strings.ToLower(c), "fileupload") {
			continue
		}
		all++
		if strings.HasSuffix(strings.TrimSpace(c), " 0") {
			aborts++
		}
	}
	return
}

func (e *itEnv) noPullCommandsAt(dev model.DeviceID) {
	e.t.Helper()
	e.assertNoPullCommands(dev)
}

var uploadReq = func(dev model.DeviceID) Request {
	return Request{DeviceID: dev, Delivery: model.DeliveryPush, AllowMQTTUpload: true, Language: "en"}
}

// Why: the whole point of the path. A dual-OTA ESP32 that HTTP and the firmware server do not reach
// is updated through the broker: one FileUpload, the new version runs after the restart, neither
// OtaUrl nor Upgrade was sent, and the job says how it went.
func TestITMQTTUploadUpdatesADualOTADevice(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	j := m.expectSuccess(dev, orig, 0, model.PlanDirect)
	if j.Transport != model.TransportMQTT || j.Delivery != model.DeliveryPush || !j.MQTTUpload {
		t.Fatalf("job transport %q delivery %q mqtt_upload %v", j.Transport, j.Delivery, j.MQTTUpload)
	}
	if aborts, all := m.fileUploadCmds(dev); m.lossy.uploads.Load() != 1 || aborts > 1 || all < 4 {
		t.Fatalf("uploads started %d, aborts %d (at most the opening one), %d FileUpload commands: %v", m.lossy.uploads.Load(), aborts, all, m.ctrl[dev].Commands())
	}
	m.noPullCommandsAt(dev)
	for _, c := range m.ctrl[dev].Commands() {
		if strings.EqualFold(strings.TrimSpace(c), "status 0") {
			t.Fatalf("Status 0 was sent")
		}
	}
	if n := m.uploadsOf(dev); n != 0 {
		t.Fatalf("%d uploads over HTTP", n)
	}
}

// refusalCase is a device or request for which the upload must not even start.
func assertNothingPublished(t *testing.T, m *itMQTT, dev model.DeviceID) {
	t.Helper()
	if _, n := m.fileUploadCmds(dev); n != 0 {
		t.Fatalf("a refused update published %d FileUpload commands: %v", n, m.ctrl[dev].Commands())
	}
	m.noPullCommandsAt(dev)
}

// Why (deterministic fit, never try-and-see): an image larger than the usable room of a safeboot-layout
// ESP32 (Status 4 Free less 192 KB), and an ESP8266 (no second stage), are refused in the preview with
// the reason; Submit refuses and publishes nothing.
func TestITMQTTUploadRefusedWhereItDoesNotFit(t *testing.T) {
	m := newItMQTT(t)
	img := padded(t, "tasmota32.bin", false)
	m.override.Store("tasmota32.bin", append(img[:len(img):len(img)], make([]byte, 3<<20)...))
	small := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP32, MQTTPassword: upPW, Faults: []simulator.Fault{{Kind: simulator.FaultFWUnreachable}}})
	if err := m.st.SetUpLayout(context.Background(), small, "direct"); err != nil { // the size is what is tested here
		t.Fatal(err)
	}
	old := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, MQTTPassword: upPW, Faults: []simulator.Fault{{Kind: simulator.FaultFWUnreachable}}})
	m.facts.mu.Lock()
	m.facts.fw = map[model.DeviceID]model.FWReach{small: {State: model.FWReachNo, At: time.Now()}, old: {State: model.FWReachNo, At: time.Now()}}
	m.facts.mu.Unlock()
	for _, dev := range []model.DeviceID{small, old} {
		if err := m.st.SetMQTTPassword(context.Background(), dev, model.MQTTPasswordOwn, upPW); err != nil {
			t.Fatal(err)
		}
	}
	eng := m.start(2)
	for _, tc := range []struct {
		dev  model.DeviceID
		want string
	}{{small, "does not fit"}, {old, "ESP8266"}} {
		q := []Request{{DeviceID: tc.dev, Release: itRelease, Delivery: model.DeliveryPush, AllowMQTTUpload: true, SkipBackup: true, Language: "en"}}
		items, err := eng.Preview(context.Background(), q)
		if err != nil || items[0].Decision != decisionRefuse || !strings.Contains(items[0].Reason, tc.want) {
			t.Fatalf("device %d preview %+v %v, want a refusal containing %q", tc.dev, items[0], err, tc.want)
		}
		if _, err := eng.Submit(context.Background(), "it", q); err == nil {
			t.Fatalf("device %d: Submit accepted an upload that does not fit", tc.dev)
		}
		assertNothingPublished(t, m, tc.dev)
	}
}

// Why: the upload is opt-in per transaction. Without the checkbox, or with the build closed, the path is
// never taken and the preview says how to enable it (only when it would work).
func TestITMQTTUploadNeedsTheCheckboxAndTheBuildGate(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	eng := m.start(2)
	items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, Delivery: model.DeliveryPush, SkipBackup: true, Language: "en"}})
	if err != nil || items[0].Decision != decisionRefuse || !strings.Contains(items[0].Reason, "allow upload over MQTT") {
		t.Fatalf("without the checkbox: %+v %v", items[0], err)
	}
	eng.mqttUpload = false // mqttUploadEnabled = false
	items, err = eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, Delivery: model.DeliveryPush, AllowMQTTUpload: true, SkipBackup: true, Language: "en"}})
	if err != nil || items[0].Decision != decisionRefuse {
		t.Fatalf("with the build gate closed: %+v %v", items[0], err)
	}
	if _, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, Delivery: model.DeliveryPush, AllowMQTTUpload: true, SkipBackup: true, Language: "en"}}); err == nil {
		t.Fatal("Submit accepted the upload with the build gate closed")
	}
	assertNothingPublished(t, m, dev)
}

// Why: the checkbox is only meaningful with push delivery; the job stores it only then (Job.MQTTUpload).
func TestITMQTTUploadCheckboxIsStoredOnlyWithPush(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en", AllowMQTTUpload: true})
	if j := m.jobOf(dev); j.MQTTUpload {
		t.Fatalf("a download job stored the upload checkbox: %+v", j)
	}
}

// Why: HTTP stays first. A device HTTP reaches takes the HTTP push even when the checkbox is ticked;
// the broker is not used for the image.
func TestITMQTTUploadCheckboxDoesNotReplaceTheHTTPPush(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP32, MQTTPassword: upPW}) // healthy HTTP
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	j := m.expectSuccess(dev, orig, 0, model.PlanDirect)
	if j.Transport == model.TransportMQTT {
		t.Fatalf("job transport %q: HTTP works, the push must go over HTTP", j.Transport)
	}
	if n := m.uploadsOf(dev); n != 1 {
		t.Fatalf("%d uploads over HTTP, want 1", n)
	}
	if _, n := m.fileUploadCmds(dev); n != 0 {
		t.Fatalf("%d FileUpload commands for a device HTTP reaches", n)
	}
}

// slowGuard is a selector that records the order of Guard and the cache reads and refuses the Guard that
// comes after the image was read for the upload (the second read of the job).
type orderFW struct {
	Firmware
	opens *atomic.Int32
}

func (o orderFW) Open(ctx context.Context, a model.Artifact) (io.ReadCloser, error) {
	o.opens.Add(1)
	return o.Firmware.(FirmwareFiles).Open(ctx, a)
}

type guardAfterRead struct {
	selector
	opens *atomic.Int32
	after int32
}

func (g guardAfterRead) Guard(id model.Identity, a model.Artifact, u string, c fwselect.Choice) error {
	if g.opens.Load() > g.after {
		return errors.New("test: the Guard refuses right before the upload")
	}
	return g.selector.Guard(id, a, u, c)
}

// Why (safety rule 1): Guard runs immediately before the FileUpload start, after the image bytes were
// read and checked. A Guard that refuses at that moment publishes no FileUpload at all.
func TestITMQTTUploadGuardRefusalPublishesNothing(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	eng := m.start(2)
	opens := &atomic.Int32{}
	eng.fw = orderFW{Firmware: eng.fw, opens: opens}
	eng.sel = guardAfterRead{selector: eng.sel, opens: opens, after: 1} // the 1st read is the set_ota_url check, the 2nd the upload's
	m.submit(eng, uploadReq(dev))
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "guard refused") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if opens.Load() < 2 {
		t.Fatalf("the image was read %d times: the upload step must read it right before Guard", opens.Load())
	}
	assertNothingPublished(t, m, dev)
}

// badImageFW serves bytes of another chip family under the planned artifact: InspectImage must catch it.
type badImageFW struct{ Firmware }

func (b badImageFW) Open(ctx context.Context, a model.Artifact) (io.ReadCloser, error) {
	rc, err := b.Firmware.(FirmwareFiles).Open(ctx, a)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck // test
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	data[0] = 0x00 // not an image: no magic byte
	return io.NopCloser(strings.NewReader(string(data))), nil
}

// Why: the header of the bytes that are sent is checked (InspectImage), not the name of the file.
func TestITMQTTUploadBadImageHeaderPublishesNothing(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	eng := m.start(2)
	eng.fw = badImageFW{eng.fw}
	m.submit(eng, uploadReq(dev))
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	assertNothingPublished(t, m, dev)
}

// Why (a partial Type 1 image is rejected by the ESP32): a link that dies mid-transfer ends with
// FileUpload 0, the job fails with the device on its old firmware, and nothing is resent.
func TestITMQTTUploadCutTransferAbortsAndKeepsTheOldFirmware(t *testing.T) {
	m := newItMQTT(t, func(o *mqttcmd.Options) { o.ReplyWait = 500 * time.Millisecond })
	dev := m.addUpload(simulator.Fault{Kind: simulator.FaultMQTTUploadCut, Chunks: 1})
	before := m.ctrl[dev].Version()
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, "nothing was flashed") {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	if m.ctrl[dev].Version() != before {
		t.Fatalf("the device runs %s after a cut transfer, want %s", m.ctrl[dev].Version(), before)
	}
	aborts, _ := m.fileUploadCmds(dev)
	if n := m.lossy.uploads.Load(); n != 1 {
		t.Fatalf("the upload was started %d times; a lost ack is never resent", n)
	}
	if aborts < 1 {
		t.Fatalf("no FileUpload 0 after the cut: %v", m.ctrl[dev].Commands())
	}
	m.noPullCommandsAt(dev)
}

// Why (the Upgrade 1 rule applied to uploads): the Done was lost but the device took the image. The job
// probes, sees the new version, succeeds, and never uploads a second time or aborts the complete image.
func TestITMQTTUploadLostDoneIsProbedNotResent(t *testing.T) {
	m := newItMQTT(t, func(o *mqttcmd.Options) { o.ReplyWait = 500 * time.Millisecond })
	dev := m.addUpload(simulator.Fault{Kind: simulator.FaultMQTTUploadLostDone})
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	m.expectSuccess(dev, orig, 0, model.PlanDirect)
	aborts, _ := m.fileUploadCmds(dev)
	if n := m.lossy.uploads.Load(); n != 1 || aborts > 1 {
		t.Fatalf("starts %d aborts %d: the lost Done must not cause a resend or a closing abort", n, aborts)
	}
}

// unsentUpload answers every upload with the unknown outcome without sending anything: the device
// keeps running the old firmware, as when the Done was lost and the image is waiting for a restart.
type unsentUpload struct {
	DeviceAPI
	mu    sync.Mutex
	calls int
}

func (u *unsentUpload) UploadFirmwareMQTT(context.Context, model.Device, string, int64, io.Reader, func(int64)) error {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	return fmt.Errorf("test: %w", mqttcmd.ErrOutcomeUnknown)
}

// Why: an upload that was sent completely but never confirmed, on a device that does not restart, ends
// the job as failed (there is no other terminal state) but not as a plain failure: the new image may switch in at the next restart. The job probes and says exactly that.
func TestITMQTTUploadUnknownOutcomeEndsFailedWithTheUnknownText(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	up := &unsentUpload{DeviceAPI: m.api}
	m.api = up
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || j.LastError != ErrTextUploadUnknown {
		t.Fatalf("job %s %q, want the unknown-outcome text", j.State, j.LastError)
	}
	if up.calls != 1 {
		t.Fatalf("%d upload calls: an unknown outcome is never resent", up.calls)
	}
	assertNothingPublished(t, m, dev)
}

// blockingUpload holds the upload open until its context ends (the engine stops), like a transfer that
// is in progress when the process exits.
type blockingUpload struct {
	DeviceAPI
	started chan struct{}
	once    sync.Once
}

func (b *blockingUpload) UploadFirmwareMQTT(ctx context.Context, _ model.Device, _ string, _ int64, _ io.Reader, _ func(int64)) error {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return ctx.Err()
}

// Why: a partial Type 1 transfer cannot be continued. When the engine restarts mid-upload the job probes
// the device, finds it on its old firmware and fails with that sentence; it does not upload again.
func TestITMQTTUploadEngineRestartMidUploadFailsAfterAProbe(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	blk := &blockingUpload{DeviceAPI: m.api, started: make(chan struct{})}
	real := m.api
	m.api = blk
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	select {
	case <-blk.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the upload never started")
	}
	m.stop() // the process exits mid-upload
	if j := m.jobOf(dev); j.State != model.JobUpgradeIssued {
		t.Fatalf("job %s after the stop, want upgrade_issued", j.State)
	}
	m.api = real
	m.start(2) // process start: the job resumes
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || j.LastError != ErrTextUploadInterrupted {
		t.Fatalf("job %s %q, want %q", j.State, j.LastError, ErrTextUploadInterrupted)
	}
	// M1: this process does not remember the unconfirmed image; the resume blocked transfers to the topic
	// again, and without a boot of the device they stay blocked.
	d, err := m.st.GetDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.ch.Upload(context.Background(), d, mqttcmd.UploadSettings, "x", 4096, strings.NewReader(strings.Repeat("x", 4096)), upPW, nil); !errors.Is(err, mqttcmd.ErrFirmwarePending) {
		t.Fatalf("after the resume a transfer is not blocked: %v", err)
	}
	if _, n := m.fileUploadCmds(dev); n > 1 {
		t.Fatalf("a resumed job sent FileUpload commands: %v", m.ctrl[dev].Commands())
	}
}

// Why (review #297 H1, brick risk): a device whose layout was never proven dual-OTA by a read of its
// /up page (or that has the safeboot layout) is never sent a Type 1 upload, however small the image
// and however much its Status 4 Free looks like room: Tasmota would Update.begin() into the running app.
func TestITMQTTUploadNeedsAProvenDualOTALayout(t *testing.T) {
	for name, layout := range map[string]string{"never read": "", "safeboot": "safeboot"} {
		t.Run(name, func(t *testing.T) {
			m := newItMQTT(t)
			dev := m.addUploadLayout(layout)
			eng := m.start(2)
			q := []Request{{DeviceID: dev, Release: itRelease, Delivery: model.DeliveryPush, AllowMQTTUpload: true, SkipBackup: true, Language: "en"}}
			items, err := eng.Preview(context.Background(), q)
			if err != nil || items[0].Path == transport.PushMQTT {
				t.Fatalf("preview %+v %v: the upload was offered", items[0], err)
			}
			if _, err := eng.Submit(context.Background(), "it", q); err == nil {
				t.Fatal("Submit accepted an upload without a proven layout")
			}
			assertNothingPublished(t, m, dev)
		})
	}
}

// Why (L1): the plain image is sent, never gzip, until the ESP32 Update class is verified to take it.
func TestMQTTPushRefusesACompressedImage(t *testing.T) {
	e := &Engine{mqttUpload: true}
	id := model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FreeKB: 4000}
	a := model.Artifact{Filename: "tasmota32.bin.gz", Size: 1 << 20}
	if err := e.mqttPushRefusal(1, id, fwselect.UpLayoutDirect, model.PlanDirect, a); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("a .gz image passed: %v", err)
	}
	a.Filename = "tasmota32.bin"
	if err := e.mqttPushRefusal(1, id, fwselect.UpLayoutDirect, model.PlanDirect, a); err != nil {
		t.Fatalf("the plain image was refused: %v", err)
	}
}

// Why (L2): the bytes that are sent are compared with the checksum the cache recorded.
type wrongSumFW struct{ Firmware }

func (w wrongSumFW) Ensure(ctx context.Context, release, filename string) (model.Artifact, error) {
	a, err := w.Firmware.Ensure(ctx, release, filename)
	a.SHA256 = strings.Repeat("0", 64)
	return a, err
}

func TestMQTTPushRefusesAnImageWithAWrongChecksum(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	eng := m.start(2)
	eng.fw = wrongSumFW{eng.fw}
	m.submit(eng, uploadReq(dev))
	if j := m.waitTerminal(dev); j.State != model.JobFailed {
		t.Fatalf("job %s", j.State)
	}
	assertNothingPublished(t, m, dev)
}

// Why (review #297, H1 residual): a proven layout belongs to the hardware that was read. A device whose
// chip or flash size changed, or whose entry was replaced by other hardware, is refused again until
// /up is read once more.
func TestITMQTTUploadLayoutProofEndsWithTheHardware(t *testing.T) {
	for name, drop := range map[string]func(m *itMQTT, dev model.DeviceID){
		"identity change": func(m *itMQTT, dev model.DeviceID) {
			d, err := m.st.GetDevice(context.Background(), dev)
			if err != nil {
				m.t.Fatal(err)
			}
			d.Identity.FlashSizeKB += 4096
			if err := m.st.UpdateIdentity(context.Background(), dev, d.Identity); err != nil {
				m.t.Fatal(err)
			}
		},
		"replace": func(m *itMQTT, dev model.DeviceID) {
			d, err := m.st.GetDevice(context.Background(), dev)
			if err != nil {
				m.t.Fatal(err)
			}
			if _, err := m.st.SwapHardware(context.Background(), dev, store.HardwareSwap{ExpectMACs: d.MACs, MAC: "aa:bb:cc:dd:ee:01", Address: d.Address, Port: d.Port, Identity: d.Identity}); err != nil {
				m.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newItMQTT(t)
			dev := m.addUpload()
			drop(m, dev)
			eng := m.start(2)
			q := []Request{{DeviceID: dev, Release: itRelease, Delivery: model.DeliveryPush, AllowMQTTUpload: true, SkipBackup: true, Language: "en"}}
			items, err := eng.Preview(context.Background(), q)
			if err != nil || items[0].Path == transport.PushMQTT {
				t.Fatalf("preview %+v %v: the upload was offered", items[0], err)
			}
			if _, err := eng.Submit(context.Background(), "it", q); err == nil {
				t.Fatal("Submit accepted an upload after the hardware changed")
			}
			assertNothingPublished(t, m, dev)
		})
	}
}

// Why (review #297): a restart of tasmofleet forgets an unconfirmed image. The start of the engine blocks
// file transfers again to the device of the newest ended upload whose outcome stayed uncertain, until
// the device announces a boot.
func TestITMQTTUploadEngineStartRearmsAnEndedUnconfirmedUpload(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addUpload()
	real := m.api
	m.api = &unsentUpload{DeviceAPI: real}
	eng := m.start(2)
	m.submit(eng, uploadReq(dev))
	if j := m.waitTerminal(dev); j.State != model.JobFailed || j.MQTTUploadState != model.MQTTUploadUnconfirmed {
		t.Fatalf("job %s state %q", j.State, j.MQTTUploadState)
	}
	d, err := m.st.GetDevice(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	try := func() error {
		return m.ch.Upload(context.Background(), d, mqttcmd.UploadSettings, "x", 4096, strings.NewReader(strings.Repeat("x", 4096)), upPW, nil)
	}
	if err := try(); errors.Is(err, mqttcmd.ErrFirmwarePending) {
		t.Fatalf("blocked before the restart (the wrapper never sent anything): %v", err)
	}
	m.stop()
	m.api = real
	fresh := m.newEngine(2) // a process start; what Run does first is called directly, so no upload races it
	fresh.rearmFinished(context.Background())
	if err := try(); !errors.Is(err, mqttcmd.ErrFirmwarePending) {
		t.Fatalf("after the engine start a transfer is not blocked: %v", err)
	}
}
