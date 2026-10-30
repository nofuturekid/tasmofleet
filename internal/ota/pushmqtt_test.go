package ota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Review #297 (fail-open audit): every state the upload through the broker reads must refuse the upload
// when it is missing, zero, unknown or unreadable. These rows pin each default.

func goodIdentity() model.Identity {
	return model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota32", FreeKB: 2000}
}

func goodArtifact() model.Artifact {
	return model.Artifact{Filename: "tasmota32.bin", Size: 1 << 20, SHA256: strings.Repeat("a", 64)}
}

func TestMQTTPushRefusalDefaultsToRefuse(t *testing.T) {
	read := time.Unix(1, 0)
	e := &Engine{mqttUpload: true}
	direct := fwselect.UpLayoutOf("direct", read)
	if err := e.mqttPushRefusal(1, goodIdentity(), direct, model.PlanDirect, goodArtifact()); err != nil {
		t.Fatalf("the baseline must pass: %v", err)
	}
	noFree := goodIdentity()
	noFree.FreeKB = 0
	unknownChip := goodIdentity()
	unknownChip.Family = model.ChipUnknown
	noVersion := goodIdentity()
	noVersion.Version = ""
	noSize := goodArtifact()
	noSize.Size = 0
	for name, tc := range map[string]struct {
		id     model.Identity
		layout fwselect.UpLayoutHint
		plan   model.Plan
		a      model.Artifact
		gate   func(*Engine)
		off    bool
	}{
		"layout never read":           {goodIdentity(), fwselect.UpLayoutOf("", time.Time{}), model.PlanDirect, goodArtifact(), nil, false},
		"layout without a read time":  {goodIdentity(), fwselect.UpLayoutOf("direct", time.Time{}), model.PlanDirect, goodArtifact(), nil, false},
		"layout of another value":     {goodIdentity(), fwselect.UpLayoutOf("dual", read), model.PlanDirect, goodArtifact(), nil, false},
		"safeboot layout":             {goodIdentity(), fwselect.UpLayoutOf("safeboot", read), model.PlanDirect, goodArtifact(), nil, false},
		"free flash unknown":          {noFree, direct, model.PlanDirect, goodArtifact(), nil, false},
		"identity without a chip":     {unknownChip, direct, model.PlanDirect, goodArtifact(), nil, false},
		"identity without a version":  {noVersion, direct, model.PlanDirect, goodArtifact(), nil, false},
		"zero identity":               {model.Identity{}, direct, model.PlanDirect, goodArtifact(), nil, false},
		"image size unknown":          {goodIdentity(), direct, model.PlanDirect, noSize, nil, false},
		"plan not set":                {goodIdentity(), direct, "", goodArtifact(), nil, false},
		"staged plan":                 {goodIdentity(), direct, model.PlanStaged, goodArtifact(), nil, false},
		"build gate closed":           {goodIdentity(), direct, model.PlanDirect, goodArtifact(), nil, true},
		"rearm failed for the device": {goodIdentity(), direct, model.PlanDirect, goodArtifact(), func(e *Engine) { e.uploadGate.blockDevice(1) }, false},
		"jobs could not be listed":    {goodIdentity(), direct, model.PlanDirect, goodArtifact(), func(e *Engine) { e.uploadGate.blockAll() }, false},
	} {
		t.Run(name, func(t *testing.T) {
			e := &Engine{mqttUpload: !tc.off}
			if tc.gate != nil {
				tc.gate(e)
			}
			if err := e.mqttPushRefusal(1, tc.id, tc.layout, tc.plan, tc.a); err == nil {
				t.Fatal("the upload was not refused")
			}
		})
	}
	// the gate of one device does not refuse another
	e = &Engine{mqttUpload: true}
	e.uploadGate.blockDevice(1)
	if err := e.mqttPushRefusal(2, goodIdentity(), direct, model.PlanDirect, goodArtifact()); err != nil {
		t.Fatalf("another device was refused: %v", err)
	}
}

type filesFW struct {
	Firmware
	data []byte
}

func (f filesFW) Open(context.Context, model.Artifact) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type uploaderAPI struct{ DeviceAPI }

func (uploaderAPI) UploadFirmwareMQTT(context.Context, model.Device, string, int64, io.Reader, func(int64)) error {
	return nil
}

