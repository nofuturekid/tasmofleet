package ota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Push over MQTT (experimental, D19): the engine sends the firmware image through the broker with
// FileUpload Type 1, for an ESP32 that TasmoFleet cannot reach over HTTP and that cannot download. It
// uses the push states of the machine (the set_ota_url step runs Guard and the image checks, the
// upgrade_issued step uploads) and every rule of the other uploads: Guard right before the start, the
// device's OtaUrl is never touched, and a lost reply is never answered with a second upload.

// mqttUploadEnabled gates the upload over MQTT. Closing it is this one line: NewEngine copies it into
// Engine.mqttUpload (which the tests set on their own engine), the path chooser then never offers the
// upload and a stored job refuses it. The path was only ever run against the simulator.
const mqttUploadEnabled = true

// The overall deadline of an upload is chunks x 1 s + 60 s. A chunk carries a little under 900 raw
// bytes (a 1200 byte packet less its framing, base64 and the topic); live a chunk took about 100 ms, so
// the deadline is ten times what the transfer needs, and the estimate below is deliberately small.
const (
	mqttUploadChunkBytes = 800
	mqttUploadPerChunk   = time.Second
	mqttUploadSlack      = 60 * time.Second
	mqttImageHeader      = 24 // what fwselect.InspectImage reads
	mqttImageMax         = 16 << 20
)

// MQTTUploader is implemented by a DeviceAPI that can push an image through the broker. It is optional
// like FirmwareUploader: without it the upload over MQTT is not offered.
type MQTTUploader interface {
	// UploadFirmwareMQTT sends size bytes of body as the image filename (FileUpload Type 1). progress, if
	// set, receives the bytes acknowledged so far. nil means the device confirmed the whole image.
	// mqttcmd.ErrOutcomeUnknown means every byte was sent but never confirmed: the image may become
	// active at the next restart, the caller probes and never resends. Every other error leaves the
	// device on its firmware.
	UploadFirmwareMQTT(ctx context.Context, d model.Device, filename string, size int64, body io.Reader, progress func(sent int64)) error
}

// uploadDeadline is the whole time an upload of size bytes may take: one second per chunk and a minute.
func uploadDeadline(size int64) time.Duration {
	chunks := (size + mqttUploadChunkBytes - 1) / mqttUploadChunkBytes
	return time.Duration(chunks)*mqttUploadPerChunk + mqttUploadSlack
}

// layoutOf is the flash layout an earlier HTTP read of the device's /up page recorded; unknown when it
// never was read. The upload over MQTT needs it proven direct (fwselect.MQTTUploadFits).
func layoutOf(d model.Device) fwselect.UpLayoutHint {
	return fwselect.UpLayoutOf(d.UpLayout, d.UpLayoutAt)
}

// mqttPushRefusal says why the image cannot go through the broker to this device, or nil. It is decided
// on data alone, before anything is published, and never by trying: the fit rule of D19
// (fwselect.MQTTUploadFits) plus what the engine itself requires of the file.
func (e *Engine) mqttPushRefusal(dev model.DeviceID, id model.Identity, layout fwselect.UpLayoutHint, plan model.Plan, a model.Artifact) error {
	switch {
	case e.uploadGate.refusal(dev) != nil:
		return e.uploadGate.refusal(dev)
	case !e.mqttUpload:
		return errors.New("upload over MQTT is not enabled in this build")
	case plan != model.PlanDirect:
		return errors.New(errPushStaged)
	case strings.HasSuffix(strings.ToLower(a.Filename), ".gz"):
		// The ESP32 Update class has not been verified to take gzip over this path; the plain image is sent.
		return fmt.Errorf("%s is compressed; the upload over MQTT sends only the plain image", a.Filename)
	}
	if ok, _, _, why := fwselect.MQTTUploadFits(id, layout, a.Size); !ok {
		return errors.New(why)
	}
	return nil
}

