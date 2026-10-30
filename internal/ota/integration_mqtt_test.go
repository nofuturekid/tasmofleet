//go:build integration

package ota

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Updates over MQTT against the simulator: the real engine, router, command channel, bus and broker.
// The devices answer nothing over HTTP (http_unreachable), so a command that works went over MQTT.

// itFacts are the facts of healthy devices on our broker; the HTTP state comes from the router's memory,
// which a test fills the way the health poll does.
type itFacts struct {
	mem *transport.Memory
	mu  sync.Mutex
	fw  map[model.DeviceID]model.FWReach
}

func (f *itFacts) HTTPState(id model.DeviceID) transport.HTTPState {
	if kind, since, ok := f.mem.HTTPFailure(id); ok {
		return transport.HTTPState{State: transport.HTTPFailing, Kind: kind, Since: since}
	}
	return transport.HTTPState{}
}

func (f *itFacts) FactsFor(_ context.Context, d model.Device) (transport.Facts, error) {
	now := time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	fw := d.FWReach
	if r, ok := f.fw[d.ID]; ok {
		fw = r
	}
	return transport.Facts{
		Now: now, HTTP: f.HTTPState(d.ID),
		MQTT: transport.MQTTFacts{BusConnected: true, Ours: d.MQTT == model.MQTTOurs, Topic: d.Topic, TopicExclusive: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: now, LastProbeAt: now.Add(-time.Hour), LWTOnline: true},
		FWReach: fw, FirmwareBaseURL: true, Identity: d.Identity,
	}, nil
}

type itPaths struct{ f *itFacts }

func (p itPaths) UpdatePath(ctx context.Context, d model.Device, q PathQuery) (transport.Paths, error) {
	f, err := p.f.FactsFor(ctx, d)
	if err != nil {
		return transport.Paths{}, err
	}
	f.Request = transport.Request{Delivery: q.Delivery, HTTPPush: q.HTTPPush, HTTPPushNote: q.HTTPPushNote}
	return transport.Choose(f), nil
}

// lossy runs a command on the device and then loses its reply, once, for one command text.
type lossy struct {
	transport.MQTTDoer
	mu      sync.Mutex
	loseCmd string
	lost    int
}

func (l *lossy) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	raw, err := l.MQTTDoer.Do(ctx, d, cmd)
	l.mu.Lock()
	defer l.mu.Unlock()
	if err == nil && l.loseCmd != "" && strings.EqualFold(strings.TrimSpace(cmd), l.loseCmd) && l.lost == 0 {
		l.lost++
		return nil, tasmota.ErrTimeout // the command ran, the answer never came: an unknown outcome
	}
	return raw, err
}

// Info passes the channel's INFO1 cache through (the router asks for it by interface).
func (l *lossy) Info(topic string) (mqttcmd.Info, bool) {
	return l.MQTTDoer.(interface {
		Info(string) (mqttcmd.Info, bool)
	}).Info(topic)
}

type itMQTT struct {
	*itEnv
	broker *simulator.Broker
	ch     *mqttcmd.Channel
	mem    *transport.Memory
	facts  *itFacts
	lossy  *lossy
	router *transport.Router
}

func newItMQTT(t *testing.T) *itMQTT {
	t.Helper()
	broker, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() }) // test teardown
	e := newItEnvMQTT(t, broker.URL())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: broker.URL(), ClientID: "tasmofleet-it-mqtt"})
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !bus.Connected(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
	}
	ch := mqttcmd.New(bus, mqttcmd.Options{Gap: 5 * time.Millisecond})
	t.Cleanup(ch.Close)
	m := &itMQTT{itEnv: e, broker: broker, ch: ch, mem: transport.NewMemory(transport.DefaultFailFor, time.Now)}
	m.facts = &itFacts{mem: m.mem}
	m.lossy = &lossy{MQTTDoer: ch}
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, Timeout: 800 * time.Millisecond})
	router := transport.NewRouter(transport.Options{HTTP: client, Devices: e.st, Memory: m.mem})
	m.router = router
	router.SetFacts(m.facts)
	router.SetMQTT(m.lossy)
	api := NewDeviceAPI(router)
	api.(*deviceAPI).gap = 5 * time.Millisecond
	e.api = &guardedAPI{DeviceAPI: api, env: e}
	e.power = devicecmd.New(devicecmd.Options{Devices: storeDevices{e.st}, Client: router, Audit: e.audits, MQTT: ch})
	e.paths = itPaths{m.facts}
	unsub := bus.Subscribe([]mqttbus.Kind{mqttbus.KindUpgrade}, func(msg mqttbus.Message) {
		var p struct{ Upgrade string }
		if json.Unmarshal(msg.Payload, &p) != nil {
			return
		}
		d, err := e.st.GetDeviceByTopic(context.Background(), msg.Topic)
		if err != nil {
			return
		}
		if eng := e.engine.Load(); eng != nil {
			eng.Hint(d.ID, p.Upgrade)
		}
	})
	t.Cleanup(unsub)
	time.Sleep(300 * time.Millisecond) // subscriptions are established asynchronously
	return m
}

