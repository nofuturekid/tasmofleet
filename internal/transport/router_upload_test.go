package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
)

// uploadMQTT is the command channel with an Upload method, recording what reached it.
type uploadMQTT struct {
	*fakeMQTT
	calls    int
	typ      int
	password string
	err      error
	marked   []string
}

func (u *uploadMQTT) MarkFirmwarePending(topic string) { u.marked = append(u.marked, topic) }

func (u *uploadMQTT) Upload(_ context.Context, _ model.Device, typ int, _ string, _ int64, _ io.Reader, password string, _ func(int64)) error {
	u.calls++
	u.typ, u.password = typ, password
	return u.err
}

func uploadRig(t *testing.T, pw Password) (*rig, *uploadMQTT) {
	t.Helper()
	g := newRig(t)
	g.devs.d.MQTTPasswordMode, g.devs.d.MQTTPasswordBadAt, g.devs.d.MQTTPassword = pw.Mode, pw.BadAt, "device-pw"
	u := &uploadMQTT{fakeMQTT: g.mq}
	g.r.SetMQTT(u)
	return g, u
}

func pinnedMQTT(ctx context.Context) context.Context { return WithPath(ctx, MQTT) }

// Why: the image goes through the broker only for a job that chose MQTT, with the device's own
// password, as a firmware upload (Type 1) and nothing else.
func TestUploadFirmwareMQTTSendsType1WithTheDevicesPassword(t *testing.T) {
	g, u := uploadRig(t, Password{Mode: model.MQTTPasswordOwn})
	err := g.r.UploadFirmwareMQTT(pinnedMQTT(t.Context()), g.target(), "tasmota32.bin", 10, strings.NewReader("0123456789"), nil)
	if err != nil || u.calls != 1 || u.typ != mqttcmd.UploadFirmware || u.password != "device-pw" {
		t.Fatalf("err %v, calls %d, type %d, password %q", err, u.calls, u.typ, u.password)
	}
}

// Why: a device in broker mode sends TasmoFleet's live MQTT_PASSWORD.
func TestUploadFirmwareMQTTBrokerModeUsesTheLiveBrokerPassword(t *testing.T) {
	g, u := uploadRig(t, Password{Mode: model.MQTTPasswordBroker})
	g.devs.d.MQTTPasswordMode = model.MQTTPasswordBroker
	live := "one"
	g.r.o.BrokerPassword = func() string { return live }
	live = "two"
	if err := g.r.UploadFirmwareMQTT(pinnedMQTT(t.Context()), g.target(), "x.bin", 1, strings.NewReader("x"), nil); err != nil || u.password != "two" {
		t.Fatalf("err %v, password %q", err, u.password)
	}
}

// Why: nothing is published when the path was not chosen, the password is missing or the device is
// not usable over MQTT; a wrong password is remembered.
func TestUploadFirmwareMQTTRefusalsPublishNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		pw   Password
		ctx  func(context.Context) context.Context
		mut  func(*rig)
		want string
	}{
		"not pinned":         {Password{Mode: model.MQTTPasswordOwn}, func(c context.Context) context.Context { return c }, nil, "chose MQTT"},
		"pinned to HTTP":     {Password{Mode: model.MQTTPasswordOwn}, func(c context.Context) context.Context { return WithPath(c, HTTP) }, nil, "chose MQTT"},
		"no password":        {Password{}, pinnedMQTT, nil, "MQTT password"},
		"password refused":   {Password{Mode: model.MQTTPasswordOwn, BadAt: time.Now()}, pinnedMQTT, nil, "refused its MQTT password"},
		"own without secret": {Password{Mode: model.MQTTPasswordOwn}, pinnedMQTT, func(g *rig) { g.devs.d.MQTTPassword = "" }, "usable MQTT password"},
		"group unknown":      {Password{Mode: model.MQTTPasswordOwn}, pinnedMQTT, func(g *rig) { g.f.groupKnown = false }, ""},
	} {
		t.Run(name, func(t *testing.T) {
			g, u := uploadRig(t, tc.pw)
			if tc.mut != nil {
				tc.mut(g)
			}
			err := g.r.UploadFirmwareMQTT(tc.ctx(t.Context()), g.target(), "x.bin", 1, strings.NewReader("x"), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) || u.calls != 0 {
				t.Fatalf("err %v, calls %d, want a refusal containing %q", err, u.calls, tc.want)
			}
		})
	}
}

func TestUploadFirmwareMQTTMarksARefusedPassword(t *testing.T) {
	g, u := uploadRig(t, Password{Mode: model.MQTTPasswordOwn})
	u.err = mqttcmd.ErrPasswordRefused
	m := &markRec{}
	g.r.o.Marker = m
	marked := &m.id
	if err := g.r.UploadFirmwareMQTT(pinnedMQTT(t.Context()), g.target(), "x.bin", 1, strings.NewReader("x"), nil); !errors.Is(err, mqttcmd.ErrPasswordRefused) || *marked != 7 {
		t.Fatalf("err %v, marked %d", err, *marked)
	}
}

// Why: after a restart of tasmofleet the engine re-arms the block of the device's topic.
func TestMarkUploadPendingMarksTheDevicesTopic(t *testing.T) {
	g, u := uploadRig(t, Password{Mode: model.MQTTPasswordOwn})
	if err := g.r.MarkUploadPending(t.Context(), g.target()); err != nil || len(u.marked) != 1 || u.marked[0] != "plug" {
		t.Fatalf("err %v, marked %v", err, u.marked)
	}
}

type markRec struct{ id model.DeviceID }

func (m *markRec) MarkMQTTPasswordBad(_ context.Context, id model.DeviceID, _ time.Time) error {
	m.id = id
	return nil
}

// Why: at the start of tasmofleet the broker connection may still be coming up; the block on the topic
// must be settable then, or the re-arm would fail open.
func TestMarkUploadPendingWorksBeforeTheBrokerIsConnected(t *testing.T) {
	g, u := uploadRig(t, Password{Mode: model.MQTTPasswordOwn})
	g.mq.down = true
	if err := g.r.MarkUploadPending(t.Context(), g.target()); err != nil || len(u.marked) != 1 {
		t.Fatalf("err %v, marked %v", err, u.marked)
	}
	// no channel attached at all: an error the caller turns into a refusal
	g.r.SetMQTT(nil)
	if err := g.r.MarkUploadPending(t.Context(), g.target()); err == nil {
		t.Fatal("without a channel the block cannot be set and must say so")
	}
}