// Why: the bytes that are sent must match the checksum the cache recorded; a missing checksum is no match.
func TestMQTTPushImageReadRefusesWithoutAMatchingChecksum(t *testing.T) {
	img := append([]byte{0xE9, 1, 0, 0}, make([]byte, 60)...)
	e := &Engine{fw: filesFW{data: img}, api: uploaderAPI{}}
	a := model.Artifact{Filename: "tasmota32.bin", Size: int64(len(img))}
	for name, sum := range map[string]string{"empty": "", "wrong": strings.Repeat("0", 64)} {
		a.SHA256 = sum
		if _, err := e.readMQTTImage(context.Background(), model.Identity{Family: model.ChipESP32}, a); err == nil || !isRefusal(err) {
			t.Errorf("%s checksum: %v", name, err)
		}
	}
	a.Size = 0
	if _, err := e.readMQTTImage(context.Background(), model.Identity{Family: model.ChipESP32}, a); err == nil {
		t.Error("a zero size was read")
	}
}

type failingJobs struct {
	store.JobStore
	err error
}

func (f failingJobs) ListJobs(context.Context, store.JobFilter) ([]model.Job, error) {
	return nil, f.err
}

type unreadableDevices struct {
	store.DeviceStore
}

func (unreadableDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return model.Device{}, errors.New("store down")
}

// Why: if the engine cannot find out which devices hold an unconfirmed image, or cannot set the block
// again, nothing may be uploaded to them in this process.
func TestMQTTPushRearmFailuresRefuseUploads(t *testing.T) {
	jobs := model.Job{ID: 1, DeviceID: 3, Delivery: model.DeliveryPush, Transport: model.TransportMQTT, MQTTUpload: true, MQTTUploadState: model.MQTTUploadUnconfirmed}
	t.Run("jobs cannot be listed", func(t *testing.T) {
		e := &Engine{jobs: failingJobs{err: errors.New("db down")}, log: slog.New(slog.DiscardHandler)}
		e.rearmFinished(context.Background())
		if e.uploadGate.refusal(9) == nil {
			t.Fatal("uploads are still allowed")
		}
	})
	t.Run("device API cannot re-arm", func(t *testing.T) {
		e := &Engine{api: &fakeAPI{}, log: slog.New(slog.DiscardHandler)}
		e.rearmUpload(context.Background(), jobs)
		if e.uploadGate.refusal(3) == nil || e.uploadGate.refusal(4) != nil {
			t.Fatal("only the device of the job must be refused")
		}
	})
	t.Run("device cannot be read", func(t *testing.T) {
		e := &Engine{api: rearmAPI{}, devices: unreadableDevices{}, log: slog.New(slog.DiscardHandler)}
		e.rearmUpload(context.Background(), jobs)
		if e.uploadGate.refusal(3) == nil {
			t.Fatal("uploads are still allowed")
		}
	})
	t.Run("re-arm fails", func(t *testing.T) {
		e := &Engine{api: rearmAPI{err: errors.New("no channel")}, devices: okDevices{}, log: slog.New(slog.DiscardHandler)}
		e.rearmUpload(context.Background(), jobs)
		if e.uploadGate.refusal(3) == nil {
			t.Fatal("uploads are still allowed")
		}
	})
	t.Run("re-arm works", func(t *testing.T) {
		e := &Engine{api: rearmAPI{}, devices: okDevices{}, log: slog.New(slog.DiscardHandler)}
		e.rearmUpload(context.Background(), jobs)
		if e.uploadGate.refusal(3) != nil {
			t.Fatal("a successful re-arm must not refuse")
		}
	})
}

type rearmAPI struct {
	DeviceAPI
	err error
}

func (r rearmAPI) MarkUploadPending(context.Context, model.Device) error { return r.err }

type okDevices struct{ store.DeviceStore }

func (okDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	return model.Device{ID: id}, nil
}

// Why: a chooser error (facts could not be gathered) must be a refusal with a reason, never an offer.
func TestPathChooserErrorRefusesTheUpdate(t *testing.T) {
	fp := &fakePaths{err: errors.New("facts unreadable")}
	h, _ := newPathHarness(t, fp)
	_, err := h.e.Submit(context.Background(), "alice", []Request{{DeviceID: 1, Release: "16.0.0", SkipBackup: true, Delivery: model.DeliveryPush, AllowMQTTUpload: true}})
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Refused[0].Reason, "facts unreadable") {
		t.Fatalf("err = %v", err)
	}
}

type probeAPI struct {
	DeviceAPI
	id  model.Identity
	err error
}

