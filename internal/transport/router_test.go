package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// fakeHTTP answers every call with err/reply and counts them.
type fakeHTTP struct {
	mu    sync.Mutex
	calls []string
	err   error
	reply string
}

func (h *fakeHTTP) Command(_ context.Context, _ tasmota.Target, c string) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, c)
	if h.err != nil {
		return nil, h.err
	}
	return json.RawMessage(h.reply), nil
}
func (h *fakeHTTP) DownloadConfig(context.Context, tasmota.Target) ([]byte, error) {
	return nil, h.err
}
func (h *fakeHTTP) RootPage(context.Context, tasmota.Target) (tasmota.RootInfo, error) {
	return tasmota.RootInfo{}, h.err
}
func (h *fakeHTTP) n() int { h.mu.Lock(); defer h.mu.Unlock(); return len(h.calls) }

// fakeMQTT records what the channel is asked to send.
type fakeMQTT struct {
	mu    sync.Mutex
	sent  []string
	loose []string
	err   error
	reply string
	down  bool
}

func (m *fakeMQTT) do(c string, loose bool) (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, c)
	if loose {
		m.loose = append(m.loose, c)
	}
	if m.err != nil {
		return nil, m.err
	}
	return json.RawMessage(m.reply), nil
}
func (m *fakeMQTT) Do(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	return m.do(c, false)
}
func (m *fakeMQTT) DoLoose(_ context.Context, _ model.Device, c string) (json.RawMessage, error) {
	return m.do(c, true)
}
func (m *fakeMQTT) Connected() bool { return !m.down }
func (m *fakeMQTT) n() int          { m.mu.Lock(); defer m.mu.Unlock(); return len(m.sent) }

type fakeDevices struct {
	mu sync.Mutex
	d  model.Device
}

func (f *fakeDevices) GetDevice(context.Context, model.DeviceID) (model.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.d, nil
}

// facts builds the Facts of a healthy MQTT device from the snapshot; HTTP state comes from the memory.
type facts struct {
	mem         *Memory
	groupKnown  bool
	othersUnrd  int
	snapshotted int
	healthNewer bool
}

func (f *facts) HTTPState(id model.DeviceID) HTTPState {
	if f.healthNewer {
		return HTTPState{} // a health poll succeeded after the failure: the cheap check says "try HTTP"
	}
	return f.memState(id)
}

func (f *facts) memState(id model.DeviceID) HTTPState {
	if kind, since, ok := f.mem.HTTPFailure(id); ok {
		return HTTPState{State: HTTPFailing, Kind: kind, Since: since}
	}
	return HTTPState{}
}

func (f *facts) FactsFor(_ context.Context, d model.Device) (Facts, error) {
	f.snapshotted++
	now := time.Now()
	m := MQTTFacts{BusConnected: true, Ours: d.MQTT == model.MQTTOurs, Topic: d.Topic, TopicExclusive: true,
		OurFullTopic: "", LWTOnline: true, OthersGroupUnread: f.othersUnrd}
	if f.groupKnown {
		m.GroupTopic, m.GroupTopicReadAt, m.LastProbeAt = "tasmotas", now, now.Add(-time.Hour)
	}
	return Facts{Now: now, HTTP: f.memState(d.ID), MQTT: m}, nil
}

type rig struct {
	r    *Router
	http *fakeHTTP
	mq   *fakeMQTT
	devs *fakeDevices
	mem  *Memory
	f    *facts
}

func newRig(t *testing.T) *rig {
	t.Helper()
	mem := NewMemory(DefaultFailFor, time.Now)
	g := &rig{http: &fakeHTTP{reply: `{"HTTP":"yes"}`}, mq: &fakeMQTT{reply: `{"MQTT":"yes"}`}, mem: mem,
		devs: &fakeDevices{d: model.Device{ID: 7, Address: "192.0.2.7", Topic: "plug", MQTT: model.MQTTOurs}},
		f:    &facts{mem: mem, groupKnown: true}}
	g.r = NewRouter(Options{HTTP: g.http, Devices: g.devs, Facts: g.f, Memory: mem})
	g.r.SetMQTT(g.mq)
	return g
}

func (g *rig) target() tasmota.Target { return tasmota.TargetFor(g.devs.d) }

