package devicecmd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type recorder struct {
	mu sync.Mutex
	ev []model.AuditEvent
}

func (r *recorder) Record(_ context.Context, e model.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, e)
}

func (r *recorder) all() []model.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ev)
}

type devices map[model.DeviceID]model.Device

func (d devices) Get(_ context.Context, id model.DeviceID) (model.Device, error) {
	if x, ok := d[id]; ok {
		return x, nil
	}
	return model.Device{}, store.ErrNotFound
}

// fakeBus records publishes and answers them like a Tasmota device would.
type fakeBus struct {
	mu        sync.Mutex
	connected bool
	published []string // "topic command payload"
	answer    func(topic, command, payload string) *mqttbus.Message
	handlers  []func(mqttbus.Message)
	retained  []mqttbus.Message
}

func (b *fakeBus) CommandTopic(topic, command string) string { return "cmnd/" + topic + "/" + command }

func (b *fakeBus) Connected() bool { return b.connected }

func (b *fakeBus) Subscribe(_ []mqttbus.Kind, h func(mqttbus.Message)) func() {
	b.mu.Lock()
	b.handlers = append(b.handlers, h)
	old := slices.Clone(b.retained)
	b.mu.Unlock()
	for _, m := range old { // the real bus replays retained messages on subscribe
		h(m)
	}
	return func() {}
}

func (b *fakeBus) Command(_ context.Context, topic, command, payload string) error {
	b.mu.Lock()
	b.published = append(b.published, topic+" "+command+" "+payload)
	hs := slices.Clone(b.handlers)
	b.mu.Unlock()
	if b.answer != nil {
		if m := b.answer(topic, command, payload); m != nil {
			for _, h := range hs {
				h(*m)
			}
		}
	}
	return nil
}

func (b *fakeBus) pubs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.published)
}

type env struct {
	svc    Service
	rec    *recorder
	sim    simulator.Device
	dev    model.Device
	bus    *fakeBus
	devs   devices
	client tasmota.Client
}

func setup(t *testing.T, spec simulator.DeviceSpec, mqtt model.MQTTCapability, bus *fakeBus) *env {
	t.Helper()
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	sim, err := fleet.Add(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	host, ps, _ := net.SplitHostPort(sim.Addr())
	port, _ := strconv.Atoi(ps)
	dev := model.Device{ID: 7, Name: spec.Name, Address: host, Port: port, Topic: sim.Topic(), MQTT: mqtt,
		HTTPUser: spec.User, HTTPPassword: spec.Password}
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	rec := &recorder{}
	o := Options{Devices: devices{7: dev}, Client: client, Audit: rec}
	if bus != nil {
		// The real command channel on a scripted bus: what Power sends and accepts is decided there.
		o.MQTT = mqttcmd.New(bus, mqttcmd.Options{Gap: time.Millisecond, ReplyWait: 60 * time.Millisecond})
	}
	return &env{svc: New(o), rec: rec, sim: sim, dev: dev, bus: bus, devs: devices{7: dev}, client: client}
}

var plug = simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M}

func operator() model.Principal {
	return model.Principal{Subject: "oidc:olli", Role: model.RoleOperator}
}
func admin() model.Principal { return model.Principal{Subject: "local:adam", Role: model.RoleAdmin} }

// Why: an HTTP-only device (no MQTT broker involved) must still switch, and
// the answer must be the state the device reports, not what we hoped for.
func TestPowerOverHTTP(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	state, err := e.svc.Power(context.Background(), "oidc:olli", 7, 1, "TOGGLE")
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Power(context.Background(), "oidc:olli", 7, 1, "toggle")
	if err != nil {
		t.Fatal(err)
	}
	if state == again || (state != "ON" && state != "OFF") {
		t.Fatalf("two toggles must flip the reported state, got %q then %q", state, again)
	}
	if got := e.sim.Commands(); !slices.Contains(got, "Power1 TOGGLE") {
		t.Fatalf("simulator saw %v", got)
	}
}