// addMQTT adds a device that only MQTT reaches and marks its HTTP as failing, as the health poll would.
func (m *itMQTT) addMQTT(spec simulator.DeviceSpec) model.DeviceID {
	m.t.Helper()
	spec.Faults = append(spec.Faults, simulator.Fault{Kind: simulator.FaultHTTPUnreachable})
	id := m.addDevice(spec)
	d, err := m.st.GetDevice(context.Background(), id)
	if err != nil {
		m.t.Fatal(err)
	}
	d.MQTT = model.MQTTOurs
	if err := m.st.UpdateDevice(context.Background(), d); err != nil {
		m.t.Fatal(err)
	}
	m.mem.Fail(id, transport.HTTPFailUnreachable)
	// a command before the channel saw the device come online is refused as offline
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := m.ch.Do(context.Background(), d, "Status 5"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			m.t.Fatal("device never answered over MQTT")
		}
	}
	return id
}

func (m *itMQTT) wantTransport(dev model.DeviceID) model.Job {
	m.t.Helper()
	j := m.jobOf(dev)
	if j.Transport != model.TransportMQTT {
		m.t.Fatalf("job transport = %q, want mqtt", j.Transport)
	}
	return j
}

// Staged ESP8266 over MQTT end to end with the power restore: both Upgrades and every probe go through
// the broker (the device answers nothing over HTTP), the minimal window is bridged by the LWT, and the
// light comes back on. The language build cannot be read over MQTT, so the person chose it (D10).
func TestITMQTTStagedESP8266WithPowerRestore(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M, Light: true})
	m.setRelays(dev, true)
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	j := m.expectSuccess(dev, orig, 2, model.PlanStaged)
	m.wantRelays(dev, true)
	if j.Transport != model.TransportMQTT {
		t.Fatalf("job transport = %q, the job must say it went over MQTT", j.Transport)
	}
}

// Why: without the person's language choice Select refuses (never guess DE vs EN), and the preview says
// the choice is what is missing instead of a bare refusal.
func TestITMQTTLanguageChoiceRequired(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	eng := m.start(2)
	items, err := eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease}})
	if err != nil || len(items) != 1 {
		t.Fatal(err, items)
	}
	if it := items[0]; it.Decision != decisionRefuse || !it.LanguageChoice {
		t.Fatalf("preview %+v, want a refusal that asks for the language", it)
	}
	if _, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true}}); err == nil {
		t.Fatal("Submit accepted a device whose language build is unknown")
	}
	for _, c := range m.ctrl[dev].Commands() {
		if l := strings.ToLower(c); strings.HasPrefix(l, "otaurl http") || strings.HasPrefix(l, "upgrade 1") {
			t.Fatalf("refused device received %q", c)
		}
	}
	// With the choice the German build is selected and planned; the language was no longer the problem.
	items, err = eng.Preview(context.Background(), []Request{{DeviceID: dev, Release: itRelease, Language: "de", SkipBackup: true}})
	if err != nil || items[0].LanguageChoice || !items[0].LanguageDeclared || items[0].Decision == decisionRefuse || items[0].Filename != "tasmota-DE.bin.gz" {
		t.Fatalf("with the choice: %+v %v", items[0], err)
	}
}

// Why (safety rule 1): a Guard refusal must publish nothing. An unknown chip is refused at Submit, and
// not one OtaUrl or Upgrade reaches the broker for it.
func TestITMQTTUnknownChipPublishesNothing(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileUnknownChip})
	eng := m.start(2)
	_, err := eng.Submit(context.Background(), "it", []Request{{DeviceID: dev, Release: itRelease, SkipBackup: true, Language: "en"}})
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("Submit: %v, want a refusal", err)
	}
	for _, c := range m.ctrl[dev].Commands() {
		l := strings.ToLower(c)
		if strings.HasPrefix(l, "upgrade 1") || strings.HasPrefix(l, "otaurl http") {
			t.Fatalf("refused device received %q", c)
		}
	}
}

// Why (safety rule 3): the Upgrade 1 ran but its answer was lost. The engine probes, sees the download
// (or the new version) and never sends a second Upgrade 1.
func TestITMQTTLostUpgradeResultIsProbedNotResent(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	m.lossy.loseCmd = "Upgrade 1"
	orig := m.ctrl[dev].OtaURL()
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	m.expectSuccess(dev, orig, 1, model.PlanDirect)
	m.wantTransport(dev)
	if m.lossy.lost != 1 {
		t.Fatal("the test never lost a reply")
	}
}

// Why (D9): "Failed HTTP error: connection failed" says the device cannot reach the firmware server.
// A second Upgrade 1 would take the same minute to fail the same way, so the job fails at once with that
// reason and sends exactly one.
func TestITMQTTFirmwareServerUnreachableFailsAfterOneUpgrade(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M,
		Faults: []simulator.Fault{{Kind: simulator.FaultFWUnreachable, After: "Upgrade 1", Duration: 100 * time.Millisecond}}})
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	j := m.waitTerminal(dev)
	if j.State != model.JobFailed || !strings.Contains(j.LastError, ErrTextFWUnreachable) {
		t.Fatalf("job %s %q", j.State, j.LastError)
	}
	time.Sleep(200 * time.Millisecond)
	if n := m.upgrades(dev); n != 1 {
		t.Fatalf("Upgrade 1 sent %d times, want 1", n)
	}
	m.assertNoViolations()
}

