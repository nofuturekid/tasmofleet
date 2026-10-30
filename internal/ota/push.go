package ota

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Push delivery (backlog F-9): instead of OtaUrl and Upgrade 1 the engine
// uploads the image to the device's /u2 endpoint, like the web UI's "Upgrade
// by file upload". The state machine is unchanged: the set_ota_url step only
// runs Guard (and the second relay reading), and upgrade_issued uploads.
// Every rule of Upgrade 1 holds: Guard runs right before the upload, a lost
// reply is never answered with a second upload (the engine probes), and the
// device's OtaUrl is never touched, so there is nothing to restore.

// FirmwareUploader is implemented by a DeviceAPI that can push an image. It
// is optional so existing fakes stay valid; without it push jobs are refused.
type FirmwareUploader interface {
	// UploadFirmware sends size bytes of body as the file filename. A nil error
	// means the device accepted the image. Every other error leaves it open
	// whether the device flashes (the caller probes first), except
	// tasmota.ErrUploadRefused and tasmota.ErrUploadUnsupported: nothing was
	// flashed and the job fails at once.
	UploadFirmware(ctx context.Context, d model.Device, filename string, size int64, body io.Reader) error
}

// SafebootAPI is implemented by a DeviceAPI that can read the upload layout of
// a device and switch its boot partition (push into safeboot, #30). It is
// optional like FirmwareUploader: without it a device that needs safeboot
// downloads.
type SafebootAPI interface {
	// UpLayout is a read-only GET of /up.
	UpLayout(ctx context.Context, d model.Device) (tasmota.UpLayout, error)
	// SwitchBoot sends GET /u4?u4=<to>&api= once. tasmota.BootRestarting says
	// the device restarts. An error other than tasmota.ErrSwitchNotDone and
	// tasmota.ErrAuth leaves open whether it does: probe, never send again.
	SwitchBoot(ctx context.Context, d model.Device, to tasmota.BootTarget) (tasmota.BootReply, error)
}

// UpPageAPI is implemented by a DeviceAPI that can read the /up page with its footer. It is optional: without
// it the layout still routes a push, but no proof for the upload over MQTT is recorded.
type UpPageAPI interface {
	UpPage(ctx context.Context, d model.Device) (tasmota.UpPage, error)
}

// FirmwareFiles is implemented by the firmware cache next to Firmware: the
// bytes of a cached artifact, verified against its SHA-256.
type FirmwareFiles interface {
	Open(ctx context.Context, a model.Artifact) (io.ReadCloser, error)
}

// uploadTimeoutFactor scales the command timeout for the upload action: the
// device writes flash while it receives the image. It stays below the
// upgrade_issued deadline, so the wait for the reboot starts after the upload.
const uploadTimeoutFactor = 4

// Reasons why a device downloads instead of taking a push upload ("push where
// possible" falls back per device). ESP8266 stays on the download until the
// upload was tested on real hardware (backlog F-9).
const (
	errPushESP8266  = "push upload is not available for ESP8266 yet"
	errPushChip     = "push upload needs a device with a known ESP32 chip"
	errPushSafeboot = "push upload needs a device running a normal firmware, not safeboot or minimal"
	errPushStaged   = "push upload cannot do a two-step update"
	// errPushSafebootOff is the note of a device that would need the push into safeboot while it
	// is gated off (tests only; the shipped gate is open).
	errPushSafebootOff = "push into safeboot is not enabled yet"
)

// safebootPushEnabled gates the push into safeboot (#30). It was opened after the sequence ran on a
// real ESP32 (16.0.0 reinstall, cancel and switch back; docs/research/tasmota-minimal-and-ota.md).
// When false, a device that needs safeboot for an upload downloads instead. NewEngine copies it into
// Engine.safebootPush, which the tests set on their own engine.
const safebootPushEnabled = true

// pushDeviceRefusal says why a device cannot take a push upload, or nil. It
// is decided on the detected identity, before any image is fetched.
func pushDeviceRefusal(id model.Identity) error {
	switch {
	case id.Family == model.ChipESP8266:
		return errors.New(errPushESP8266)
	case !id.Family.IsESP32():
		return errors.New(errPushChip)
	case isIntermediate(id.Variant):
		return errors.New(errPushSafeboot)
	}
	return nil
}

// uploadMarginBytes: Tasmota's upload handler starts Update with the free
// sketch space less one flash sector (HandleUploadLoop in
// xdrv_01_9_webserver.ino) and refuses a larger image with "Not enough space".
const uploadMarginBytes = 4096