// readMQTTImage reads the cached image once, verifies its length and checks its header: the bytes that
// pass here are the bytes that are sent. A header of another chip family is refused.
func (e *Engine) readMQTTImage(ctx context.Context, id model.Identity, a model.Artifact) ([]byte, error) {
	files, ok := e.fw.(FirmwareFiles)
	if !ok {
		return nil, refused(errors.New("upload over MQTT is not available: the firmware cache cannot hand out files"))
	}
	if _, ok := e.api.(MQTTUploader); !ok {
		return nil, refused(errors.New("upload over MQTT is not available: the device API cannot upload"))
	}
	if a.Size <= 0 || a.Size > mqttImageMax {
		return nil, refused(fmt.Errorf("%s has an unusable size (%d bytes)", a.Filename, a.Size))
	}
	rc, err := files.Open(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("firmware %s: %w", a.Filename, err)
	}
	defer rc.Close() //nolint:errcheck // read-only file
	data, err := io.ReadAll(io.LimitReader(rc, a.Size+1))
	if err != nil {
		return nil, fmt.Errorf("firmware %s: %w", a.Filename, err)
	}
	if int64(len(data)) != a.Size {
		return nil, refused(fmt.Errorf("%s has %d bytes, the cache says %d", a.Filename, len(data), a.Size))
	}
	// The bytes that are sent must be the bytes the cache vouches for (Guard checked the artifact, not the file).
	if sum := sha256.Sum256(data); a.SHA256 == "" || !strings.EqualFold(hex.EncodeToString(sum[:]), a.SHA256) {
		return nil, refused(fmt.Errorf("%s does not match the checksum in the cache", a.Filename))
	}
	info, err := fwselect.InspectImage(data[:min(len(data), mqttImageHeader)])
	if err != nil {
		return nil, refused(fmt.Errorf("%s: %w", a.Filename, err))
	}
	if info.Family != id.Family {
		return nil, refused(fmt.Errorf("%s is an image for %s, the device is %s", a.Filename, info.Family, id.Family))
	}
	return data, nil
}

// mqttPushCheck runs in the set_ota_url step, before the job commits to the upload: the refusals above
// and a readable image with a matching header, so a damaged cache file fails the job cleanly.
func (e *Engine) mqttPushCheck(ctx context.Context, j model.Job, d model.Device, a model.Artifact) error {
	if !j.MQTTUpload {
		return refused(errors.New("the transaction did not allow the upload over MQTT"))
	}
	if err := e.mqttPushRefusal(j.DeviceID, j.IdentityBefore, layoutOf(d), j.Plan, a); err != nil {
		return refused(err)
	}
	_, err := e.readMQTTImage(ctx, j.IdentityBefore, a)
	return err
}

// pushMQTT uploads the artifact through the broker. stageArtifact (Guard) ran right before; the image
// is read and inspected here and Guard runs once more after that, so the check sits immediately before
// the FileUpload start and both saw the bytes that are sent. The upload has its own deadline.
func (e *Engine) pushMQTT(ctx context.Context, j model.Job, d model.Device, a model.Artifact) Event {
	up, ok := e.api.(MQTTUploader)
	if !ok || !j.MQTTUpload {
		return failEvent(refused(errors.New("upload over MQTT is not available")))
	}
	if err := e.mqttPushRefusal(j.DeviceID, j.IdentityBefore, layoutOf(d), j.Plan, a); err != nil {
		return failEvent(refused(err))
	}
	data, err := e.readMQTTImage(ctx, j.IdentityBefore, a)
	if err != nil {
		return failEvent(err)
	}
	if err := e.sel.Guard(j.IdentityBefore, a, e.fw.URLFor(a), guardChoice(j)); err != nil {
		return failEvent(refused(err))
	}
	// State drift: nothing read at preview or plan time is trusted. The device, its identity (chip, flash,
	// free flash, firmware) and the layout proof are read again now, and the upload goes ahead only if the
	// job's own picture of the device still holds. The block on file transfers is checked by the channel
	// itself when the start is published.
	if err := e.recheckBeforeUpload(ctx, j, d, a); err != nil {
		return failEvent(refused(err))
	}
	if j.CancelRequested {
		return Event{Kind: EvCommandFailed, NotFlashed: true, Err: "cancelled by the user"}
	}
	uctx, cancel := context.WithTimeout(ctx, uploadDeadline(a.Size))
	defer cancel()
	size := int64(len(data))
	step, next := max(size/20, 1), int64(0)
	progress := func(sent int64) {
		if sent < next {
			return
		}
		next = sent + step
		e.notify.JobChanged(j, model.JobEvent{JobID: j.ID, At: e.clk.Now(), From: j.State, To: j.State, Event: "upload_progress",
			Detail: fmt.Sprintf("%d %% (%d of %d KB)", sent*100/size, sent/1024, (size+1023)/1024)})
	}
	err = up.UploadFirmwareMQTT(uctx, d, a.Filename, size, bytes.NewReader(data), progress)
	switch {
	case err == nil:
		return Event{Kind: EvCommandOK}
	case errors.Is(err, mqttcmd.ErrOutcomeUnknown), errors.Is(err, mqttcmd.ErrFirmwarePending):
		// ErrFirmwarePending: an earlier upload (Retry, or another process) is still unconfirmed and nothing
		// was published now: the device may hold an image, so this is no "nothing was flashed" either.
		// Every byte went out and nothing confirmed it: the image may be active after the next restart.
		return Event{Kind: EvCommandFailed, OutcomeUnknown: true, Err: err.Error()}
	}
	// A refusal, a cut or silent link: the transfer was aborted (a partial image is rejected), the device keeps its firmware.
	return Event{Kind: EvCommandFailed, NotFlashed: true, Err: err.Error()}
}