func (p probeAPI) ProbeIdentity(context.Context, model.Device) (model.Identity, string, int64, error) {
	return p.id, "", 0, p.err
}

type layoutDevices struct {
	store.DeviceStore
	d   model.Device
	err error
}

func (l layoutDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	return l.d, l.err
}

// Why (review #297, state drift): right before the FileUpload start the device, its identity and the layout
// proof are read again. A read error, a device that changed since the plan, or a proof that is gone
// refuses the upload; only an unchanged device passes.
func TestMQTTPushRechecksTheDeviceRightBeforeTheUpload(t *testing.T) {
	read := time.Unix(1, 0)
	planned := goodIdentity()
	job := model.Job{DeviceID: 5, IdentityBefore: planned, Plan: model.PlanDirect, MQTTUpload: true}
	proven := model.Device{ID: 5, UpLayout: "direct", UpLayoutAt: read}
	mk := func(devs layoutDevices, api probeAPI) *Engine {
		return &Engine{mqttUpload: true, devices: devs, api: api}
	}
	if err := mk(layoutDevices{d: proven}, probeAPI{id: planned}).recheckBeforeUpload(context.Background(), job, proven, goodArtifact()); err != nil {
		t.Fatalf("an unchanged device must pass: %v", err)
	}
	drift := func(f func(*model.Identity)) model.Identity { i := planned; f(&i); return i }
	for name, tc := range map[string]struct {
		devs layoutDevices
		api  probeAPI
	}{
		"device cannot be read":        {layoutDevices{err: errors.New("store down")}, probeAPI{id: planned}},
		"identity cannot be read":      {layoutDevices{d: proven}, probeAPI{err: errors.New("no answer")}},
		"chip changed":                 {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.Family = model.ChipESP32S3 })}},
		"flash changed":                {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.FlashSizeKB = 8192 })}},
		"free flash changed":           {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.FreeKB-- })}},
		"firmware changed":             {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.Version = "16.1.0" })}},
		"variant changed":              {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.Variant = "safeboot" })}},
		"proof dropped meanwhile":      {layoutDevices{d: model.Device{ID: 5}}, probeAPI{id: planned}},
		"proof without a read time":    {layoutDevices{d: model.Device{ID: 5, UpLayout: "direct"}}, probeAPI{id: planned}},
		"layout became safeboot":       {layoutDevices{d: model.Device{ID: 5, UpLayout: "safeboot", UpLayoutAt: read}}, probeAPI{id: planned}},
		"topic changed meanwhile":      {layoutDevices{d: model.Device{ID: 5, Topic: "other", UpLayout: "direct", UpLayoutAt: read}}, probeAPI{id: planned}},
		"full topic changed":           {layoutDevices{d: model.Device{ID: 5, FullTopic: "x/%topic%/%prefix%/", UpLayout: "direct", UpLayoutAt: read}}, probeAPI{id: planned}},
		"fresh free flash became zero": {layoutDevices{d: proven}, probeAPI{id: drift(func(i *model.Identity) { i.FreeKB = 0 })}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := mk(tc.devs, tc.api).recheckBeforeUpload(context.Background(), job, proven, goodArtifact()); err == nil {
				t.Fatal("the upload was not refused")
			}
		})
	}
}

type layoutRec struct {
	store.DeviceStore
	set []string
}

func (l *layoutRec) SetUpLayout(_ context.Context, _ model.DeviceID, layout string) error {
	l.set = append(l.set, layout)
	return nil
}

type upPageAPI struct {
	DeviceAPI
	page tasmota.UpPage
	err  error
	// noPage hides UpPage (an API that cannot read the footer): the type below has it, this flag only selects
	// the plain UpLayout answer for the fallback case
}

func (u upPageAPI) UpLayout(context.Context, model.Device) (tasmota.UpLayout, error) {
	return u.page.Layout, u.err
}
func (u upPageAPI) UpPage(context.Context, model.Device) (tasmota.UpPage, error) {
	return u.page, u.err
}
func (u upPageAPI) SwitchBoot(context.Context, model.Device, tasmota.BootTarget) (tasmota.BootReply, error) {
	return "", errors.New("not used")
}

// layoutOnlyAPI has no UpPage: the routing layout works, no proof is recorded.
type layoutOnlyAPI struct {
	DeviceAPI
	layout tasmota.UpLayout
}