func TestFailsOverToMQTTOnlyWhenNothingWasSent(t *testing.T) {
	// Why (D4): the HTTP request may have been executed. Only proofs of "never sent" allow the same
	// command to be started again on the other transport; anything else would double-execute it.
	for _, tc := range []struct {
		name     string
		err      error
		failover bool
	}{
		{"address outside the device networks", tasmota.ErrForbiddenAddr, true},
		{"401 before execution", tasmota.ErrAuth, true},
		{"SetOption128: the handler runs nothing", tasmota.ErrHTTPAPIRefused, true},
		{"timeout: the command may have run", tasmota.ErrTimeout, false},
		{"unparsable answer: it ran", tasmota.ErrBadResponse, false},
		{"unknown outcome unreachable (dropped after the write)", tasmota.ErrUnreachable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			g.http.err = tc.err
			raw, err := g.r.Command(t.Context(), g.target(), "Status 2")
			if tc.failover {
				if err != nil || string(raw) != `{"MQTT":"yes"}` || g.mq.n() != 1 || g.http.n() != 1 {
					t.Fatalf("raw=%s err=%v http=%d mqtt=%d", raw, err, g.http.n(), g.mq.n())
				}
				return
			}
			if !errors.Is(err, tc.err) || g.mq.n() != 0 {
				t.Fatalf("err=%v mqtt=%d: a call whose outcome is unknown must not be sent again", err, g.mq.n())
			}
		})
	}
}

func TestRefusedConnectionIsOneMQTTSend(t *testing.T) {
	// Why (D4, #45): a closed listener is the proof that nothing left tasmofleet; the command is
	// then sent exactly once, over MQTT, and the next ones go there up front.
	g := newRig(t)
	g.devs.d.Address, g.devs.d.Port = "127.0.0.1", 1 // nothing listens
	cl := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopback()})
	g.r = NewRouter(Options{HTTP: cl, Devices: g.devs, Facts: g.f, Memory: g.mem})
	g.r.SetMQTT(g.mq)
	ctx, tr := WithTrace(t.Context())
	if _, err := g.r.Command(ctx, g.target(), "Power1 ON"); err != nil {
		t.Fatal(err)
	}
	if g.mq.n() != 1 || tr.Via() != "mqtt" {
		t.Fatalf("mqtt sends = %d, via = %s", g.mq.n(), tr.Via())
	}
	if _, _, ok := g.mem.HTTPFailure(7); !ok {
		t.Error("the refusal was not remembered, the next call would try HTTP again")
	}
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 OFF"); err != nil || g.mq.n() != 2 {
		t.Fatalf("second command: err=%v mqtt=%d", err, g.mq.n())
	}
}

func TestTargetWithoutDeviceNeverUsesMQTT(t *testing.T) {
	// Why (D1): scans, MAC verifiers and address-change probes verify an address over HTTP; sending
	// them to a topic would answer for another device.
	g := newRig(t)
	g.http.err = tasmota.ErrForbiddenAddr
	g.mem.Fail(7, HTTPFailUnreachable)
	tg := tasmota.TargetFor(model.Device{Address: "192.0.2.7"})
	if _, err := g.r.Command(t.Context(), tg, "Status 5"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 0 {
		t.Fatalf("err=%v mqtt=%d", err, g.mq.n())
	}
	if _, _, ok := g.mem.HTTPFailure(0); ok {
		t.Error("an address check was remembered as the failure of an inventory device")
	}
}

func TestReadsAndWritesFollowTheirOwnPath(t *testing.T) {
	// Why (review #279): an unread GroupTopic lets a read go to the topic but not a write; a write
	// to a topic that may be shared would change several devices.
	g := newRig(t)
	g.f.groupKnown = false
	g.mem.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.mq.n() != 1 {
		t.Fatalf("read: err=%v mqtt=%d", err, g.mq.n())
	}
	g.http.err = tasmota.ErrUnreachable
	_, err := g.r.Command(t.Context(), g.target(), "Power1 ON")
	if err == nil || g.mq.n() != 1 {
		t.Fatalf("write: err=%v mqtt=%d, want a refusal without a send", err, g.mq.n())
	}
	if !strings.Contains(err.Error(), "GroupTopic") {
		t.Errorf("the error does not say why MQTT was not used: %v", err)
	}
	g.f.groupKnown = true
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 ON"); err != nil || g.mq.n() != 2 {
		t.Fatalf("write with a known group topic: err=%v mqtt=%d", err, g.mq.n())
	}
}