// pushSpaceRefusal says why the image does not fit the device's free flash
// for an upload, or nil. FreeKB is Status 4 "Free", read with the identity.
// A safeboot-layout ESP32 (one app partition) reports only what is left of
// its running partition and takes a full image only through the download.
func pushSpaceRefusal(id model.Identity, size int64) error {
	if id.FreeKB <= 0 {
		return errors.New("the device's free flash is unknown (Status 4)")
	}
	if size > int64(id.FreeKB)*1024-uploadMarginBytes {
		return &pushSpaceError{fmt.Sprintf("not enough free flash for an upload (image %d KB, %d KB free)", (size+1023)/1024, id.FreeKB)}
	}
	return nil
}

// pushSpaceError: the image is larger than the running partition can take. On an ESP32 with the
// safeboot layout this is the case the push into safeboot is for.
type pushSpaceError struct{ msg string }

func (e *pushSpaceError) Error() string { return e.msg }

// safebootRoute decides, for a push request whose image does not fit the running partition, whether
// the job pushes into safeboot. It reads /up (read-only): the safeboot layout shows the upload form
// that calls upl(this) while the normal firmware runs. It returns the note why the device downloads
// instead, "" to keep the space refusal's own text.
func (e *Engine) safebootRoute(ctx context.Context, d model.Device, id model.Identity) (via bool, note string) {
	sb, ok := e.api.(SafebootAPI)
	if !ok || !id.Family.IsESP32() {
		return false, ""
	}
	layout, err := e.readLayout(ctx, d, sb)
	if err != nil || layout != tasmota.UpLayoutSafeboot {
		return false, "" // a dual layout cannot be told from an unreadable page: the plain refusal stands (the upload re-checks /up and refuses upl(this), #191)
	}
	if !e.safebootPush {
		return false, errPushSafebootOff
	}
	return true, ""
}

// firstByteImage is the first byte of every ESP32 image: the browser's own upload script
// (HTTP_HEAD_LAST_SCRIPT32.h upl()) takes the safeboot route only for a file that starts with it.
const firstByteImage = 0xE9

// pushPlanRefusal: a staged plan means a minimal hop, which the upload does not drive.
func pushPlanRefusal(p model.Plan) error {
	if p != model.PlanDirect {
		return errors.New(errPushStaged)
	}
	return nil
}

// pushCheck verifies, before the job commits to the upload, that the image can
// be read and still matches the cache: a damaged file is found while the
// job can still fail cleanly. For the push into safeboot it also checks that
// the image is a plain ESP32 image: this runs before the device is restarted.
func (e *Engine) pushCheck(ctx context.Context, a model.Artifact, viaSafeboot bool) error {
	rc, err := e.openImage(ctx, a)
	if err != nil {
		return err
	}
	defer rc.Close() //nolint:errcheck // read-only file
	if !viaSafeboot {
		return nil
	}
	var first [1]byte
	if _, err := io.ReadFull(rc, first[:]); err != nil || first[0] != firstByteImage {
		return refused(fmt.Errorf("%s does not start with the ESP32 image byte 0xE9; the safeboot upload takes only a plain image", a.Filename))
	}
	return nil
}

func (e *Engine) openImage(ctx context.Context, a model.Artifact) (io.ReadCloser, error) {
	files, ok := e.fw.(FirmwareFiles)
	if !ok {
		return nil, refused(errors.New("push upload is not available: the firmware cache cannot hand out files"))
	}
	if _, ok := e.api.(FirmwareUploader); !ok {
		return nil, refused(errors.New("push upload is not available: the device API cannot upload"))
	}
	rc, err := files.Open(ctx, a)
	if err != nil {
		return nil, fmt.Errorf("firmware %s: %w", a.Filename, err)
	}
	return rc, nil
}

// push uploads the artifact. Guard ran in stageArtifact right before.
func (e *Engine) push(ctx context.Context, d model.Device, a model.Artifact) Event {
	rc, err := e.openImage(ctx, a)
	if err != nil {
		// Nothing was sent: certain, so a push into safeboot goes straight to its recovery.
		return Event{Kind: EvCommandFailed, Err: err.Error(), NotFlashed: true}
	}
	defer rc.Close() //nolint:errcheck // read-only file
	if err := e.api.(FirmwareUploader).UploadFirmware(ctx, d, a.Filename, a.Size, rc); err != nil {
		// A refusal and a missing upload form are certain: nothing was flashed.
		certain := errors.Is(err, tasmota.ErrUploadRefused) || errors.Is(err, tasmota.ErrUploadUnsupported)
		return Event{Kind: EvCommandFailed, Err: err.Error(), NotFlashed: certain}
	}
	return Event{Kind: EvCommandOK}
}
