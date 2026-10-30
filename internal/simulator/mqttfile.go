package simulator

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: the Tasmota file transfer protocol mandates MD5
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MQTT file transfer (FileDownload / FileUpload), modelled on
// tasmota/tasmota_xdrv_driver/xdrv_02_1_mqtt_file.ino of Tasmota v16.0.0.
// docs/research/tasmota-mqtt-transport.md cites the source line by line.
//
// The device keeps one shared state (FMqtt) for both directions and answers
// on stat/<topic>/FILEDOWNLOAD and stat/<topic>/FILEUPLOAD, not on RESULT.
// The simulator follows the source step by step, quirks included: a download
// advances one step per command, an abort of a running download first sends
// one more chunk, a file id above 255 is truncated to 8 bits.
//
// Not simulated: the binary variants (FileUpload<index above 299>, Download
// "Binary":1); TasmoFleet uses base64 only.

// Upload types (UploadTypes in Tasmota).
const (
	uplTasmota  = 1 // OTA firmware
	uplSettings = 2 // settings dump
)

const (
	// mqttMaxPacket is MQTT_MAX_PACKET_SIZE: the PubSubClient buffer of the
	// device. An incoming packet of this size or more closes the connection.
	mqttMaxPacket = 1200
	// fileHeaderSize is FileTransferHeaderSize, {"Id":116,"Data":""}<null>.
	fileHeaderSize = 21
	// responseSizeESP8266 and responseSizeESP32 are ResponseSize()
	// (MAX_LOGSZ = LOG_BUFFER_SIZE - 96) and decide the download chunk.
	responseSizeESP8266 = 4000
	responseSizeESP32   = 6000
	secMQTTRetry        = 10.0 // mqtt_retry: the device reconnects 10 s after a lost connection
	flashSector         = 4096
	settingsBufferMax   = 16 << 10 // SettingsBufferAlloc takes 2 to 16 KiB
	mmuPage             = 0x10000  // SPI_FLASH_MMU_PAGE_SIZE: ESP32 flash write areas are 64 kB aligned
)

// Faults of the file transfer (After is "" or "fileupload").
const (
	// FaultMQTTUploadCut: the link dies after Chunks acknowledged data chunks
	// (default 1): the device ignores everything but an abort, nothing is flashed.
	FaultMQTTUploadCut = "mqtt_upload_cut"
	// FaultMQTTUploadLostDone: the upload completes (flash written, restart
	// scheduled) but the final {"FileUpload":"Done"} is never published.
	FaultMQTTUploadLostDone = "mqtt_upload_lost_done"
)

// mqttFile is FMqtt.
type mqttFile struct {
	pos, size, typ uint32
	chunk          uint32 // upload: the payload size the device announces as MaxSize
	id             uint8  // file_id; 0 = no transfer
	active         bool   // file_buffer != nullptr
	buf            []byte // download: the file; upload: what arrived so far
	md5            string // upload: the MD5 the sender announced
	password       *string
	sum            hash.Hash
	topicSize      uint32 // length of the topic the command arrived on
	group          bool   // the command arrived on the group topic
	chunks         int    // upload data messages acknowledged (for FaultMQTTUploadCut)
}

func (m *mqttFile) reset() { // MqttFilePublish without a buffer
	m.id, m.size, m.typ = 0, 0, 0
	m.md5 = ""
	m.password = nil
}

func newMD5() hash.Hash { return md5.New() } //nolint:gosec // see import

// payloadOf is XdrvMailbox.payload: the number the command data parses to,
// -99 when it is not one.
func payloadOf(data string) int {
	switch strings.ToLower(data) {
	case "off", "false":
		return 0
	}
	if n, err := strconv.Atoi(data); err == nil {
		return n
	}
	return -99
}

// fileFields is the JSON a file command carries; Tasmota's parser upper-cases keys.
type fileFields map[string]any

func parseFileFields(data string) fileFields {
	if len(data) <= 8 { // "Workaround exception if empty JSON like {}"
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil
	}
	f := fileFields{}
	for k, v := range raw {
		f[strings.ToUpper(k)] = v
	}
	return f
}

func (f fileFields) uint(key string) (uint32, bool) {
	switch v := f[key].(type) {
	case float64:
		if v < 0 {
			return 0, true
		}
		return uint32(v), true
	case string:
		n, _ := strconv.ParseUint(v, 10, 32)
		return uint32(n), true
	}
	return 0, false
}