func TestPinnedPathsNeverFailOver(t *testing.T) {
	// Why (D5): a job that chose its path must not be rerouted half way.
	g := newRig(t)
	g.http.err = tasmota.ErrForbiddenAddr
	if _, err := g.r.Command(WithPath(t.Context(), HTTP), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 0 {
		t.Fatalf("pinned HTTP: err=%v mqtt=%d", err, g.mq.n())
	}
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout)
	before := g.http.n()
	if _, err := g.r.Command(WithPath(t.Context(), MQTT), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrTimeout) || g.http.n() != before {
		t.Fatalf("pinned MQTT: err=%v http calls %d -> %d", err, before, g.http.n())
	}
	// pinned MQTT works although HTTP is not failing, but a write still needs the strict conditions
	g.mq.err = nil
	g.f.groupKnown = false
	n := g.mq.n()
	if _, err := g.r.Command(WithPath(t.Context(), MQTT), g.target(), "Power1 ON"); err == nil || g.mq.n() != n {
		t.Fatalf("pinned MQTT write with an unread group topic: err=%v", err)
	}
}

func TestAFailedMQTTSendIsNeverRepeatedOverHTTP(t *testing.T) {
	// Why (D4): never MQTT -> HTTP after a publish.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.mq.err = fmt.Errorf("mqtt: no answer: %w", tasmota.ErrTimeout)
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 TOGGLE"); !errors.Is(err, tasmota.ErrTimeout) || g.http.n() != 0 {
		t.Fatalf("err=%v http calls=%d", err, g.http.n())
	}
}

func TestUpgradeAndRestoreCommandsOnlyGoOverMQTTPinned(t *testing.T) {
	// Why: an upgrade or a configuration restore is run by a job with its own safety checks
	// (Guard, probe-first). The generic router must not start one on a transport nobody chose.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.http.err = tasmota.ErrForbiddenAddr
	for _, c := range []string{"Upgrade 1", "WebGetConfig http://x/y", "Backlog OtaUrl http://x/y; Upgrade 1"} {
		if _, err := g.r.Command(t.Context(), g.target(), c); err == nil || g.mq.n() != 0 {
			t.Errorf("%q: err=%v mqtt=%d", c, err, g.mq.n())
		}
	}
	n := g.mq.n()
	if _, err := g.r.Command(WithPath(t.Context(), MQTT), g.target(), "Upgrade 1"); err != nil || g.mq.n() != n+1 {
		t.Fatalf("pinned: err=%v mqtt=%d", err, g.mq.n())
	}
}

func TestSecretReplyIsScrubbedOverMQTT(t *testing.T) {
	// Why (#274): WebPassword and MqttPassword echo the new password in their RESULT; over MQTT it
	// travels on stat/<t>/RESULT and must be masked before anything stores or shows it.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.mq.reply = `{"WebPassword":"hunter2"}`
	raw, err := g.r.Command(t.Context(), g.target(), "WebPassword hunter2")
	if err != nil || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
}

func TestTopicChangeBetweenSnapshotAndSendSendsNothing(t *testing.T) {
	// Why: the facts and the send look at one snapshot; an edit that lands in between must not
	// publish to the old topic.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	g.f.mem = g.mem
	g.r.o.Facts = changingFacts{inner: g.f, devs: g.devs}
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err == nil || g.mq.n() != 0 {
		t.Fatalf("err=%v mqtt=%d", err, g.mq.n())
	}
}

type changingFacts struct {
	inner *facts
	devs  *fakeDevices
}

func (c changingFacts) HTTPState(id model.DeviceID) HTTPState { return c.inner.HTTPState(id) }
func (c changingFacts) FactsFor(ctx context.Context, d model.Device) (Facts, error) {
	f, err := c.inner.FactsFor(ctx, d)
	c.devs.mu.Lock()
	c.devs.d.Topic = "renamed"
	c.devs.mu.Unlock()
	return f, err
}

func TestConsoleUsesTheLooseMatch(t *testing.T) {
	g := newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable)
	if _, err := g.r.Command(WithConsole(t.Context()), g.target(), "Template"); err != nil || len(g.mq.loose) != 1 {
		t.Fatalf("err=%v loose=%v", err, g.mq.loose)
	}
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || len(g.mq.loose) != 1 {
		t.Fatalf("an automated read must match strictly: %v", g.mq.loose)
	}
}

func TestMQTTNotConnectedKeepsHTTP(t *testing.T) {
	// Why: before the broker is up (and in the CLI) the router is the HTTP client it wraps.
	g := newRig(t)
	g.mq.down = true
	g.http.err = tasmota.ErrForbiddenAddr
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrForbiddenAddr) || g.mq.n() != 0 {
		t.Fatalf("err=%v", err)
	}
	g.r.SetMQTT(nil)
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); !errors.Is(err, tasmota.ErrForbiddenAddr) {
		t.Fatalf("err=%v", err)
	}
}

