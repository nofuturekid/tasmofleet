package mqttcmd

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: the Tasmota file transfer protocol mandates MD5
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// The two file types the uploader pushes. Nothing else: it must not become a generic file pusher.
const (
	UploadFirmware = 1 // UPL_TASMOTA: OTA firmware (D19, experimental)
	UploadSettings = 2 // UPL_SETTINGS: a settings dump (restore)

	maxUploadSize = 1<<31 - 1
	maxNameLen    = 64
	// settingsDumpSize is the smallest settings dump (a .dmp is one 4 KiB flash sector).
	settingsDumpSize = 4096
)

// ErrNoRoom: the device answered "Error 2" to FileUpload: the image does not fit its OTA space, or
// the settings buffer cannot be allocated. Nothing was written. It also matches ErrTransfer.
var ErrNoRoom = errors.New("mqttcmd: the device has no room for the file (Error 2)")

// ErrOutcomeUnknown: every byte was sent but the device never confirmed ("Done" lost, or the
// connection ended while waiting). The device may well have applied the upload and be restarting.
// Callers probe the device (settings: BootCount; firmware: version) and never resend.
// For Type 1 whose chunks were all acknowledged, the image may still become active at the next
// restart, because no abort was sent.
var ErrOutcomeUnknown = errors.New("mqttcmd: the upload was sent but the device never confirmed it (a firmware image may become active at the next restart); check the device, do not resend")

// Upload sends r (size bytes) to d with FileUpload: typ 2 is a settings dump, typ 1 a firmware
// image. name is the File field. progress, if set, is called with the bytes acknowledged so far.
// The overall time limit is ctx; each message waits at most ReplyWait.
//
// Protocol (docs/research/tasmota-mqtt-transport.md section 3, live test 2026-10-30):
// start {"Password","File","Id","Type","Size"} -> "Started" and {"Id","MaxSize"}; then
// {"Id","Data"} chunks of the latest MaxSize, one acknowledgement each; then {"Id","Md5"} ->
// "Done". progress runs on the uploading goroutine between chunks and must not block. The device has one transfer state for both directions, so an upload after an unclean
// transfer (this process cannot tell what the device holds) opens with "FileUpload 0".
//
// After a start was (maybe) published, every failure except the device's own "Error N" ends with
// "FileUpload 0" -- except for Type 1 once every chunk was acknowledged: the abort would run
// Update.end(true) and commit the complete image, so the outcome is left unknown instead. Nothing is retried or resent. No payload is ever logged or put into an error:
// the start carries the password, and a minimal firmware echoes it back.
func (c *Channel) Upload(ctx context.Context, d model.Device, typ int, name string, size int64, r io.Reader, password string, progress func(sent int64)) error {
	if err := checkUpload(d, typ, name, size, r); err != nil {
		return err
	}
	id, err := newUploadID()
	if err != nil {
		return err
	}
	start, err := uploadStart(password, name, id, typ, size)
	if err != nil {
		return err
	}
	if !c.bus.Connected() {
		return fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttbus.ErrNotConnected)
	}
	sem := c.sem(d.Topic)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err() // still queued: nothing was sent
	case <-c.done:
		return c.closed()
	}
	defer func() { <-sem }()
	if c.offline(d.Topic) {
		return fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}

	in := make(chan mqttbus.Message, waitDepth)
	c.mu.Lock()
	if err := c.firmwarePendingLocked(d.Topic); err != nil {
		c.mu.Unlock()
		return err
	}
	boots := c.bootsSeen[d.Topic]
	c.files[d.Topic] = in
	clean := c.fileClean[d.Topic] // shared with Download: one FMqtt state on the device
	c.fileClean[d.Topic] = false  // clean again only after "Done"
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.files, d.Topic)
		c.mu.Unlock()
	}()

	if !clean {
		if err := c.sendFileCmd(ctx, d.Topic, cmdUpload, "0"); err != nil {
			return err
		}
		c.waitAbortedOf(ctx, in, suffixUpload)
	}
	abort, err := c.runUpload(ctx, d.Topic, start, id, typ, size, r, in, progress)
	if err != nil && typ == UploadFirmware && (errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, errMD5Mismatch)) {
		c.mu.Lock()
		if c.bootsSeen[d.Topic] == boots { // the device has not booted since the transfer began
			c.fwPending[d.Topic] = boots
		}
		c.mu.Unlock()
	}
	if err != nil {
		if abort && !deviceReset(err) {
			c.abortCmd(d.Topic, cmdUpload) // the start may have reached the device: leave it idle
		}
		return err
	}
	c.mu.Lock()
	c.fileClean[d.Topic] = true
	c.mu.Unlock()
	return nil
}

