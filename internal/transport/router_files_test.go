package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// fsHTTP is the raw HTTP client with the file system part; it records what reached it.
type fsHTTP struct {
	fakeHTTP
	fsCalls []string
	fsErr   error
}

func (h *fsHTTP) rec(c string) { h.mu.Lock(); h.fsCalls = append(h.fsCalls, c); h.mu.Unlock() }
func (h *fsHTTP) FSInfo(context.Context, tasmota.Target) (tasmota.FSInfo, error) {
	h.rec("info")
	return tasmota.FSInfo{Type: tasmota.FSLittleFS}, h.fsErr
}
func (h *fsHTTP) ListFiles(_ context.Context, _ tasmota.Target, d string) ([]tasmota.FileEntry, error) {
	h.rec("list " + d)
	return []tasmota.FileEntry{{Name: "a"}}, h.fsErr
}
func (h *fsHTTP) DownloadFile(_ context.Context, _ tasmota.Target, p string, _ int64) ([]byte, error) {
	h.rec("get " + p)
	return []byte("x"), h.fsErr
}
func (h *fsHTTP) UploadFile(_ context.Context, _ tasmota.Target, p string, _ []byte) error {
	h.rec("put " + p)
	return h.fsErr
}

func newFSRig(t *testing.T) (*Router, *fsHTTP, tasmota.Target, *Memory) {
	t.Helper()
	mem := NewMemory(DefaultFailFor, time.Now)
	h := &fsHTTP{fakeHTTP: fakeHTTP{reply: `{}`}}
	devs := &fakeDevices{d: model.Device{ID: 7, Address: "192.0.2.7", Topic: "plug", MQTT: model.MQTTOurs}}
	r := NewRouter(Options{HTTP: h, Devices: devs, Facts: &facts{mem: mem, groupKnown: true}, Memory: mem})
	r.SetMQTT(&fakeMQTT{reply: `{}`})
	return r, h, tasmota.TargetFor(devs.d), mem
}

func TestFileSystemCallsGoToHTTPAndAreTraced(t *testing.T) {
	// Why (#324): MQTT cannot carry files, so the router forwards every file call to HTTP and the
	// audit "via" of a backup that read files says http.
	r, h, tg, _ := newFSRig(t)
	ctx, tr := WithTrace(t.Context())
	if _, err := r.FSInfo(ctx, tg); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ListFiles(ctx, tg, "/"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DownloadFile(ctx, tg, "/a", 10); err != nil {
		t.Fatal(err)
	}
	if err := r.UploadFile(ctx, tg, "/a", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if got := len(h.fsCalls); got != 4 {
		t.Fatalf("HTTP saw %v", h.fsCalls)
	}
	if tr.Via() != "http" {
		t.Errorf("via %s", tr.Via())
	}
}

func TestFileSystemRefusesAPinnedMQTTPath(t *testing.T) {
	// Why (#324): a job pinned to MQTT (the device is only reachable that way) must not silently
	// reach for HTTP; the caller gets a distinct error and nothing is sent.
	r, h, tg, _ := newFSRig(t)
	ctx := WithPath(t.Context(), MQTT)
	for name, err := range map[string]error{
		"info":   func() error { _, e := r.FSInfo(ctx, tg); return e }(),
		"list":   func() error { _, e := r.ListFiles(ctx, tg, "/"); return e }(),
		"get":    func() error { _, e := r.DownloadFile(ctx, tg, "/a", 1); return e }(),
		"upload": r.UploadFile(ctx, tg, "/a", nil),
	} {
		if !errors.Is(err, ErrFilesNeedHTTP) {
			t.Errorf("%s: %v, want ErrFilesNeedHTTP", name, err)
		}
	}
	if len(h.fsCalls) != 0 {
		t.Errorf("HTTP was reached: %v", h.fsCalls)
	}
}

func TestFileSystemFailureFeedsTheHTTPMemory(t *testing.T) {
	// Why: an unreachable device found by a file call is as much evidence as one found by a command.
	r, h, tg, mem := newFSRig(t)
	h.fsErr = tasmota.ErrUnreachable
	if _, err := r.FSInfo(t.Context(), tg); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatal(err)
	}
	if _, _, failing := mem.HTTPFailure(7); !failing {
		t.Error("memory did not record the failure")
	}
}

func TestFileSystemWithoutHTTPSupportSaysNotImplemented(t *testing.T) {
	mem := NewMemory(DefaultFailFor, time.Now)
	r := NewRouter(Options{HTTP: &fakeHTTP{}, Devices: &fakeDevices{}, Memory: mem})
	if _, err := r.FSInfo(t.Context(), tasmota.TargetFor(model.Device{ID: 7, Address: "192.0.2.7"})); !errors.Is(err, tasmota.ErrNotImplemented) {
		t.Fatalf("got %v", err)
	}
}

func TestMQTTOnlyReadsTheTrace(t *testing.T) {
	// Why (#324): a backup whose dump came over MQTT must say "files need HTTP" instead of
	// trying HTTP, which is known to fail.
	ctx, tr := WithTrace(t.Context())
	if MQTTOnly(ctx) {
		t.Error("nothing recorded yet")
	}
	tr.note(MQTT)
	if !MQTTOnly(ctx) {
		t.Error("only MQTT carried calls")
	}
	tr.note(HTTP)
	if MQTTOnly(ctx) {
		t.Error("HTTP carried a call too")
	}
	if MQTTOnly(t.Context()) {
		t.Error("no trace at all")
	}
}