func (f fileFields) str(key string) (string, bool) {
	s, ok := f[key].(string)
	return s, ok
}

// fileOut collects the messages one file command publishes.
type fileOut struct {
	d    *simDevice
	cmd  string // "FileUpload" or "FileDownload"
	msgs []outMsg
	resp string // the Response buffer
	// skipFinal: nothing is published at the end (FaultMQTTUploadLostDone).
	skipFinal bool
}

func (o *fileOut) publish(payload string) {
	o.msgs = append(o.msgs, outMsg{topic: "stat/" + o.d.topic + "/" + strings.ToUpper(o.cmd), payload: payload})
}

func (o *fileOut) cmndChar(text string) { o.resp = `{"` + o.cmd + `":` + jsonString(text) + `}` }

// validate is MqttFileValidate: a non-zero error ends the transfer.
func (o *fileOut) validate(code uint32) {
	if code == 0 {
		return
	}
	o.d.mf.active = false
	if code == 4 {
		o.cmndChar("Aborted")
		return
	}
	o.cmndChar(fmt.Sprintf("Error %d", code))
}

// finish is MqttFilePublish.
func (o *fileOut) finish() {
	if !o.d.mf.active {
		o.d.mf.reset()
	}
	if !o.skipFinal {
		o.publish(o.resp) // MqttFilePublish always publishes, an empty response too
	}
}

func (d *simDevice) passwordOKLocked() bool {
	return d.mf.password != nil && *d.mf.password == d.get("mqttpassword")
}

// headRoomLocked is (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096,
// the room Tasmota has for an intermediate upload, derived from what Status 4
// reports (ProgramSize and Free, in kB).
//
//   - ESP8266: from the sector after the sketch plus 2 to the file system
//     start minus 2 (support_esp8266.ino).
//   - ESP32 with one app partition (safeboot layout): from the running
//     partition address plus the sketch plus 128 kB to the end of the
//     partition, both rounded down to 64 kB (support_esp32.ino).
//   - ESP32 with two app partitions: the other partition, 128 kB in, to its end.
func (d *simDevice) headRoomLocked() int {
	mem := d.render("status-4.json", d.viewLocked())
	num := func(re string) int {
		m := regexp.MustCompile(re).FindSubmatch(mem)
		if m == nil {
			return 0
		}
		n, _ := strconv.Atoi(string(m[1]))
		return n
	}
	sketch, free := num(`"ProgramSize":(\d+)`)*1024, num(`"Free":(\d+)`)*1024
	down := func(n int) int { return n / mmuPage * mmuPage }
	switch {
	case !d.esp32:
		// Free is the room from the sketch (rounded up to a sector) to the file system start.
		fsStart := (sketch+flashSector-1)/flashSector*flashSector + free
		return max(0, (fsStart/flashSector-2-(sketch/flashSector+2))*flashSector)
	case d.def.DualOTA:
		const other = 0x200000 // the second app partition
		return max(0, down(other+free)-down(other+2*mmuPage))
	default:
		const running = mmuPage
		return max(0, down(running+sketch+free)-down(running+sketch+2*mmuPage))
	}
}

// fileUploadLocked is CmndFileUpload.
func (d *simDevice) fileUploadLocked(data string) ([]byte, []outMsg) {
	m := &d.mf
	o := &fileOut{d: d, cmd: "FileUpload"}
	f := parseFileFields(data)
	var rcvID uint32
	var b64 *string
	if v, ok := f.uint("ID"); ok {
		rcvID = v
	}
	if v, ok := f.uint("TYPE"); ok {
		m.typ = v
	}
	if v, ok := f.uint("SIZE"); ok {
		m.size = v
	}
	if v, ok := f.str("MD5"); ok {
		m.md5 = v
	}
	if v, ok := f.str("DATA"); ok {
		b64 = &v
	}
	if v, ok := f.str("PASSWORD"); ok {
		m.password = &v
	}
	_, cutSilent := d.uploadCutLocked(m, b64 != nil)
	if cutSilent {
		return nil, nil
	}
	o.validate(d.uploadValidateLocked(o, rcvID, payloadOf(data)))
	if m.active {
		d.uploadStepLocked(o, b64)
	}
	o.finish()
	return []byte(o.resp), o.msgs
}