// errMD5Mismatch: the device rejected a complete Type 1 image at the MD5. Its Update object may still
// hold the data, so the topic is blocked like after an unconfirmed upload.
var errMD5Mismatch = errors.New("md5 mismatch")

// MarkFirmwarePending blocks file transfers to the topic until the device has booted again (INFO3):
// the caller knows a firmware upload to it ended without a confirmation, and this process may not
// (it restarted). Idempotent.
func (c *Channel) MarkFirmwarePending(topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.fwPending[topic]; !ok {
		c.fwPending[topic] = c.bootsSeen[topic]
	}
}

// firmwarePendingLocked refuses a transfer on a topic whose Type 1 upload has an unknown outcome.
//
// Every chunk of the image was acknowledged but nothing confirmed it, so the device holds a
// complete image in FMqtt (file_id still set, Update still open) that "FileUpload 0" would commit
// with Update.end(true) (xdrv_02_1_mqtt_file.ino, Tasmota v16.0.0, lines 162-171). Any further
// file command is refused until the device has booted again (its INFO3 announcement, which a Wi-Fi or
// broker reconnect does not repeat; LWT Offline then Online alone proves nothing), which drops
// that state. "FileDownload 0" would not call Update.end (its abort path only frees the settings
// buffer, lines 369-375), but it does not clear file_id either and a download start on a busy
// FMqtt falls through into the upload's dummy buffer, so it is refused too. After the reboot the
// topic is "unclean" as usual and the next transfer opens with its abort.
func (c *Channel) firmwarePendingLocked(topic string) error {
	at, pending := c.fwPending[topic]
	if !pending {
		return nil
	}
	if c.bootsSeen[topic] > at {
		delete(c.fwPending, topic)
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRefused, ErrFirmwarePending)
}

func checkUpload(d model.Device, typ int, name string, size int64, r io.Reader) error {
	if err := checkTopic(d); err != nil {
		return err
	}
	if typ != UploadFirmware && typ != UploadSettings {
		return fmt.Errorf("%w: FileUpload type %d is not supported (only 1 and 2)", ErrRefused, typ)
	}
	// An aborted Type 1 runs Update.end(true): an ESP32 rejects a partial image, an ESP8266 only
	// checks the magic byte and would boot a truncated one. Firmware over MQTT is ESP32 only.
	if typ == UploadFirmware && !d.Identity.Family.IsESP32() {
		return fmt.Errorf("%w: firmware over MQTT needs a known ESP32 family", ErrRefused)
	}
	if r == nil {
		return fmt.Errorf("%w: no data to upload", ErrRefused)
	}
	if name == "" || len(name) > maxNameLen {
		return fmt.Errorf("%w: the file name must be 1 to %d characters", ErrRefused, maxNameLen)
	}
	for _, ch := range name {
		if ch == '"' || ch == '\\' || ch < 0x20 || ch == 0x7f {
			return fmt.Errorf("%w: the file name contains a character that cannot be sent", ErrRefused)
		}
	}
	switch {
	case size <= 0 || size > maxUploadSize:
		return fmt.Errorf("%w: invalid upload size %d", ErrRefused, size)
	case typ == UploadSettings && (size < settingsDumpSize || size > maxDumpSize):
		return fmt.Errorf("%w: a settings dump has %d to %d bytes, not %d", ErrRefused, settingsDumpSize, maxDumpSize, size)
	}
	return nil
}

// newUploadID picks the transfer Id. The device keeps file_id in a uint8_t: a larger value is
// truncated there and the acknowledgements would no longer carry ours (the plan said 1..999).
func newUploadID() (int, error) {
	var b [1]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("mqttcmd: no random transfer id: %w", err)
		}
		if b[0] != 0 {
			return int(b[0]), nil
		}
	}
}