// upLayoutStore is implemented by the device store: it remembers the layout of a read of /up.
type upLayoutStore interface {
	SetUpLayout(ctx context.Context, id model.DeviceID, layout string) error
}

// rememberLayout records what a read of the device's /up page showed. The upload over MQTT cannot read
// that page and refuses a layout that was never proven direct.
func (e *Engine) rememberLayout(ctx context.Context, d model.Device, l tasmota.UpLayout) {
	st, ok := e.devices.(upLayoutStore)
	if !ok || d.ID == 0 || (l != "" && l != tasmota.UpLayoutDirect && l != tasmota.UpLayoutSafeboot) || d.UpLayout == string(l) {
		return
	}
	if err := st.SetUpLayout(ctx, d.ID, string(l)); err != nil {
		e.log.Warn("ota: could not store the upload layout", "device", d.ID, "err", err)
	}
}

// UploadRearmer is implemented by a DeviceAPI that can block file transfers to a device again after
// a restart of tasmofleet.
type UploadRearmer interface {
	// MarkUploadPending tells the MQTT channel that a firmware upload to the device ended without a
	// confirmation: no file transfer to it until the device has booted again (commands still go out).
	MarkUploadPending(ctx context.Context, d model.Device) error
}

// rearmUpload runs when a job that uploads through the broker is resumed in upgrade_issued: this
// process does not remember the unconfirmed image, so the block that keeps a later transfer from
// aborting (and thereby committing) it must be set again before anything else is published.
func (e *Engine) rearmUpload(ctx context.Context, j model.Job) {
	if !mqttPush(j) {
		return
	}
	rt, ok := e.api.(UploadRearmer)
	if !ok {
		e.log.Warn("ota: the device API cannot block transfers after a restart", "job", j.ID)
		e.uploadGate.blockDevice(j.DeviceID)
		return
	}
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err == nil {
		err = rt.MarkUploadPending(pinned(ctx, j), d)
	}
	if err != nil {
		// Fail closed: without the block a later transfer could abort, and so commit, the unconfirmed image.
		e.log.Warn("ota: could not block transfers to a device with an interrupted upload; uploads to it are refused", "job", j.ID, "err", err)
		e.uploadGate.blockDevice(j.DeviceID)
	}
}

// uploadGate refuses uploads through the broker where the block on file transfers could not be set again
// after a restart: for one device, or for all when the jobs could not be listed.
type uploadGate struct {
	mu   sync.Mutex
	all  bool
	devs map[model.DeviceID]bool
}

func (g *uploadGate) blockDevice(id model.DeviceID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.devs == nil {
		g.devs = map[model.DeviceID]bool{}
	}
	g.devs[id] = true
}

func (g *uploadGate) blockAll() {
	g.mu.Lock()
	g.all = true
	g.mu.Unlock()
}

// refusal is the reason an upload to the device is refused, or nil.
func (g *uploadGate) refusal(id model.DeviceID) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.all:
		return errors.New("tasmofleet could not check which devices hold an unconfirmed firmware upload")
	case g.devs[id]:
		return errors.New("tasmofleet could not block file transfers to this device after its interrupted upload")
	}
	return nil
}