// uploadCutLocked implements FaultMQTTUploadCut: after N acknowledged data
// chunks the device answers nothing (except to an abort).
func (d *simDevice) uploadCutLocked(m *mqttFile, isData bool) (Fault, bool) {
	cuts := d.faultsForLocked("fileupload", FaultMQTTUploadCut)
	if len(cuts) == 0 || !m.active || !isData {
		return Fault{}, false
	}
	n := max(cuts[0].Chunks, 1)
	return cuts[0], m.chunks >= n
}

// uploadValidateLocked is MqttFileUploadValidate.
func (d *simDevice) uploadValidateLocked(o *fileOut, rcvID uint32, payload int) uint32 {
	m := &d.mf
	if m.group {
		return 5
	}
	switch {
	case m.id == 0 && rcvID > 0 && m.size > 0 && m.typ > 0:
		m.active = false
		if !d.passwordOKLocked() {
			return 1
		}
		if m.typ == uplSettings {
			if m.size > settingsBufferMax {
				return 2 // SettingsBufferAlloc: Settings supports max size
			}
		} else {
			rounded := (int(m.size) + flashSector - 1) &^ (flashSector - 1)
			if rounded > d.headRoomLocked() {
				return 2 // not enough space
			}
			if m.typ != uplTasmota {
				return 3 // invalid file type
			}
		}
		m.active = true     // the settings buffer, or Update.begin() succeeded
		m.id = uint8(rcvID) //nolint:gosec // G115: file_id is a uint8_t on the device, the truncation is the behaviour
		m.pos = 0
		m.buf = nil
		m.sum = newMD5()
		m.chunks = 0
		o.cmndChar("Started")
		o.publish(o.resp)
		// chunk_size = buffer size - 5 byte header - topic length - 1
		m.chunk = uint32(mqttMaxPacket) - 5 - m.topicSize - 1
	case (m.id > 0 && uint32(m.id) != rcvID) || payload == 0:
		if m.active && m.typ == uplTasmota && m.size > 0 && m.pos >= m.size {
			// MqttFileUploadAbort runs Update.end(true) (xdrv_02_1_mqtt_file.ino): a COMPLETE image becomes
			// the next boot image although the MD5 never arrived, a partial one is rejected by the ESP32.
			if next, ok := d.uploadedImageLocked(m.buf); ok {
				d.nextImage = next
			}
		}
		m.buf = nil
		return 4 // upload aborted
	}
	return 0
}

// uploadStepLocked is the body of CmndFileUpload once a buffer exists.
func (d *simDevice) uploadStepLocked(o *fileOut, b64 *string) {
	m := &d.mf
	if m.pos < m.size && b64 != nil {
		raw, err := base64.StdEncoding.DecodeString(*b64)
		if err != nil {
			raw = nil // decode_base64 stops at what it cannot read; the simulator keeps nothing
		}
		left := m.size - m.pos
		n := min(uint32(len(raw)), left) //nolint:gosec // G115: a message is below 1200 bytes
		m.sum.Write(raw[:n])
		m.buf = append(m.buf, raw[:n]...)
		m.pos += n
		m.chunks++
	}
	if m.pos < m.size || len(m.md5) != 32 {
		if m.pos == 0 { // "!binary_data && !FMqtt.file_pos": converted again on every message at position 0
			m.chunk = (m.chunk-fileHeaderSize)/4*3 - 2
		}
		o.resp = fmt.Sprintf(`{"Id":%d,"MaxSize":%d}`, m.id, m.chunk)
		return
	}
	sum := hex.EncodeToString(m.sum.Sum(nil))
	if !strings.EqualFold(m.md5, sum) {
		o.cmndChar("MD5 mismatch")
		m.active = false
		return
	}
	o.cmndChar("Done")
	failed := false
	var after func()
	switch m.typ {
	case uplSettings:
		if len(m.buf) != configDumpSize {
			failed = true
			break
		}
		d.applyConfigLocked(bytes.Clone(m.buf))
		after = func() { d.reboot(false) }
	case uplTasmota:
		next, ok := d.uploadedImageLocked(m.buf)
		if !ok {
			failed = true
			break
		}
		d.armLocked("fileupload")
		d.nextImage = next
		after = func() { d.reboot(true) }
	}
	m.active = false
	m.buf = nil
	if failed {
		o.cmndChar("Failed")
		return
	}
	if len(d.faultsForLocked("fileupload", FaultMQTTUploadLostDone)) > 0 {
		o.skipFinal = true
	}
	d.spawn(func() {
		if d.sleep(secRestartCmd) {
			after()
		}
	})
}

