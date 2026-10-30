package fwselect

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

func esp32Free(freeKB int) model.Identity {
	return model.Identity{Family: model.ChipESP32, Hardware: "ESP32-D0WD-V3", Version: "16.0.0", Variant: "tasmota", FreeKB: freeKB}
}

// The live safeboot ESP32 reports Free 697 KB. A full image (about 2.2 MB) can never be staged
// there, and the device would answer "Not enough space" only after TasmoFleet had sent a transfer
// request; the fit rule must say no up front (D19: never try-and-see).
func TestMQTTUploadFitsRefusesTheLiveSafebootDevice(t *testing.T) {
	for _, layout := range []UpLayoutHint{UpLayoutDirect} {
		ok, need, room, reason := MQTTUploadFits(esp32Free(697), layout, 2200*1024)
		if ok {
			t.Fatalf("layout %q: a 2.2 MB image passed on 697 KB free", layout)
		}
		if need != 2200 || room != 697-192 {
			t.Errorf("layout %q: need/room = %d/%d KB, want 2200/505", layout, need, room)
		}
		if !strings.Contains(reason, "2200 KB") || !strings.Contains(reason, "505 KB") {
			t.Errorf("layout %q: reason %q does not name the numbers", layout, reason)
		}
	}
}

// The bound is round_up_4k(size) <= Free - 192 KiB, matching Tasmota's own check
// (rounded_size > head_room refuses). One byte over a 4 KiB boundary costs a whole sector.
func TestMQTTUploadFitsBoundary(t *testing.T) {
	const freeKB = 2048 + 192
	tests := []struct {
		name string
		size int64
		want bool
	}{
		{"exactly at the bound", 2048 * 1024, true},
		{"4095 bytes below the bound still rounds to it", 2048*1024 - 4095, true},
		{"one byte over rounds up a whole sector", 2048*1024 + 1, false},
		{"4 KB over", 2048*1024 + 4096, false},
		{"one sector", 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ok, _, _, reason := MQTTUploadFits(esp32Free(freeKB), UpLayoutDirect, tt.size); ok != tt.want {
				t.Errorf("fits = %v, want %v (%s)", ok, tt.want, reason)
			}
		})
	}
	// One KB less free flash moves the bound below an image that fitted.
	if ok, _, _, _ := MQTTUploadFits(esp32Free(freeKB-1), UpLayoutDirect, 2048*1024); ok {
		t.Error("an image exactly at the bound passed with 1 KB less free flash")
	}
}

// On a dual-OTA device Status 4 Free is the size of the other partition and the running sketch
// is irrelevant to the upload (v16 support_esp32.ino). The esp32-dual simulator fixture reports
// ProgramSize 2182 KB next to Free 2880 KB (the other partition); a rule that subtracted the sketch
// would leave 698 KB and refuse an image that Tasmota accepts.
func TestMQTTUploadFitsIgnoresTheSketchSize(t *testing.T) {
	id := esp32Free(2880)
	id.ProgramFlashSizeKB, id.FlashSizeKB = 2182, 8192
	ok, _, room, reason := MQTTUploadFits(id, UpLayoutDirect, 2500*1024)
	if !ok || room != 2880-192 {
		t.Fatalf("2500 KB on a 2880 KB partition: ok = %v room = %d (%s)", ok, room, reason)
	}
}

// A known safeboot layout (one app partition) is refused outright, however much room Free reports:
// Tasmota's MQTT FileUpload calls Update.begin without the single-OTA guard, so the upload could
// target the running partition (v16 xdrv_02_1_mqtt_file.ino). The other hints never change the
// numeric decision (the layout is unknown for most devices over MQTT).
func TestMQTTUploadFitsRefusesTheSafebootLayout(t *testing.T) {
	ok, need, room, reason := MQTTUploadFits(esp32Free(8000), UpLayoutSafeboot, 100*1024)
	if ok || need != 0 || room != 0 || !strings.Contains(reason, "safeboot") {
		t.Errorf("ok/need/room = %v/%d/%d reason %q, want a refusal naming the layout", ok, need, room, reason)
	}
	for _, layout := range []UpLayoutHint{UpLayoutUnknown, "", "garbage"} {
		if ok, _, _, reason := MQTTUploadFits(esp32Free(2240), layout, 500*1024); ok || !strings.Contains(reason, "not proven dual-OTA") {
			t.Errorf("layout %q: ok %v reason %q, want the layout-not-proven refusal", layout, ok, reason)
		}
	}
}