// uploadStart builds {"Password":…,"File":…,"Id":…,"Type":…,"Size":…} in compact framing.
func uploadStart(password, name string, id, typ int, size int64) (string, error) {
	if err := checkFilePassword(password); err != nil {
		return "", err
	}
	return `{"Password":"` + password + `","File":"` + name + `","Id":` + strconv.Itoa(id) +
		`,"Type":` + strconv.Itoa(typ) + `,"Size":` + strconv.FormatInt(size, 10) + `}`, nil
}

// uploadErrorStatus maps "Error N" of FileUpload to a typed error (the device ended the transfer).
func uploadErrorStatus(s string, typ int) error {
	n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(s, "Error")))
	switch n {
	case 1:
		return &deviceResetError{ErrPasswordRefused}
	case 2:
		what := "the settings buffer cannot be allocated"
		if typ == UploadFirmware {
			what = "the image does not fit"
		}
		return &deviceResetError{fmt.Errorf("%w: %w: the device refused: %s (Error 2)", ErrTransfer, ErrNoRoom, what)}
	case 6:
		return &deviceResetError{transferErr("the device could not prepare the update (Error 6)")}
	case 3:
		return &deviceResetError{transferErr("the device refused: the file type is not supported (Error 3)")}
	case 5:
		return &deviceResetError{transferErr("the device refused: the command arrived on a group topic (Error 5)")}
	}
	return &deviceResetError{transferErr("the device refused the upload (Error %d)", n)}
}

// startSettle is how long the uploader listens for a second answer to the start.
const startSettle = 50 * time.Millisecond

// moreAnswers reports whether another FILEUPLOAD message arrives within settle.
func (c *Channel) moreAnswers(ctx context.Context, in chan mqttbus.Message, settle time.Duration) bool {
	timer := time.NewTimer(settle)
	defer timer.Stop()
	for {
		select {
		case m := <-in:
			if m.Suffix == suffixUpload {
				return true
			}
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		case <-c.done:
			return false
		}
	}
}

// queuedAnswer reports, without waiting, whether another FILEUPLOAD message is already queued.
func queuedAnswer(in chan mqttbus.Message) bool {
	for {
		select {
		case m := <-in:
			if m.Suffix == suffixUpload {
				return true
			}
		default:
			return false
		}
	}
}

// rawBudget is the largest raw chunk whose {"Id":N,"Data":"<b64>"} fits room (17 bytes of framing
// plus the digits of the Id; base64 turns 3 raw bytes into 4).
func rawBudget(room, id int) int { return (room - 17 - len(strconv.Itoa(id))) / 4 * 3 }