func TestPowerRejectsBadInputBeforeAnyIO(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	for _, tc := range []struct {
		relay  int
		action string
		want   error
	}{
		{1, "ON;Reset 1", ErrBadAction},
		{1, "BLINK", ErrBadAction},
		{1, "", ErrBadAction},
		{0, "ON", ErrNoRelay},
		{9, "ON", ErrNoRelay},
		{-1, "ON", ErrNoRelay},
	} {
		if _, err := e.svc.Power(context.Background(), "x", 7, tc.relay, tc.action); !errors.Is(err, tc.want) {
			t.Errorf("Power(%d,%q) = %v, want %v", tc.relay, tc.action, err, tc.want)
		}
	}
	if got := e.sim.Commands(); len(got) != 0 {
		t.Fatalf("invalid input reached the device: %v", got)
	}
}

// Why: a relay the device does not have must give a clear answer instead of
// a reply of the wrong relay being shown as success.
func TestPowerUnknownRelay(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	_, err := e.svc.Power(context.Background(), "oidc:olli", 7, 3, "ON")
	if !errors.Is(err, ErrNoRelay) {
		t.Fatalf("want ErrNoRelay, got %v", err)
	}
	if !strings.Contains(err.Error(), "relay 3") {
		t.Fatalf("message must name the relay: %v", err)
	}
}

func TestPowerUnknownDevice(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	if _, err := e.svc.Power(context.Background(), "x", 99, 1, "ON"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

type noted struct {
	id    model.DeviceID
	relay int
	state string
	calls int
}

func (n *noted) NotePower(id model.DeviceID, relay int, state string) {
	n.id, n.relay, n.state = id, relay, state
	n.calls++
}

// Why (#206): the HTTP answer of a toggle must reach the health tracker, or a poll that read the device
// just before the toggle puts the old state back in the list. Only a confirmed state is passed on.
func TestPowerOverHTTPTellsTheTrackerTheConfirmedState(t *testing.T) {
	n := &noted{}
	e := setup(t, plug, model.MQTTNone, nil)
	e.svc = New(Options{Devices: e.devs, Client: e.client, Audit: e.rec, Health: n})
	state, err := e.svc.Power(context.Background(), "oidc:olli", 7, 1, "TOGGLE")
	if err != nil {
		t.Fatal(err)
	}
	if n.calls != 1 || n.id != 7 || n.relay != 1 || n.state != state {
		t.Fatalf("tracker told %+v, device said %q", n, state)
	}
	if _, err := e.svc.Power(context.Background(), "oidc:olli", 7, 9, "ON"); err == nil || n.calls != 1 {
		t.Fatalf("a refused command must not be passed on (calls %d, err %v)", n.calls, err)
	}
}

// Why: with MQTT as the working channel the switch goes over the broker and
// the device is not hit over HTTP at all (ESP8266 web servers are fragile).
func TestPowerOverMQTT(t *testing.T) {
	bus := &fakeBus{connected: true}
	bus.answer = func(topic, command, payload string) *mqttbus.Message {
		return &mqttbus.Message{Kind: mqttbus.KindResult, Topic: topic, Suffix: "RESULT", Payload: []byte(`{"POWER":"ON"}`)}
	}
	e := setup(t, plug, model.MQTTOurs, bus)
	state, err := e.svc.Power(context.Background(), "oidc:olli", 7, 1, "ON")
	if err != nil || state != "ON" {
		t.Fatalf("state %q err %v", state, err)
	}
	if got := bus.pubs(); !slices.Equal(got, []string{"plug Power1 ON"}) {
		t.Fatalf("published %v", got)
	}
	if got := e.sim.Commands(); len(got) != 0 {
		t.Fatalf("MQTT path must not use HTTP: %v", got)
	}
}

// Why: a retained stat message is old news, and a button press on the same topic is not the
// answer to our command; taking either would show a wrong state right after a click.
func TestPowerOverMQTTIgnoresRetained(t *testing.T) {
	bus := &fakeBus{connected: true}
	bus.retained = []mqttbus.Message{{Kind: mqttbus.KindResult, Topic: "plug", Suffix: "RESULT", Payload: []byte(`{"POWER":"OFF"}`), Retained: true}}
	bus.answer = func(topic, _, _ string) *mqttbus.Message {
		return &mqttbus.Message{Kind: mqttbus.KindResult, Topic: topic, Suffix: "RESULT", Payload: []byte(`{"POWER":"ON"}`)}
	}
	e := setup(t, plug, model.MQTTOurs, bus)
	state, err := e.svc.Power(context.Background(), "x", 7, 1, "ON")
	if err != nil || state != "ON" {
		t.Fatalf("state %q err %v", state, err)
	}
}

func TestPowerOverMQTTMultiRelayAnswersPerRelay(t *testing.T) {
	bus := &fakeBus{connected: true}
	bus.answer = func(topic, _, _ string) *mqttbus.Message {
		// another relay's report arrives first and must not be mistaken
		return &mqttbus.Message{Kind: mqttbus.KindResult, Topic: topic, Suffix: "RESULT", Payload: []byte(`{"POWER1":"ON"}`)}
	}
	e := setup(t, plug, model.MQTTOurs, bus)
	_, err := e.svc.Power(context.Background(), "x", 7, 2, "ON")
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("a report for relay 1 does not confirm relay 2: %v", err)
	}
}

// Why: after a published command we cannot know whether it was applied. A
// silent HTTP retry could toggle twice, so the answer is "unconfirmed".
func TestPowerOverMQTTUnconfirmedDoesNotRetryOverHTTP(t *testing.T) {
	bus := &fakeBus{connected: true}
	e := setup(t, plug, model.MQTTOurs, bus)
	_, err := e.svc.Power(context.Background(), "x", 7, 1, "TOGGLE")
	if !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("want ErrUnconfirmed, got %v", err)
	}
	if got := e.sim.Commands(); len(got) != 0 {
		t.Fatalf("no HTTP retry after a publish: %v", got)
	}
}