func TestHTTPFailureMemory(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	m := NewMemory(10*time.Minute, func() time.Time { return now })
	m.Fail(1, HTTPFailUnreachable)
	now = now.Add(5 * time.Minute)
	m.Fail(1, HTTPFailAuth) // a second failure extends the series but keeps its start
	kind, since, ok := m.HTTPFailure(1)
	if !ok || kind != HTTPFailAuth || !since.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("%v %v %v", kind, since, ok)
	}
	now = now.Add(9 * time.Minute)
	if _, _, ok := m.HTTPFailure(1); !ok {
		t.Error("expired too early")
	}
	now = now.Add(2 * time.Minute)
	if _, _, ok := m.HTTPFailure(1); ok {
		t.Error("a failure lasts 10 minutes after the last one")
	}
	m.Fail(2, HTTPFailUnreachable)
	m.Clear(2)
	if _, _, ok := m.HTTPFailure(2); ok {
		t.Error("an answered call clears the failure")
	}
}

func TestObserverRemembersWhatHTTPShows(t *testing.T) {
	// Why: the health poll shares the memory, so a device whose port stopped answering is routed
	// to MQTT before anyone sends it a command, and a recovered one is not kept on MQTT.
	g := newRig(t)
	h := g.r.HTTPOnly()
	for _, tc := range []struct {
		err  error
		want HTTPFailKind
	}{{tasmota.ErrUnreachable, HTTPFailUnreachable}, {tasmota.ErrForbiddenAddr, HTTPFailOutside}, {tasmota.ErrHTTPAPIRefused, HTTPFailAPIRefused}, {tasmota.ErrAuth, HTTPFailAuth}} {
		g.mem.Clear(7)
		g.http.err = tc.err
		_, _ = h.Command(t.Context(), g.target(), "Status 11")
		if k, _, ok := g.mem.HTTPFailure(7); !ok || k != tc.want {
			t.Errorf("%v: %v %v", tc.err, k, ok)
		}
	}
	g.http.err = tasmota.ErrTimeout
	_, _ = h.Command(t.Context(), g.target(), "Status 11")
	if _, _, ok := g.mem.HTTPFailure(7); !ok {
		t.Error("a timeout says nothing; it must not clear a failure")
	}
	g.http.err = nil
	_, _ = h.Command(t.Context(), g.target(), "Status 11")
	if _, _, ok := g.mem.HTTPFailure(7); ok {
		t.Error("a successful call must clear the failure")
	}
}

func TestUploadAndBootSwitchPassThroughToHTTPOnly(t *testing.T) {
	// Why (D1, D4): uploads and the safeboot switch are HTTP only and never fail over.
	g := newRig(t)
	if err := g.r.UploadFirmware(t.Context(), g.target(), "x.bin", 1, strings.NewReader("x")); !errors.Is(err, tasmota.ErrUploadUnsupported) {
		t.Fatalf("a client that cannot upload: %v", err)
	}
	if g.mq.n() != 0 {
		t.Fatal("an upload reached MQTT")
	}
}

func TestMQTTBlockerMatchesChoose(t *testing.T) {
	// Why: the router's pinned calls use the same conditions Choose uses for its decision.
	f := Facts{MQTT: MQTTFacts{BusConnected: true, Ours: true, Topic: "plug", TopicExclusive: true, LWTOnline: true,
		GroupTopic: "g", GroupTopicReadAt: time.Now(), LastProbeAt: time.Now().Add(-time.Hour)}}
	if why := MQTTBlocker(f, true); why != "" {
		t.Fatal(why)
	}
	f.MQTT.GroupTopicReadAt = time.Time{}
	if MQTTBlocker(f, true) == "" || MQTTBlocker(f, false) != "" {
		t.Error("an unread group topic blocks writes only")
	}
}

