package transport

import (
	"context"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func (h *fsHTTP) RestoreConfig(_ context.Context, _ tasmota.Target, dump []byte) error {
	h.rec("restore " + string(dump))
	return h.fsErr
}

// Why (#430): a restore over HTTP is the device's own restore upload, sent once over HTTP. It never
// fails over to MQTT, the audit "via" says http, and a failed upload is recorded against the HTTP
// path like any other request.
func TestRestoreConfigGoesToHTTPOnly(t *testing.T) {
	r, h, tg, mem := newFSRig(t)
	ctx, tr := WithTrace(WithPath(t.Context(), HTTP))
	if err := r.RestoreConfig(ctx, tg, []byte("dmp")); err != nil {
		t.Fatal(err)
	}
	if len(h.fsCalls) != 1 || h.fsCalls[0] != "restore dmp" || tr.Via() != "http" {
		t.Fatalf("calls %v via %s", h.fsCalls, tr.Via())
	}
	h.fsErr = tasmota.ErrUnreachable
	if err := r.RestoreConfig(ctx, tg, []byte("dmp")); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("err %v", err)
	}
	if len(h.fsCalls) != 2 {
		t.Errorf("the failed restore was repeated or sent elsewhere: %v", h.fsCalls)
	}
	if _, _, failing := mem.HTTPFailure(tg.DeviceID()); !failing {
		t.Error("the unreachable device was not remembered as failing over HTTP")
	}
}

// Why (#430): a restore pinned to MQTT goes through FileUpload; the HTTP upload must refuse it
// before anything is sent.
func TestRestoreConfigRefusesAPinnedMQTTPath(t *testing.T) {
	r, h, tg, _ := newFSRig(t)
	err := r.RestoreConfig(WithPath(t.Context(), MQTT), tg, []byte("dmp"))
	if err == nil || len(h.fsCalls) != 0 {
		t.Fatalf("err %v calls %v", err, h.fsCalls)
	}
	// #432 review L1: the restore must fail at once, not wait for a restart that never comes.
	if !errors.Is(err, tasmota.ErrNotSent) {
		t.Errorf("err %v does not say that nothing was sent", err)
	}
}