// Why: MQTT is an accelerator. When the broker is down, or the device does
// not talk to our broker, HTTP does the job.
func TestPowerFallsBackToHTTP(t *testing.T) {
	for name, tc := range map[string]struct {
		bus *fakeBus
		cap model.MQTTCapability
	}{
		"broker down":       {&fakeBus{connected: false}, model.MQTTOurs},
		"other broker":      {&fakeBus{connected: true}, model.MQTTOtherBroker},
		"unknown":           {&fakeBus{connected: true}, model.MQTTUnknown},
		"no bus configured": {nil, model.MQTTOurs},
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, plug, tc.cap, tc.bus)
			if _, err := e.svc.Power(context.Background(), "x", 7, 1, "ON"); err != nil {
				t.Fatal(err)
			}
			if tc.bus != nil && len(tc.bus.pubs()) != 0 {
				t.Fatalf("must not publish: %v", tc.bus.pubs())
			}
			if !slices.Contains(e.sim.Commands(), "Power1 ON") {
				t.Fatalf("HTTP not used: %v", e.sim.Commands())
			}
		})
	}
}

func TestPowerIsAudited(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	_, _ = e.svc.Power(context.Background(), "oidc:olli", 7, 1, "ON")
	_, _ = e.svc.Power(context.Background(), "oidc:olli", 7, 5, "ON")
	ev := e.rec.all()
	if len(ev) != 2 {
		t.Fatalf("want 2 audit events, got %+v", ev)
	}
	if ev[0].Action != "device.power" || ev[0].Actor != "oidc:olli" || ev[0].Target != "device:7" || ev[0].Result != "ok" {
		t.Errorf("first event: %+v", ev[0])
	}
	if ev[1].Result == "ok" || ev[1].Result == "" {
		t.Errorf("a failed switch must be audited as failed: %+v", ev[1])
	}
}