// runUpload returns abort=true while a "FileUpload 0" must end the transfer on failure: once the
// start was (maybe) published, and for Type 1 only until the last chunk was acknowledged.
func (c *Channel) runUpload(ctx context.Context, topic, start string, id, typ int, size int64, r io.Reader, in chan mqttbus.Message, progress func(int64)) (abort bool, err error) {
	if err := c.sendFileCmd(ctx, topic, cmdUpload, start); err != nil {
		// A timeout here means the publish was attempted; a refusal or a spent context sent nothing.
		return errors.Is(err, tasmota.ErrTimeout), err
	}
	abort = true
	// Anything before "Started" is a leftover of an earlier transfer (an "Aborted", a stray chunk).
	for {
		m, err := c.nextFileOf(ctx, in, "the start", suffixUpload)
		if err != nil {
			return abort, err
		}
		switch {
		case m.Command != nil:
			// A firmware without FileUpload (minimal, safeboot) echoed the command, password included.
			return abort, &deviceResetError{transferErr("the device does not support FileUpload (minimal or safeboot firmware?)")}
		case m.UpState == nil:
			continue
		case *m.UpState == "Started":
		case strings.HasPrefix(*m.UpState, "Error"):
			return abort, uploadErrorStatus(*m.UpState, typ)
		default:
			continue
		}
		break
	}

	// ack reads the {"Id","MaxSize"} after the start or a chunk.
	ack := func(what string) (int, error) {
		m, err := c.nextFileOf(ctx, in, what, suffixUpload)
		if err != nil {
			return 0, err
		}
		if m.UpState != nil {
			if strings.HasPrefix(*m.UpState, "Error") {
				return 0, uploadErrorStatus(*m.UpState, typ)
			}
			return 0, transferErr("the device ended the transfer (%s)", boundedStatus(*m.UpState))
		}
		if m.ID == nil || *m.ID != id {
			return 0, transferErr("a message of another transfer arrived")
		}
		if m.MaxSize == nil || *m.MaxSize <= 0 {
			return 0, transferErr("the device acknowledged %s without a usable chunk size", what)
		}
		return *m.MaxSize, nil
	}
	maxSize, err := ack("the start")
	if err != nil {
		return abort, err
	}
	// A second device on the topic answers the start too. Wait a moment for it: a second Started or
	// MaxSize for this Id is fatal and ends with FileUpload 0 (the first device is left idle).
	if c.moreAnswers(ctx, in, startSettle) {
		return abort, transferErr("more than one device answered the start; is another device using this topic?")
	}

	// The device sizes MaxSize from the topic it saw; the budget uses the topic we publish to.
	room := payloadRoom(c.bus.CommandTopic(topic, cmdUpload))
	sum := md5.New() //nolint:gosec // see import
	var done int64
	for done < size {
		n := int(min(int64(maxSize), int64(rawBudget(room, id)), size-done)) // the latest MaxSize, clamped
		if n <= 0 {
			return abort, transferErr("no chunk fits the device's MQTT buffer")
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return abort, transferErr("the data ended after %d of %d bytes", done, size)
		}
		sum.Write(buf)
		payload := `{"Id":` + strconv.Itoa(id) + `,"Data":"` + base64.StdEncoding.EncodeToString(buf) + `"}`
		// The last chunk of a firmware image completes it on the device. If its acknowledgement is lost the
		// image may be complete, and "FileUpload 0" would commit it (Update.end(true)): no abort then.
		last := typ == UploadFirmware && done+int64(n) == size
		if err := c.sendFileCmd(ctx, topic, cmdUpload, payload); err != nil {
			if last {
				return false, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
			}
			return abort, err
		}
		if maxSize, err = ack(fmt.Sprintf("the chunk at byte %d", done)); err != nil {
			if last && !deviceReset(err) {
				return false, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
			}
			return abort, err
		}
		if queuedAnswer(in) { // one chunk, one acknowledgement
			return abort, transferErr("more than one acknowledgement for the chunk at byte %d; is another device using this topic?", done)
		}
		done += int64(n)
		if progress != nil {
			progress(done)
		}
	}
	// Every chunk is acknowledged. An abort now would commit a complete Type 1 image (Update.end(true)).
	complete := typ == UploadFirmware
	if complete {
		abort = false
	}
	for range 8 { // a reader may return (0, nil); do not spin on it
		var extra [1]byte
		n, err := r.Read(extra[:])
		if n > 0 {
			return abort, transferErr("the data is longer than the announced %d bytes", size)
		}
		if err != nil {
			break
		}
	}

	md5msg := `{"Id":` + strconv.Itoa(id) + `,"Md5":"` + hex.EncodeToString(sum.Sum(nil)) + `"}`
	if err := c.sendFileCmd(ctx, topic, cmdUpload, md5msg); err != nil {
		if complete || errors.Is(err, tasmota.ErrTimeout) { // maybe published, or an image left complete
			return abort, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
		}
		return abort, err
	}
	m, err := c.nextFileOf(ctx, in, "the final confirmation", suffixUpload)
	if err != nil {
		return abort, fmt.Errorf("%w: %w", ErrOutcomeUnknown, err)
	}
	if m.UpState == nil {
		return abort, transferErr("the device answered the MD5 with something other than a status")
	}
	switch st := *m.UpState; {
	case st == "Done":
		return abort, nil
	case strings.HasPrefix(st, "Error"):
		return abort, uploadErrorStatus(st, typ)
	case st == "Failed" && typ == UploadFirmware:
		return abort, transferErr("the device rejected the image after the upload (Failed); nothing was switched")
	case st == "Failed":
		return abort, transferErr("the device rejected the settings dump after the upload (Failed)")
	case strings.EqualFold(st, "MD5 mismatch"):
		return abort, fmt.Errorf("%w: %w", errMD5Mismatch, transferErr("the device computed another MD5 than we sent; nothing was applied"))
	default:
		return abort, transferErr("the device ended the transfer (%s)", boundedStatus(st))
	}
}
