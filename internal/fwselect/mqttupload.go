package fwselect

import (
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// UpLayoutHint is what an HTTP read of the device's /up page showed about its flash layout (#30).
// A known safeboot layout is refused outright; the other values never let an image pass that the
// bound below does not prove (an unknown layout is the normal case over MQTT).
type UpLayoutHint string

// Layout hints.
const (
	UpLayoutUnknown  UpLayoutHint = "unknown"
	UpLayoutDirect   UpLayoutHint = "direct"   // two app partitions: the image goes into the other one
	UpLayoutSafeboot UpLayoutHint = "safeboot" // one app partition next to safeboot: refused
)

// MQTT FileUpload Type 1 constants (Tasmota v16.0.0).
const (
	// mqttUploadSector is SPI_FLASH_SEC_SIZE: MqttFileUploadValidate rounds the size up to it.
	mqttUploadSector = 4096
	// mqttUploadMarginKB is the part of Status 4 "Free" that head_room can never use, see the
	// derivation on MQTTUploadFits.
	mqttUploadMarginKB = 192
)

// MQTTUploadFits decides, before anything is sent, whether Tasmota will accept an image of
// imageSize bytes as FileUpload Type 1 (xdrv_02_1_mqtt_file.ino, MqttFileUploadValidate):
//
//	head_room    = (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096
//	rounded_size = round_up_4096(file_size)
//	refused ("Not enough space", error 2) when rounded_size > head_room
//
// The device is never asked to try. The rule, valid for both ESP32 layouts without knowing which
// one the device has:
//
//	round_up_4k(imageSize) <= FreeKB*1024 - 192 KiB      (FreeKB = Status 4 "Free")
//
// Why 192 KiB (support_esp32.ino, v16.0.0):
//   - FlashWriteStartSector = (EspFlashBaseAddress + 128 KiB) rounded down to 64 KiB,
//     FlashWriteMaxSector = EspFlashBaseEndAddress rounded down to 64 KiB.
//   - Two app partitions (direct): EspFlashBaseAddress is the start of the OTHER partition and
//     EspFlashBaseEndAddress its end, and ESP_getFreeSketchSpace() (Status 4 Free) is that
//     partition's size. So head_room > Free - 128 KiB - 64 KiB (rounding the end down loses less
//     than 64 KiB), and the size of the running sketch plays no part.
//   - One app partition (safeboot layout): EspFlashBaseAddress is running->address + sketch size,
//     the end is the partition's end, and Free is partition size - sketch size. Again
//     head_room > Free - 128 KiB - 64 KiB. (That layout is refused anyway, see below.)
//   - Status 4 prints Free in whole KB (rounded down), which only makes FreeKB smaller than the
//     real value and the bound more conservative. 128 + 64 = 192 KiB.
//
// Refused outright, each with its own reason: identity not Known, not an ESP32 family (the only
// other family is ESP8266, which has no second stage: minimal firmware has no FileUpload), variant
// minimal or safeboot (neither has FileUpload), a known safeboot layout (one app partition:
// MqttFileUploadValidate calls Update.begin without the single-OTA handling of the web upload, so the
// next update partition can be the running one), Free unknown, image size unknown. needKB is the rounded image size and roomKB what
// the bound allows (0 when Free is below the margin or unknown).
func MQTTUploadFits(id model.Identity, layout UpLayoutHint, imageSize int64) (ok bool, needKB, roomKB int, reason string) {
	switch {
	case !id.Known():
		return false, 0, 0, "the device's identity is not known, so an upload over MQTT is not attempted"
	case id.Family == model.ChipESP8266 || !id.Family.IsESP32():
		return false, 0, 0, "an ESP8266 cannot take an upload over MQTT: its minimal firmware has no FileUpload"
	case id.Variant == variantMinimal || id.Variant == variantSafeboot:
		return false, 0, 0, fmt.Sprintf("the device runs %s firmware, which has no FileUpload", id.Variant)
	case layout == UpLayoutSafeboot:
		// MqttFileUploadValidate calls Update.begin(file_size) without the single-OTA handling the web
		// upload has; with one app partition the next update partition can be the running one. Until
		// that is settled for the Arduino core in use, an upload over MQTT is never started there.
		return false, 0, 0, "the device has the safeboot (single app partition) layout, where an upload over MQTT could overwrite the running firmware"
	case layout != UpLayoutDirect:
		// Status 4 Free cannot tell the layouts apart (a safeboot device reports the factory partition
		// less the sketch, a dual device the other partition), and on the safeboot layout Tasmota's
		// FileUpload calls Update.begin without the single-OTA handling, so the next update partition can
		// be the running app: the device would fall into safeboot, which has no MQTT. Only a layout read
		// from the device's /up page (HTTP, earlier) proves two app partitions.
		return false, 0, 0, "the device's flash layout is not proven dual-OTA (it is only known from an earlier read of its /up page over HTTP)"
	case id.FreeKB <= 0:
		return false, 0, 0, "the device's free flash (Status 4) is unknown"
	case imageSize <= 0:
		return false, 0, 0, "the image size is unknown"
	}
	need := (imageSize + mqttUploadSector - 1) / mqttUploadSector * mqttUploadSector
	needKB = int((need + 1023) / 1024)
	roomKB = max(id.FreeKB-mqttUploadMarginKB, 0)
	if need > int64(roomKB)*1024 {
		return false, needKB, roomKB, fmt.Sprintf("the image does not fit for an upload over MQTT (needs %d KB, %d KB of the %d KB free are usable)", needKB, roomKB, id.FreeKB)
	}
	return true, needKB, roomKB, fmt.Sprintf("the image fits for an upload over MQTT (needs %d KB, %d KB usable)", needKB, roomKB)
}

// UpLayoutOf turns the layout the store recorded (Device.UpLayout, read at readAt) into a hint. A layout
// without a read time is no proof and counts as unknown.
func UpLayoutOf(stored string, readAt time.Time) UpLayoutHint {
	if readAt.IsZero() {
		return UpLayoutUnknown
	}
	switch stored {
	case string(UpLayoutDirect):
		return UpLayoutDirect
	case string(UpLayoutSafeboot):
		return UpLayoutSafeboot
	}
	return UpLayoutUnknown
}