// Why: the console is the biggest hole in the app. Anything the policy
// refuses (malformed lines for everybody, everything beyond the operator set
// for operators) must be refused before the device is contacted.
func TestRunRefusesBeforeNetworkIO(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	for _, tc := range []struct {
		p    model.Principal
		cmd  string
		want error
	}{
		{operator(), "SetOption19 1", cmdpolicy.ErrRole},
		{operator(), "Reset 1", cmdpolicy.ErrRole},
		{operator(), "Backlog Power ON", cmdpolicy.ErrRole},
		{operator(), "Status 0", cmdpolicy.ErrRole},
		{operator(), "FooBar 1", cmdpolicy.ErrRole},
		{operator(), "MqttPassword x", cmdpolicy.ErrRole},
		{admin(), "Power ON;Reset 1", cmdpolicy.ErrInvalid},
		{operator(), "Power%3BReset", cmdpolicy.ErrInvalid},
		{model.Principal{Subject: "x"}, "Status 2", cmdpolicy.ErrRole},
	} {
		if _, err := e.svc.Run(context.Background(), tc.p, 7, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("Run(%v, %q) = %v, want %v", tc.p.Role, tc.cmd, err, tc.want)
		}
	}
	if got := e.sim.Commands(); len(got) != 0 {
		t.Fatalf("the device received %v", got)
	}
	for _, ev := range e.rec.all() {
		if ev.Action != "command.rejected" || ev.Result == "ok" {
			t.Errorf("refusals must be audited as such: %+v", ev)
		}
	}
	if len(e.rec.all()) != 9 {
		t.Errorf("every refusal is audited, got %d", len(e.rec.all()))
	}
}

func TestRunSendsAllowedCommand(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	raw, err := e.svc.Run(context.Background(), operator(), 7, "Status 2")
	if err != nil || !strings.Contains(string(raw), "StatusFWR") {
		t.Fatalf("raw %s err %v", raw, err)
	}
	if !slices.Equal(e.sim.Commands(), []string{"Status 2"}) {
		t.Fatalf("simulator saw %v", e.sim.Commands())
	}
	ev := e.rec.all()
	if len(ev) != 1 || ev[0].Action != "command.send" || ev[0].Actor != "oidc:olli" || ev[0].Target != "device:7" || ev[0].Result != "ok" {
		t.Fatalf("audit: %+v", ev)
	}
	if !strings.Contains(ev[0].Detail, `"Status 2"`) {
		t.Errorf("audit must hold the command: %s", ev[0].Detail)
	}
}

func TestRunAdminMayWriteSettings(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	if _, err := e.svc.Run(context.Background(), admin(), 7, "TelePeriod 120"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.sim.Commands(), "TelePeriod 120") {
		t.Fatalf("simulator saw %v", e.sim.Commands())
	}
}