func (u layoutOnlyAPI) UpLayout(context.Context, model.Device) (tasmota.UpLayout, error) {
	return u.layout, nil
}
func (u layoutOnlyAPI) SwitchBoot(context.Context, model.Device, tasmota.BootTarget) (tasmota.BootReply, error) {
	return "", errors.New("not used")
}

// Why (review #297): a read of /up records "direct" only when the page proves the NORMAL firmware runs on
// two app partitions (a single-OTA ESP32 running safeboot serves the plain form too), "safeboot" for the
// safeboot form, and drops an older proof for a page that is ambiguous or proves too little. An unreadable
// page changes nothing.
func TestSafebootRouteRecordsOrDropsTheLayoutProof(t *testing.T) {
	id := model.Identity{Family: model.ChipESP32}
	dual := tasmota.UpPage{Layout: tasmota.UpLayoutDirect, Image: "release-tasmota32"}
	for name, tc := range map[string]struct {
		api  SafebootAPI
		want []string
	}{
		"dual page":                        {upPageAPI{page: dual}, []string{"direct"}},
		"safeboot form":                    {upPageAPI{page: tasmota.UpPage{Layout: tasmota.UpLayoutSafeboot, Image: "release-tasmota32"}}, []string{"safeboot"}},
		"plain form served by safeboot":    {upPageAPI{page: tasmota.UpPage{Layout: tasmota.UpLayoutDirect, Image: "release-safeboot", SafebootBanner: true}}, []string{""}},
		"plain form, banner only":          {upPageAPI{page: tasmota.UpPage{Layout: tasmota.UpLayoutDirect, Image: "release-tasmota32", SafebootBanner: true}}, []string{""}},
		"plain form without a footer":      {upPageAPI{page: tasmota.UpPage{Layout: tasmota.UpLayoutDirect}}, []string{""}},
		"plain form of a minimal firmware": {upPageAPI{page: tasmota.UpPage{Layout: tasmota.UpLayoutDirect, Image: "release-minimal"}}, []string{""}},
		"ambiguous page":                   {upPageAPI{err: tasmota.ErrUpLayoutAmbiguous}, []string{""}},
		"unreadable page":                  {upPageAPI{err: errors.New("timeout")}, nil},
		"no form at all":                   {upPageAPI{err: tasmota.ErrUploadUnsupported}, nil},
		"api without the page reader":      {layoutOnlyAPI{layout: tasmota.UpLayoutDirect}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &layoutRec{}
			stored := "direct" // an older proof; the dual page replaces it with the same value, so start from the other
			if name == "dual page" {
				stored = "safeboot"
			}
			d := model.Device{ID: 4, UpLayout: stored, UpLayoutAt: time.Unix(1, 0)}
			e := &Engine{api: tc.api.(DeviceAPI), devices: rec, safebootPush: true, log: slog.New(slog.DiscardHandler)}
			e.safebootRoute(context.Background(), d, id)
			if strings.Join(rec.set, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("recorded %q, want %q", rec.set, tc.want)
			}
		})
	}
}

// Why (L1): a job that was waiting for the device to restart when tasmofleet restarted needs the block too;
// the machine treats upgrade_issued and await_reboot alike on resume.
func TestRearmCoversEveryStateTheMachineResumesAlike(t *testing.T) {
	for _, st := range []model.JobState{model.JobUpgradeIssued, model.JobAwaitReboot} {
		called := 0
		e := &Engine{api: &countingRearm{n: &called}, devices: okDevices{}, log: slog.New(slog.DiscardHandler),
			jobs: failingJobs{err: errors.New("unused")}}
		j := model.Job{ID: 1, DeviceID: 3, State: st, Delivery: model.DeliveryPush, Transport: model.TransportMQTT, MQTTUpload: true}
		e.rearmResumed(context.Background(), []model.Job{j})
		if called != 1 {
			t.Errorf("%s: re-armed %d times", st, called)
		}
	}
	called := 0
	e := &Engine{api: &countingRearm{n: &called}, devices: okDevices{}, log: slog.New(slog.DiscardHandler)}
	e.rearmResumed(context.Background(), []model.Job{{DeviceID: 3, State: model.JobVerify, Delivery: model.DeliveryPush, Transport: model.TransportMQTT}})
	if called != 0 {
		t.Error("a job in verify was re-armed")
	}
}

type countingRearm struct {
	DeviceAPI
	n *int
}

func (c *countingRearm) MarkUploadPending(context.Context, model.Device) error { *c.n++; return nil }