// uploadedImageLocked is Update.write + Update.end(true) for a firmware sent
// over MQTT: the same bootloader header check as the /u2 upload. The file
// name is not part of the protocol, so the image kind is the plain build
// (a device running safeboot or minimal cannot take a Type 1 upload anyway).
func (d *simDevice) uploadedImageLocked(body []byte) (*flashed, bool) {
	if len(body) == 0 || (body[0] != 0xE9 && body[0] != 0x1F) {
		return nil, false
	}
	raw, err := unpack(body)
	if err != nil {
		return nil, false
	}
	fam, err := imageFamily(raw)
	if err != nil || d.chipFamily == "" || fam != d.chipFamily || d.hasFaultLocked(FaultWrongFamily) {
		return nil, false
	}
	version, ok := findVersion(raw)
	if !ok {
		version = d.f.x.FlashVersion
	}
	kind := d.image
	if d.minimalLocked() {
		kind = imageKind("", d.esp32)
	}
	return &flashed{kind: kind, version: version}, true
}

// fileDownloadLocked is CmndFileDownload: one step per command.
func (d *simDevice) fileDownloadLocked(data string) ([]byte, []outMsg) {
	m := &d.mf
	o := &fileOut{d: d, cmd: "FileDownload"}
	payload := payloadOf(data)
	if m.active {
		if m.pos < m.size {
			responseSize := responseSizeESP8266
			if d.esp32 {
				responseSize = responseSizeESP32
			}
			chunk := uint32((responseSize-fileHeaderSize)/4*3 - 2)
			n := min(m.size-m.pos, chunk)
			part := m.buf[m.pos : m.pos+n]
			m.sum.Write(part)
			m.pos += n
			o.publish(fmt.Sprintf(`{"Id":%d,"Data":"%s"}`, m.id, base64.StdEncoding.EncodeToString(part)))
			o.resp = ""
			if payload != 0 {
				return nil, o.msgs
			}
		} else {
			o.publish(fmt.Sprintf(`{"Id":%d,"Md5":"%s"}`, m.id, hex.EncodeToString(m.sum.Sum(nil))))
			o.cmndChar("Done")
			m.active = false
			m.buf = nil
		}
	}
	if f := parseFileFields(data); f != nil {
		if v, ok := f.uint("TYPE"); ok {
			m.typ = v
		}
		if v, ok := f.str("PASSWORD"); ok {
			m.password = &v
		}
	}
	o.validate(d.downloadValidateLocked(o, payload))
	o.finish()
	return []byte(o.resp), o.msgs
}

// downloadValidateLocked is MqttFileDownloadValidate.
func (d *simDevice) downloadValidateLocked(o *fileOut, payload int) uint32 {
	m := &d.mf
	if m.group {
		return 5
	}
	switch {
	case m.id == 0 && m.typ > 0:
		m.active = false
		if !d.passwordOKLocked() {
			return 1
		}
		m.id = uint8(time.Now().Unix()&0xFE) + 1 // odd id between 1 and 255
		if m.typ != uplSettings {
			return 3
		}
		dump := d.configDump
		if dump == nil {
			dump = d.dumpLocked()
		}
		m.buf, m.size, m.active = bytes.Clone(dump), uint32(len(dump)), true //nolint:gosec // G115: a dump is 4096 bytes
		name := "Config_" + strings.ReplaceAll(d.topic, "-", "_") + "_" + d.version + ".dmp"
		o.resp = fmt.Sprintf(`{"File":%s,"Id":%d,"Type":%d,"Size":%d}`, jsonString(name), m.id, m.typ, m.size)
		m.pos = 0
		m.sum = newMD5()
		o.msgs = append(o.msgs, outMsg{topic: "stat/" + d.topic + "/FILEDOWNLOAD", payload: `{"FileDownload":"Started"}`})
	case payload == 0:
		m.buf = nil
		return 4
	}
	return 0
}