// Why: secrets typed in the console must reach neither the audit log nor the
// history (which is built from it), whether the command is sent (admin) or
// refused (operator, malformed). Admins may now send credential commands and
// command executors, so the redaction is the only thing between them and the log.
func TestSecretsNeverReachTheAuditLog(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	_, _ = e.svc.Run(context.Background(), admin(), 7, "MqttPassword hunter2-secret")
	_, _ = e.svc.Run(context.Background(), admin(), 7, "SSId1 Home-Net")
	_, _ = e.svc.Run(context.Background(), operator(), 7, "WebPassword hunter2-secret")
	_, _ = e.svc.Run(context.Background(), admin(), 7, "MqttPassword hunter2-secret;Reset 1")
	_, _ = e.svc.Run(context.Background(), admin(), 7, "Backlog WebPassword hunter2-secret; Power1 ON")
	_, _ = e.svc.Run(context.Background(), admin(), 7, `Json {"Password1":"hunter2-secret"}`)
	_, _ = e.svc.Run(context.Background(), admin(), 7, "Rule1 ON x DO MqttPassword hunter2-secret ENDON")
	_, _ = e.svc.Run(context.Background(), admin(), 7, "Color hunter2-secret")
	if len(e.rec.all()) != 8 {
		t.Fatalf("events: %+v", e.rec.all())
	}
	for _, ev := range e.rec.all() {
		blob := ev.Action + ev.Target + ev.Detail + ev.Result
		for _, s := range []string{"hunter2", "Home-Net"} {
			if strings.Contains(blob, s) {
				t.Errorf("secret %q leaked into %+v", s, ev)
			}
		}
	}
	if !strings.Contains(e.rec.all()[0].Detail, "MqttPassword ***") {
		t.Errorf("redacted form missing: %s", e.rec.all()[0].Detail)
	}
	if !strings.Contains(e.rec.all()[4].Detail, "Backlog WebPassword ***; Power1 ON") {
		t.Errorf("a Backlog is logged part by part: %s", e.rec.all()[4].Detail)
	}
	// Admins send everything that is well-formed, as typed; the log holds
	// only the redacted form.
	want := []string{"MqttPassword hunter2-secret", "SSId1 Home-Net", "Backlog WebPassword hunter2-secret; Power1 ON",
		`Json {"Password1":"hunter2-secret"}`, "Rule1 ON x DO MqttPassword hunter2-secret ENDON", "Color hunter2-secret"}
	if got := e.sim.Commands(); !slices.Equal(got, want) {
		t.Errorf("device received %v", got)
	}
}

// Why (decision 2026-10-21): admins may send formerly forbidden commands; the
// device receives exactly the canonical text, and the audit row says it was
// sent, redacted.
func TestRunAdminSendsFormerlyForbiddenCommands(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	for _, cmd := range []string{"Backlog Power ON", "Sensor13 1", "SetOption85 1", "Status 0", "Timer1 {}"} {
		if _, err := e.svc.Run(context.Background(), admin(), 7, cmd); errors.Is(err, cmdpolicy.ErrForbidden) || errors.Is(err, cmdpolicy.ErrRole) || errors.Is(err, cmdpolicy.ErrInvalid) {
			t.Errorf("Run(%q) refused by the policy: %v", cmd, err)
		}
	}
	for _, ev := range e.rec.all() {
		if ev.Action != "command.send" {
			t.Errorf("not sent: %+v", ev)
		}
	}
}

func TestRunDeviceErrorsAreAuditedAndReturned(t *testing.T) {
	spec := simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M,
		Faults: []simulator.Fault{{Kind: simulator.FaultUnknownCommand, After: "Time"}}}
	e := setup(t, spec, model.MQTTNone, nil)
	_, err := e.svc.Run(context.Background(), operator(), 7, "Time")
	if !errors.Is(err, tasmota.ErrUnknownCommand) {
		t.Fatalf("got %v", err)
	}
	ev := e.rec.all()
	if len(ev) != 1 || ev[0].Result == "ok" {
		t.Fatalf("audit: %+v", ev)
	}
}

