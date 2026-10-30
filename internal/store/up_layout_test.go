package store

import (
	"context"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the layout of the upload over MQTT is only ever what a read of /up showed; a made-up value or
// an unknown device is refused, and the layout survives a reload.
func TestSetUpLayout(t *testing.T) {
	s, _, _ := newStore(t)
	d, err := s.CreateDevice(context.Background(), model.Device{Name: "x", Address: "192.0.2.1", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDevice(context.Background(), d); got.UpLayout != "" {
		t.Fatalf("layout %q before any read", got.UpLayout)
	}
	if err := s.SetUpLayout(context.Background(), d, "direct"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDevice(context.Background(), d); got.UpLayout != "direct" {
		t.Fatalf("layout %q", got.UpLayout)
	}
	if err := s.SetUpLayout(context.Background(), d, "weird"); err == nil {
		t.Fatal("a made-up layout was stored")
	}
	if err := s.SetUpLayout(context.Background(), 9999, "direct"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device: %v", err)
	}
}

// Why (review #297, H1 residual): a layout read from /up proves two app partitions only for the hardware
// that was read. A replaced entry (other chip, other flash) and a probe that sees another chip or flash
// size must drop it, or an MQTT upload could be sent to a device whose layout nobody checked.
func TestUpLayoutIsDroppedWhenTheHardwareChanges(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	base := model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FlashSizeKB: 4096, ProgramFlashSizeKB: 2048, FreeKB: 1700}
	if err := s.UpdateIdentity(ctx, id, base); err != nil {
		t.Fatal(err)
	}
	read := func() model.Device {
		d, err := s.GetDevice(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	set := func() {
		if err := s.SetUpLayout(ctx, id, "direct"); err != nil {
			t.Fatal(err)
		}
	}
	set()
	if d := read(); d.UpLayout != "direct" || d.UpLayoutAt.IsZero() {
		t.Fatalf("not stored: %q %v", d.UpLayout, d.UpLayoutAt)
	}
	// the same hardware, a new firmware version: the layout stays proven
	same := base
	same.Version = "16.1.0"
	if err := s.UpdateIdentity(ctx, id, same); err != nil {
		t.Fatal(err)
	}
	if d := read(); d.UpLayout != "direct" {
		t.Fatalf("a new version dropped the layout: %q", d.UpLayout)
	}
	for name, change := range map[string]func(*model.Identity){
		"chip":   func(i *model.Identity) { i.Family = model.ChipESP32S3 },
		"flash":  func(i *model.Identity) { i.FlashSizeKB = 8192 },
		"sketch": func(i *model.Identity) { i.ProgramFlashSizeKB = 1024 },
		// a serial reflash from dual-OTA to safeboot keeps chip and flash sizes; Free (the other partition vs the
		// factory partition less the sketch) is what moves, and a dual layout keeps it constant across updates
		"free flash": func(i *model.Identity) { i.FreeKB = 697 },
	} {
		set()
		i := base
		change(&i)
		if err := s.UpdateIdentity(ctx, id, i); err != nil {
			t.Fatal(err)
		}
		if d := read(); d.UpLayout != "" || !d.UpLayoutAt.IsZero() {
			t.Fatalf("%s change kept the layout: %q %v", name, d.UpLayout, d.UpLayoutAt)
		}
		if err := s.UpdateIdentity(ctx, id, base); err != nil {
			t.Fatal(err)
		}
	}
	set()
	if _, err := s.SwapHardware(ctx, id, HardwareSwap{MAC: "AA:BB:CC:00:00:11", Address: "10.0.0.7", Port: 80, Identity: base}); err != nil {
		t.Fatal(err)
	}
	if d := read(); d.UpLayout != "" || !d.UpLayoutAt.IsZero() {
		t.Fatalf("a replaced entry kept the layout: %q", d.UpLayout)
	}
}

// Why: a read of /up that proved nothing drops the record, time stamp included.
func TestSetUpLayoutEmptyDropsTheRecord(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetUpLayout(ctx, id, "direct"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpLayout(ctx, id, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.UpLayout != "" || !d.UpLayoutAt.IsZero() {
		t.Fatalf("record kept: %q %v", d.UpLayout, d.UpLayoutAt)
	}
}