func TestACutOffWriteNeedsAConfirmationOverMQTT(t *testing.T) {
	// Why (D13): a new topic, host or login published to a device that only MQTT reaches can cut it off
	// for good. The console's confirmation carries the consent; nothing else does, and a refusal
	// publishes nothing, over the routed path and after a failover alike.
	for _, name := range []string{"routed", "failover"} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			if name == "routed" {
				g.mem.Fail(7, HTTPFailUnreachable)
			} else {
				g.http.err = tasmota.ErrForbiddenAddr
			}
			for _, c := range []string{"Topic other", "Backlog Power ON; MqttHost 10.0.0.9", "SetOption3 0", "Prefix1 x"} {
				ctx, tr := WithTrace(t.Context())
				_, err := g.r.Command(ctx, g.target(), c)
				if !errors.Is(err, ErrCutOff) || g.mq.n() != 0 || tr.Via() != "none" {
					t.Fatalf("%q: err=%v mqtt=%d via=%s, want a refusal that published nothing", c, err, g.mq.n(), tr.Via())
				}
				if !strings.Contains(err.Error(), "not sent: this device is reached over MQTT only") || strings.Contains(err.Error(), "other") {
					t.Errorf("%q: text = %v", c, err)
				}
			}
			if _, err := g.r.Command(WithCutOffConsent(t.Context()), g.target(), "Topic other"); err != nil || g.mq.n() != 1 {
				t.Fatalf("with the consent: err=%v mqtt=%d", err, g.mq.n())
			}
			// a plain write and a query need no consent
			if _, err := g.r.Command(t.Context(), g.target(), "Topic"); err != nil {
				t.Fatalf("query: %v", err)
			}
		})
	}
}

func TestOverHTTPACutOffWriteIsSentAsAlways(t *testing.T) {
	// Why: the consent only concerns the MQTT path; a device HTTP reaches takes the change.
	g := newRig(t)
	if _, err := g.r.Command(t.Context(), g.target(), "Topic other"); err != nil || g.http.n() != 1 || g.mq.n() != 0 {
		t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
	}
}

func TestAPIRefusedFailsOverOnlyForReadsOrAKnownRefusal(t *testing.T) {
	// Why (D4 amended): a device with SetOption128 0 sends nothing for a /cm request with the wrong
	// Referer, but the same silence can follow a command that ran. A write that gets that answer on its
	// first try is therefore not repeated over MQTT; reads are, and so is any call to a device the memory
	// already knows to refuse the API.
	g := newRig(t)
	g.http.err = tasmota.ErrHTTPAPIRefused
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 TOGGLE"); !errors.Is(err, tasmota.ErrHTTPAPIRefused) || g.mq.n() != 0 {
		t.Fatalf("first-try write: err=%v mqtt=%d", err, g.mq.n())
	}
	g = newRig(t)
	g.http.err = tasmota.ErrHTTPAPIRefused
	if _, err := g.r.Command(t.Context(), g.target(), "Status 2"); err != nil || g.mq.n() != 1 {
		t.Fatalf("read: err=%v mqtt=%d", err, g.mq.n())
	}
	g = newRig(t)
	g.mem.Fail(7, HTTPFailAPIRefused)
	g.http.err = tasmota.ErrHTTPAPIRefused
	// memory says failing, so Choose already routes this write to MQTT up front
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 TOGGLE"); err != nil || g.mq.n() != 1 || g.http.n() != 0 {
		t.Fatalf("known refusal: err=%v mqtt=%d http=%d", err, g.mq.n(), g.http.n())
	}
}

func TestAPIRefusedWriteFailsOverWhenTheMemoryAlreadyKnewTheRefusal(t *testing.T) {
	// Why (D4 amended): the cheap HTTP check (a health poll newer than the failure) says "try HTTP", while
	// the memory still holds API-refused from before this call. The answer to the write is then the same
	// refusal the memory already knew, so the write goes over MQTT once. Without that history it does not.
	g := newRig(t)
	g.mem.Fail(7, HTTPFailAPIRefused)
	g.f.healthNewer = true
	g.http.err = tasmota.ErrHTTPAPIRefused
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 TOGGLE"); err != nil || g.http.n() != 1 || g.mq.n() != 1 {
		t.Fatalf("err=%v http=%d mqtt=%d", err, g.http.n(), g.mq.n())
	}
	g = newRig(t)
	g.mem.Fail(7, HTTPFailUnreachable) // failing, but not because of the API
	g.f.healthNewer = true
	g.http.err = tasmota.ErrHTTPAPIRefused
	if _, err := g.r.Command(t.Context(), g.target(), "Power1 TOGGLE"); !errors.Is(err, tasmota.ErrHTTPAPIRefused) || g.mq.n() != 0 {
		t.Fatalf("a write whose first refusal is this call: err=%v mqtt=%d", err, g.mq.n())
	}
}

func TestTraceSaysNoneWhenNothingWasSent(t *testing.T) {
	g := newRig(t)
	g.http.err = tasmota.ErrForbiddenAddr
	g.mq.down = true // no MQTT either
	ctx, tr := WithTrace(t.Context())
	_, _ = g.r.Command(ctx, g.target(), "Power1 ON")
	if tr.Via() != "none" {
		t.Fatalf("via = %s", tr.Via())
	}
	if (*Trace)(nil).Via() != "http" {
		t.Error("no trace reads as http")
	}
}