func TestRunUnknownDevice(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	if _, err := e.svc.Run(context.Background(), operator(), 99, "Status 2"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// Why: classify one string, send another is the classic bypass. Blanks around
// and inside the command must not change what the policy judged.
func TestRunSendsTheCanonicalCommand(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	if _, err := e.svc.Run(context.Background(), operator(), 7, "  Status   2  "); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.sim.Commands(), []string{"Status 2"}) {
		t.Fatalf("simulator saw %q", e.sim.Commands())
	}
	if d := e.rec.all()[0].Detail; !strings.Contains(d, `"Status 2"`) {
		t.Fatalf("audit holds %s", d)
	}
}

// Why: the role/class mapping is enforced here for every spelling of the
// power command, not only in the web handler.
func TestRunPowerVariantsPerRole(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	for _, tc := range []struct {
		cmd  string
		p    model.Principal
		want error
	}{
		{"Power8 ON", operator(), nil},
		{"Power0 1", operator(), cmdpolicy.ErrRole},
		{"Power0 1", admin(), nil},
		{"Power9 ON", operator(), cmdpolicy.ErrRole},
		{"Power9 ON", admin(), nil},
		{"Power1 BLINK", operator(), cmdpolicy.ErrRole},
		{"power1 toggle", operator(), nil},
		{"Power1   TOGGLE;Reset 1", admin(), cmdpolicy.ErrInvalid},
		{"Power;1", admin(), cmdpolicy.ErrInvalid},
	} {
		if _, err := e.svc.Run(context.Background(), tc.p, 7, tc.cmd); !errors.Is(err, tc.want) {
			t.Errorf("Run(%v, %q) = %v, want %v", tc.p.Role, tc.cmd, err, tc.want)
		}
	}
	for _, c := range e.sim.Commands() {
		if strings.ContainsAny(c, ";") || strings.HasPrefix(strings.ToLower(c), "reset") {
			t.Errorf("device received %q", c)
		}
	}
}

// Why: a failing send must not put the typed secret into the error, which is
// shown in the UI, nor into the audit result.
func TestErrorsNeverEchoTheCommand(t *testing.T) {
	for _, kind := range []string{simulator.FaultAuth, simulator.FaultDrop, simulator.FaultUnknownCommand} {
		t.Run(kind, func(t *testing.T) {
			spec := simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M,
				Faults: []simulator.Fault{{Kind: kind}}}
			e := setup(t, spec, model.MQTTNone, nil)
			_, err := e.svc.Run(context.Background(), admin(), 7, "MqttPassword hunter2-secret")
			if err == nil {
				t.Fatal("fault should fail the command")
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error echoes the secret: %v", err)
			}
			for _, ev := range e.rec.all() {
				if strings.Contains(ev.Detail+ev.Result, "hunter2") {
					t.Errorf("audit echoes the secret: %+v", ev)
				}
			}
		})
	}
	// Refusals too.
	e := setup(t, plug, model.MQTTNone, nil)
	for _, cmd := range []string{"MqttPassword hunter2-secret;Reset 1", "Reset\u200b hunter2-secret", "Backlog MqttPassword hunter2-secret\u037eReset"} {
		_, err := e.svc.Run(context.Background(), admin(), 7, cmd)
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("Run(%q) error %v", cmd, err)
		}
	}
	for _, ev := range e.rec.all() {
		if strings.Contains(ev.Detail+ev.Result, "hunter2") {
			t.Errorf("audit echoes the secret: %+v", ev)
		}
	}
}

type echoClient struct {
	tasmota.Client
	reply string
	sent  []string
}

func (c *echoClient) Command(_ context.Context, _ tasmota.Target, cmd string) (json.RawMessage, error) {
	c.sent = append(c.sent, cmd)
	return json.RawMessage(c.reply), nil
}

// Why: Status replies and rule bodies can hold credentials; the console must
// not become a way to read them.
func TestRunScrubsCredentialsFromReplies(t *testing.T) {
	rec := &recorder{}
	cl := &echoClient{reply: `{"StatusMQT":{"MqttHost":"b","MqttUser":"bob"},"Rule1":"WebSend [bob:hunter2@h] x","Url":"http://u:hunter2@h/"}`}
	svc := New(Options{Devices: devices{7: {ID: 7, Address: "127.0.0.1"}}, Client: cl, Audit: rec})
	raw, err := svc.Run(context.Background(), admin(), 7, "Status 6")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"hunter2", "bob"} {
		if strings.Contains(string(raw), s) {
			t.Errorf("%q survived in %s", s, raw)
		}
	}
	if !strings.Contains(string(raw), `"MqttHost":"b"`) {
		t.Errorf("harmless fields must stay: %s", raw)
	}
}

// Why: an authorised command that cannot even be attempted (unknown device)
// still leaves a trace, so attempts are never silent.
func TestRunUnknownDeviceIsAudited(t *testing.T) {
	e := setup(t, plug, model.MQTTNone, nil)
	_, _ = e.svc.Run(context.Background(), operator(), 99, "Status 2")
	ev := e.rec.all()
	if len(ev) != 1 || ev[0].Result == "ok" || ev[0].Target != "device:99" {
		t.Fatalf("audit: %+v", ev)
	}
}