// Why: the job's path is fixed when it is made. A restarted process (no chooser, nothing in memory)
// resumes it over MQTT and does not ask HTTP, which cannot answer.
func TestITMQTTJobTransportSurvivesEngineRestart(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	orig := m.ctrl[dev].OtaURL()
	crashed := m.crashAt(model.JobUpgradeIssued, 1)
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: dev, Language: "en"})
	select {
	case <-crashed:
	case <-time.After(60 * time.Second):
		t.Fatal("job never reached upgrade_issued")
	}
	m.stop()
	m.clearHook()
	m.paths = nil // the new process asks nobody; the stored transport decides
	if j := m.jobOf(dev); j.State.Terminal() || j.Transport != model.TransportMQTT {
		t.Fatalf("job %s transport %q at the crash", j.State, j.Transport)
	}
	m.start(2)
	m.expectSuccess(dev, orig, 1, model.PlanDirect)
}

// Why (H1): over MQTT the language cannot be read after the flash, so the verify step must not hold the
// person's choice (DE) against the "unknown" it reads back. Staged and direct both end Succeeded, the
// German file was flashed, and the light is back on.
func TestITMQTTLanguageChoiceDoesNotFailTheVerify(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile simulator.Profile
		upgr    int
		plan    model.Plan
	}{{"staged", simulator.ProfileESP8266_1M, 2, model.PlanStaged}, {"direct", simulator.ProfileESP8266_4M, 1, model.PlanDirect}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newItMQTT(t)
			dev := m.addMQTT(simulator.DeviceSpec{Profile: tc.profile, Light: true})
			m.setRelays(dev, true)
			orig := m.ctrl[dev].OtaURL()
			eng := m.start(2)
			m.submit(eng, Request{DeviceID: dev, Language: "de"})
			m.expectSuccess(dev, orig, tc.upgr, tc.plan)
			flashed := false
			for _, c := range m.ctrl[dev].Commands() {
				flashed = flashed || strings.HasSuffix(c, "/fw/"+itRelease+"/tasmota-DE.bin.gz")
			}
			if !flashed {
				t.Fatalf("the German file was never set as OtaUrl: %v", m.ctrl[dev].Commands())
			}
			m.wantRelays(dev, true)
			if d, _ := m.st.GetDevice(context.Background(), dev); d.Identity.Version != itRelease {
				t.Fatalf("inventory shows %q, want the verified %s", d.Identity.Version, itRelease)
			}
		})
	}
}

// Why: an INFO1 from before a restart describes the old firmware. The channel drops it on the LWT
// Offline the restart publishes, and the router's page must then fail until the new boot announces
// itself, never answer from the old one.
func TestITMQTTPreRebootInfo1IsNeverUsed(t *testing.T) {
	m := newItMQTT(t)
	dev := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M, Faults: []simulator.Fault{{After: "Restart 1", Kind: simulator.FaultLongReboot, Duration: 600 * time.Millisecond}}})
	d, _ := m.st.GetDevice(context.Background(), dev)
	ctx := transport.WithPath(context.Background(), transport.MQTT)
	page := func() (tasmota.RootInfo, error) { return m.router.RootPage(ctx, tasmota.TargetFor(d)) }
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := page(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("no INFO1 seen at boot in this run") // the claim needs one to lose
		}
	}
	if _, err := m.ch.Do(context.Background(), d, "Restart 1"); err != nil {
		t.Fatal(err)
	}
	sawGap := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		_, err := page()
		if errors.Is(err, transport.ErrNoInfo) {
			sawGap = true
		}
		if err == nil && sawGap {
			return // the new boot's INFO1, after a window in which there was none
		}
	}
	t.Fatalf("no gap and new INFO1 seen (gap=%v)", sawGap)
}

// Why (M1): an HTTP job and a staged MQTT job in one batch both finish; neither path disturbs the other.
func TestITMQTTMixedBatchWithAnHTTPJob(t *testing.T) {
	m := newItMQTT(t)
	mq := m.addMQTT(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M})
	plain := m.addDevice(simulator.DeviceSpec{Profile: simulator.ProfileESP8266_4M})
	origMQ, origPlain := m.ctrl[mq].OtaURL(), m.ctrl[plain].OtaURL()
	eng := m.start(2)
	m.submit(eng, Request{DeviceID: mq, Language: "en"}, Request{DeviceID: plain, Language: "en"})
	m.expectSuccess(mq, origMQ, 2, model.PlanStaged)
	m.expectSuccess(plain, origPlain, 1, model.PlanDirect)
	if j := m.jobOf(plain); j.Transport != model.TransportHTTP {
		t.Fatalf("the reachable device went over %q", j.Transport)
	}
	m.wantTransport(mq)
}