// rearmFinished runs at the start of Run: the newest ended upload through the broker of each device whose
// outcome stayed uncertain blocks file transfers to it again, because this process does not remember
// the image the device may still hold. The next INFO3 boot announcement of the device lifts the block.
func (e *Engine) rearmFinished(ctx context.Context) {
	jobs, err := e.jobs.ListJobs(ctx, store.JobFilter{States: []model.JobState{model.JobFailed, model.JobCancelled}})
	if err != nil {
		e.log.Warn("ota: could not look for uncertain uploads; uploads through the broker are refused", "err", err)
		e.uploadGate.blockAll() // fail closed
		return
	}
	seen := map[model.DeviceID]bool{}
	for _, j := range jobs { // newest first
		if seen[j.DeviceID] || !j.IsUpdate() {
			continue
		}
		seen[j.DeviceID] = true
		if mqttPush(j) && j.MQTTUploadState != model.MQTTUploadClean {
			e.rearmUpload(ctx, j)
		}
	}
}

// recheckBeforeUpload reads the device and its identity afresh and refuses when anything differs from
// what the job was planned on, or when the fit or the layout proof no longer holds on the fresh data.
// Every read error is a refusal.
func (e *Engine) recheckBeforeUpload(ctx context.Context, j model.Job, probed model.Device, a model.Artifact) error {
	d, err := e.devices.GetDevice(ctx, j.DeviceID)
	if err != nil {
		return fmt.Errorf("the device could not be read again before the upload: %w", err)
	}
	if d.Topic != probed.Topic || d.FullTopic != probed.FullTopic || d.GroupTopic != probed.GroupTopic || d.MQTT != probed.MQTT {
		// The upload is published to the topic of the snapshot the job holds; if the entry now points elsewhere,
		// the identity read below would be another device's.
		return errors.New("the device's MQTT settings changed since the update was planned")
	}
	id, _, _, err := e.api.ProbeIdentity(ctx, d)
	if err != nil {
		return fmt.Errorf("the device could not be read again before the upload: %w", err)
	}
	if why := identityDrift(j.IdentityBefore, id); why != "" {
		return fmt.Errorf("the device changed since the update was planned (%s)", why)
	}
	return e.mqttPushRefusal(j.DeviceID, id, layoutOf(d), j.Plan, a)
}

// identityDrift names the first fact of the device that is not what the job was planned on, "" if none.
func identityDrift(before, now model.Identity) string {
	switch {
	case now.Family != before.Family:
		return "chip family"
	case now.FlashSizeKB != before.FlashSizeKB || now.ProgramFlashSizeKB != before.ProgramFlashSizeKB:
		return "flash size"
	case now.FreeKB != before.FreeKB:
		return "free flash"
	case now.Version != before.Version || now.Image != before.Image || now.Variant != before.Variant:
		return "firmware"
	}
	return ""
}

// readLayout reads /up once for the push routing and records the proof for the upload over MQTT: "direct"
// only when the page proves the NORMAL firmware runs on two app partitions (tasmota.UpPage.ProvesDualOTA:
// a single-OTA ESP32 that runs safeboot serves the plain form as well), "safeboot" for the safeboot form,
// and nothing (the older proof is dropped) for a page that is ambiguous or proves too little. Without a
// page reader the routing layout is still returned and no proof is recorded.
func (e *Engine) readLayout(ctx context.Context, d model.Device, sb SafebootAPI) (tasmota.UpLayout, error) {
	pr, ok := e.api.(UpPageAPI)
	if !ok {
		return sb.UpLayout(ctx, d)
	}
	p, err := pr.UpPage(ctx, d)
	switch {
	case err != nil:
		if errors.Is(err, tasmota.ErrUpLayoutAmbiguous) {
			e.rememberLayout(ctx, d, "")
		}
		return "", err
	case p.Layout == tasmota.UpLayoutSafeboot:
		e.rememberLayout(ctx, d, tasmota.UpLayoutSafeboot)
	case p.ProvesDualOTA():
		e.rememberLayout(ctx, d, tasmota.UpLayoutDirect)
	default:
		e.rememberLayout(ctx, d, "")
	}
	return p.Layout, nil
}

// rearmResumed blocks file transfers again for the jobs that go on after a restart in a state where an
// upload through the broker may have been running or waiting for its restart (the machine resumes
// upgrade_issued and await_reboot alike).
func (e *Engine) rearmResumed(ctx context.Context, active []model.Job) {
	for _, j := range active {
		if j.State == model.JobUpgradeIssued || j.State == model.JobAwaitReboot {
			e.rearmUpload(ctx, j)
		}
	}
}