// Why: a RESULT that disagrees with the request (a button press, a stale reply) is not the
// confirmation of an explicit ON or OFF; showing it as the new state would hide that the switch
// did not do what was asked. TOGGLE has no expected state and takes what the device reports.
func TestPowerOverMQTTReplyThatContradictsTheRequestIsUnconfirmed(t *testing.T) {
	bus := &fakeBus{connected: true}
	bus.answer = func(topic, _, _ string) *mqttbus.Message {
		return &mqttbus.Message{Kind: mqttbus.KindResult, Topic: topic, Suffix: "RESULT", Payload: []byte(`{"POWER":"OFF"}`)}
	}
	e := setup(t, plug, model.MQTTOurs, bus)
	if _, err := e.svc.Power(context.Background(), "x", 7, 1, "ON"); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("ON answered OFF: %v", err)
	}
	if state, err := e.svc.Power(context.Background(), "x", 7, 1, "TOGGLE"); err != nil || state != "OFF" {
		t.Fatalf("TOGGLE: %q, %v", state, err)
	}
}

// stubs for a router that must send over MQTT.
type refusingHTTP struct{ tasmota.Client }

func (refusingHTTP) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	return nil, tasmota.ErrForbiddenAddr
}

type stubFacts struct{ mem *transport.Memory }

func (f stubFacts) HTTPState(id model.DeviceID) transport.HTTPState {
	if k, since, ok := f.mem.HTTPFailure(id); ok {
		return transport.HTTPState{State: transport.HTTPFailing, Kind: k, Since: since}
	}
	return transport.HTTPState{}
}
func (f stubFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	return transport.Facts{Now: now, HTTP: f.HTTPState(d.ID), MQTT: transport.MQTTFacts{BusConnected: true, Ours: true, Topic: d.Topic,
		TopicExclusive: true, LWTOnline: true, GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour)}}, nil
}

type stubMQTT struct{ reply string }

func (s stubMQTT) Do(context.Context, model.Device, string) (json.RawMessage, error) {
	return json.RawMessage(s.reply), nil
}
func (s stubMQTT) DoLoose(ctx context.Context, d model.Device, c string) (json.RawMessage, error) {
	return s.Do(ctx, d, c)
}
func (stubMQTT) Connected() bool { return true }

// Why (D5/D18): the audit says which transport carried a console command, a secret-setting reply
// never reaches the caller unmasked.
func TestRunOverMQTTIsAuditedAndScrubbed(t *testing.T) {
	dev := model.Device{ID: 7, Address: "192.0.2.7", Topic: "plug", MQTT: model.MQTTOurs}
	mem := transport.NewMemory(time.Minute, time.Now)
	r := transport.NewRouter(transport.Options{HTTP: refusingHTTP{}, Devices: routerDevices{devices{7: dev}}, Facts: stubFacts{mem}, Memory: mem})
	r.SetMQTT(stubMQTT{reply: `{"WebPassword":"hunter2"}`})
	rec := &recorder{}
	svc := New(Options{Devices: devices{7: dev}, Client: r, Audit: rec})
	raw, err := svc.Run(t.Context(), model.Principal{Subject: "admin", Role: model.RoleAdmin}, 7, "WebPassword hunter2")
	if err != nil || strings.Contains(string(raw), "hunter2") {
		t.Fatalf("raw=%s err=%v", raw, err)
	}
	ev := rec.all()
	if len(ev) != 1 || !strings.Contains(ev[0].Detail, `"via":"mqtt"`) || strings.Contains(ev[0].Detail, "hunter2") {
		t.Fatalf("audit = %+v", ev)
	}
}

type routerDevices struct{ devices }

func (d routerDevices) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	return d.Get(ctx, id)
}