// Why (review #297 H1, brick risk): Status 4 Free looks the same on a safeboot-shaped device (factory
// partition less sketch) and on a dual-OTA one, and a safeboot device that starts a FileUpload Type 1
// may overwrite its running app. A small image on a safeboot-shaped Status 4 must be refused unless
// the layout was PROVEN direct.
func TestMQTTUploadFitsRefusesASafebootShapedDeviceWithAnUnprovenLayout(t *testing.T) {
	id := esp32Free(1400) // a 1.4 MB factory partition left after a 0.6 MB sketch: plenty "free"
	if ok, _, _, reason := MQTTUploadFits(id, UpLayoutUnknown, 64*1024); ok || !strings.Contains(reason, "not proven dual-OTA") {
		t.Fatalf("unknown layout: ok %v reason %q", ok, reason)
	}
	if ok, _, _, _ := MQTTUploadFits(id, UpLayoutDirect, 64*1024); !ok {
		t.Fatal("a proven direct layout with room must pass")
	}
}

// Every other refusal states its own cause, so a person (and the path chooser's error) can tell
// "this device never can" from "this image is too big".
func TestMQTTUploadFitsRefusals(t *testing.T) {
	notKnown := esp32Free(3000)
	notKnown.Version = ""
	esp8266 := esp32Free(3000)
	esp8266.Family = model.ChipESP8266
	unknownFamily := esp32Free(3000)
	unknownFamily.Family = model.ChipUnknown
	minimal := esp32Free(3000)
	minimal.Variant = "minimal"
	safeboot := esp32Free(3000)
	safeboot.Variant = "safeboot"
	tests := []struct {
		name string
		id   model.Identity
		size int64
		want string
	}{
		{"identity not known", notKnown, 1 << 20, "identity is not known"},
		{"esp8266", esp8266, 1 << 20, "ESP8266"},
		{"unknown family", unknownFamily, 1 << 20, "identity is not known"},
		{"minimal", minimal, 1 << 20, "minimal firmware"},
		{"safeboot", safeboot, 1 << 20, "safeboot firmware"},
		{"free unknown", esp32Free(0), 1 << 20, "free flash"},
		{"free negative", esp32Free(-5), 1 << 20, "free flash"},
		{"size unknown", esp32Free(3000), 0, "image size"},
	}
	seen := map[string]string{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, need, room, reason := MQTTUploadFits(tt.id, UpLayoutDirect, tt.size)
			if ok || need != 0 || room != 0 {
				t.Fatalf("ok/need/room = %v/%d/%d, want a refusal without numbers", ok, need, room)
			}
			if !strings.Contains(reason, tt.want) {
				t.Errorf("reason %q does not contain %q", reason, tt.want)
			}
			// An unknown family is an unknown identity; a negative Free is an unknown Free.
			if prev, dup := seen[reason]; dup && tt.name != "free negative" && tt.name != "unknown family" {
				t.Errorf("reason shared with %q: %q", prev, reason)
			}
			seen[reason] = tt.name
		})
	}
}

// Free below the 192 KiB margin leaves no room at all; the clamp must not turn into a pass.
func TestMQTTUploadFitsFreeBelowTheMargin(t *testing.T) {
	ok, _, room, _ := MQTTUploadFits(esp32Free(100), UpLayoutDirect, 4096)
	if ok || room != 0 {
		t.Errorf("ok = %v room = %d, want a refusal with room 0", ok, room)
	}
}

// Every ESP32 chip family takes the upload (same support_esp32.ino), not only the classic ESP32.
func TestMQTTUploadFitsAllESP32Families(t *testing.T) {
	for _, fam := range []model.ChipFamily{model.ChipESP32, model.ChipESP32S2, model.ChipESP32S3, model.ChipESP32C2, model.ChipESP32C3, model.ChipESP32C5, model.ChipESP32C6, model.ChipESP32C61, model.ChipESP32P4} {
		id := esp32Free(2240)
		id.Family = fam
		if ok, _, _, reason := MQTTUploadFits(id, UpLayoutDirect, 1<<20); !ok {
			t.Errorf("%s: %s", fam, reason)
		}
	}
}

// Why: a layout without a read time is no proof; only a recorded read of "direct" or "safeboot" counts.
func TestUpLayoutOf(t *testing.T) {
	at := time.Unix(1, 0)
	for _, tc := range []struct {
		stored string
		at     time.Time
		want   UpLayoutHint
	}{
		{"direct", at, UpLayoutDirect}, {"safeboot", at, UpLayoutSafeboot}, {"direct", time.Time{}, UpLayoutUnknown},
		{"", at, UpLayoutUnknown}, {"weird", at, UpLayoutUnknown},
	} {
		if got := UpLayoutOf(tc.stored, tc.at); got != tc.want {
			t.Errorf("%q %v: %q, want %q", tc.stored, tc.at, got, tc.want)
		}
	}
}
