package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// "Refresh identity" brings every identity field up to date (#91) and refuses a device that is
// not the stored one (#65), over HTTP or, for a device only reachable on our broker, over MQTT.

// refusal is how the web layer recognises a refused refresh (devices.go refreshProblem).
type refusal interface{ Refusal() string }

func isRefusal(err error) bool {
	var r refusal
	return errors.As(err, &r) && r.Refusal() != ""
}

func (a *idAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.ev {
		out = append(out, e.Action)
	}
	return out
}

func (a *idAudit) find(action string) (model.AuditEvent, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.ev {
		if e.Action == action {
			return e, true
		}
	}
	return model.AuditEvent{}, false
}

// Why (#91): after "Refresh identity" the device page must show what the device reports now:
// device name, hardware, build, chip, firmware, hostname, MAC and MQTT topic. Before, the name
// was never written, the hostname only arrived later in the background (the card showed
// "Unknown" right after the refresh) and the topic was only repaired when unresolved.
func TestRefreshUpdatesEveryIdentityField(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	// Stored as an older version left it: named after the hostname, no topic, no identity.
	d.Name, d.Topic = "plug-1", ""
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status16}}
	p.Now = func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) }
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ field, got, want string }{
		{"device name", got.Name, "Tasmota TV Wand"},
		{"hardware", got.Identity.Hardware, "ESP8285N08"},
		{"build", got.Identity.Variant, "tasmota"},
		{"chip", string(got.Identity.Family), string(model.ChipESP8266)},
		{"hostname", got.Net.Hostname, "plug-1"},
		{"IP", got.Net.IP, "192.0.2.10"},
		{"MAC", got.MAC, "aa:bb:cc:00:00:01"},
		{"MQTT topic", got.Topic, "tasmota_2BE970"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if got.Identity.Version == "" || got.Identity.DetectedAt.IsZero() || got.Identity.FlashSizeKB == 0 {
		t.Errorf("firmware/flash not refreshed: %+v", got.Identity)
	}
	if got.Net.ReadAt.IsZero() {
		t.Error("the network facts of Status 5 must be stored with the refresh, not only later")
	}
}

// Why (#52 decision, kept by #91): a name the user chose in tasmofleet is never overwritten by a
// refresh. The topic is not a user choice but a mirror of the device's (#111): it follows.
func TestRefreshKeepsAUserChosenNameButTopicFollows(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	d.Name, d.Topic = "Kitchen lamp", "my-topic"
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status16}}
	for range 2 {
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Name != "Kitchen lamp" || got.Topic != "tasmota_2BE970" {
		t.Fatalf("name %q topic %q, want the user's name and the device's topic", got.Name, got.Topic)
	}
}

// Why (#91): a name that is the device's own DeviceName follows it when it changes on the
// device; the Tasmota default DeviceName ("Tasmota") never replaces a hostname name, it would
// turn many distinct devices into "Tasmota".
func TestRefreshNameFollowsTheDeviceNameButNotTheDefault(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	status := func(name, topic string) string {
		b, _ := json.Marshal(map[string]any{"Status": map[string]string{"DeviceName": name, "Topic": topic}})
		return string(b)
	}
	d.Name = "plug-1" // the hostname, as older versions named devices
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	base := p.Client
	refresh := func(name, topic string) model.Device {
		t.Helper()
		p.Client = override{Client: base, reply: map[string]string{"Status 5": status5Net, "Status": status(name, topic)}}
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
		got, _ := st.GetDevice(context.Background(), id)
		return got
	}
	if got := refresh("Tasmota", "plug_a"); got.Name != "plug-1" {
		t.Fatalf("Name = %q, the default DeviceName replaced the hostname name", got.Name)
	}
	if got := refresh("Desk lamp", "plug_a"); got.Name != "Desk lamp" {
		t.Fatalf("Name = %q, want the DeviceName", got.Name)
	}
	if got := refresh("Desk lamp left", "plug_b"); got.Name != "Desk lamp left" || got.Topic != "plug_b" {
		t.Fatalf("name %q topic %q, want both to follow the device", got.Name, got.Topic)
	}
}

// Why (#65): if the stored address now belongs to another device (DHCP reshuffle, a spoofed
// claim), a refresh must not fill the stored device with the other device's identity, and the
// stored password must not go to that host more than the one command that read its MAC.
// The refusal is distinct (the device page explains it) and audited.
func TestRefreshRefusesADeviceWithAnotherMAC(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	audit := &idAudit{}
	p.Audit = audit
	var sent []string
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{
		"Status 5": `{"StatusNET":{"Hostname":"intruder","IPAddress":"192.0.2.10","Mac":"DE:AD:BE:EF:00:01"}}`,
	}}, &sent}
	err := p.Refresh(context.Background(), id, "admin")
	if !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if strings.Join(sent, ",") != "Status 5" {
		t.Fatalf("sent %v after the MAC mismatch, want only Status 5", sent)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Identity.Version != "" || got.Net.Hostname != "" || got.MAC != d.MAC || got.Name != d.Name {
		t.Fatalf("a refused refresh wrote %+v", got)
	}
	ev, ok := audit.find("device.refresh_refused")
	if !ok || ev.Actor != "admin" || ev.Result != "refused" || !strings.Contains(ev.Detail, "de:ad:be:ef:00:01") {
		t.Fatalf("audit %+v (all: %v)", ev, audit.actions())
	}
}

// Why (#65): a wired board answers with its Wi-Fi MAC and lists the Ethernet MAC too; either
// one in the stored MAC set is the same device.
func TestRefreshAcceptsTheEthernetMACOfTheStoredSet(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	cur, _ := st.GetDevice(context.Background(), id)
	// A device stored by its Ethernet MAC only.
	cur.MAC = "aa:bb:cc:00:00:e1"
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{
		"Status 5": `{"StatusNET":{"Hostname":"eth","IPAddress":"0.0.0.0","Mac":"AA:BB:CC:00:00:01","Ethernet":{"Hostname":"eth","IPAddress":"192.0.2.10","Gateway":"192.0.2.1","Mac":"AA:BB:CC:00:00:E1"}}}`,
	}}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatalf("the Ethernet MAC is the stored device: %v", err)
	}
}

// Why (#65, D8): a device stored without a MAC (import) has nothing to compare with. An admin's
// refresh binds the MAC it read, audited, so every later probe can tell another device at the
// address. An automatic refresh (discovery's reprobe, triggered by an MQTT message anyone can
// publish) never binds: binding stays an admin action.
func TestRefreshBindsTheMACOnlyForAnAdmin(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	audit := &idAudit{}
	p.Audit = audit
	cur, _ := st.GetDevice(context.Background(), id)
	cur.MAC = ""
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" {
		t.Fatalf("an automatic refresh bound MAC %q", got.MAC)
	}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.MAC != d.MAC {
		t.Fatalf("MAC = %q, want the one the device reported, %q", got.MAC, d.MAC)
	}
	ev, ok := audit.find("device.mac_bound")
	if !ok || ev.Actor != "admin" || !strings.Contains(ev.Detail, got.MAC) {
		t.Fatalf("audit %+v (all: %v)", ev, audit.actions())
	}
}

// Why (#65): binding a MAC that another stored device holds would give two records one device;
// the address answers as that other device, so the refresh is refused like a mismatch.
func TestRefreshRefusesToBindAMACOfAnotherDevice(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	p.Audit = &idAudit{}
	cur, _ := st.GetDevice(context.Background(), id)
	cur.MAC = ""
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDevice(context.Background(), model.Device{Name: "owner", Address: "192.0.2.77", MAC: "aa:bb:cc:00:00:01"}); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net}}
	if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" || got.Identity.Version != "" {
		t.Fatalf("a refused refresh wrote %+v", got)
	}
}

// fakeAsker answers Status N over "MQTT" from a table and records what was asked.
type fakeAsker struct {
	mu      sync.Mutex
	replies map[int]string
	asked   []int
	err     error
	busy    map[int]int // answer errMQTTBusy this many times for Status N
}

func (f *fakeAsker) askStatus(_ context.Context, _ model.Device, n int, _ string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, n)
	if f.err != nil {
		return nil, f.err
	}
	if f.busy[n] > 0 {
		f.busy[n]--
		return nil, errMQTTBusy
	}
	r, ok := f.replies[n]
	if !ok {
		return nil, tasmota.ErrTimeout
	}
	return json.RawMessage(r), nil
}

// unreachable is a device client for a device HTTP cannot reach.
type unreachable struct{ tasmota.Client }

func (unreachable) Command(context.Context, tasmota.Target, string) (json.RawMessage, error) {
	return nil, tasmota.ErrUnreachable
}

func mqttOnlySetup(t *testing.T, cap model.MQTTCapability) (*Prober, *store.SQLite, model.DeviceID, *fakeAsker) {
	t.Helper()
	p, st, id, _, _ := raceSetup(t)
	cur, _ := st.GetDevice(context.Background(), id)
	cur.Topic, cur.MQTT = "plug_a", cap
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	p.Client = unreachable{}
	f := &fakeAsker{replies: map[int]string{5: status5Net, 2: status2Payload, 1: `{"StatusPRM":{"GroupTopic":"tasmotas"}}`}}
	p.MQTT = f
	p.Audit = &idAudit{}
	return p, st, id, f
}

// Why (#91, #79 transport rule): a device on our broker that HTTP cannot reach is refreshed over
// MQTT: Status 5 (MAC, hostname, IP) first, then Status 2 (firmware, hardware), one at a time,
// never Status 0. MQTT never sets DetectedAt, so the first HTTP probe still replaces it (#87).
func TestRefreshOverMQTTWhenHTTPCannotReachTheDevice(t *testing.T) {
	p, st, id, f := mqttOnlySetup(t, model.MQTTOurs)
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.asked) != 4 || f.asked[0] != 5 || f.asked[1] != 2 || f.asked[2] != 1 || f.asked[3] != statusObject {
		t.Fatalf("asked %v, want Status 5, Status 2, Status 1 (GroupTopic), then the plain Status (DeviceName)", f.asked)
	}
	got, _ := st.GetDevice(context.Background(), id)
	// Why (#279): a transfer over MQTT needs a fresh GroupTopic, and HTTP cannot give it for this device.
	if got.GroupTopic != "tasmotas" || got.GroupTopicReadAt.IsZero() {
		t.Errorf("GroupTopic %q read at %v: the MQTT refresh did not store it", got.GroupTopic, got.GroupTopicReadAt)
	}
	if got.Identity.Version != "16.0.0" || got.Identity.Hardware != "ESP8266EX" || got.Net.Hostname != "plug-1" {
		t.Fatalf("not refreshed over MQTT: %+v %+v", got.Identity, got.Net)
	}
	if !got.Identity.DetectedAt.IsZero() {
		t.Fatal("MQTT set DetectedAt: the next HTTP probe would no longer replace it")
	}
}

// Why (#65 over MQTT): the MAC rule holds on the MQTT path too. Another MAC in the Status 5
// answer: refused, audited, Status 2 never asked, nothing written.
func TestRefreshOverMQTTRefusesAnotherMAC(t *testing.T) {
	p, st, id, f := mqttOnlySetup(t, model.MQTTOurs)
	f.replies[5] = `{"StatusNET":{"Hostname":"x","IPAddress":"192.0.2.10","Mac":"DE:AD:BE:EF:00:01"}}`
	if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if len(f.asked) != 1 {
		t.Fatalf("asked %v after the mismatch", f.asked)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Identity.Version != "" || got.Net.Hostname != "" {
		t.Fatalf("a refused refresh wrote %+v", got)
	}
	if _, ok := p.Audit.(*idAudit).find("device.refresh_refused"); !ok {
		t.Fatal("refusal not audited")
	}
}

// Why (#87 review M1, safety rule 1): MQTT is a weaker source than HTTP. A refresh over MQTT
// never replaces an identity an HTTP probe stored (Guard works on that one), never takes a
// minimal/safeboot image from an answer (it would switch the update form's variant), and never
// binds a MAC.
func TestRefreshOverMQTTNeverOverridesHTTPOrBindsAMAC(t *testing.T) {
	p, st, id, _ := mqttOnlySetup(t, model.MQTTOurs)
	cur, _ := st.GetDevice(context.Background(), id)
	httpID := model.Identity{Family: model.ChipESP8266, Hardware: "ESP8285N08", Version: "15.0.0", Image: "tasmota", Variant: "tasmota", DetectedAt: time.Now()}
	if err := st.UpdateIdentity(context.Background(), id, httpID); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Identity.Version != "15.0.0" || got.Identity.Hardware != "ESP8285N08" {
		t.Fatalf("MQTT replaced the HTTP identity: %+v", got.Identity)
	}

	p2, st2, id2, f2 := mqttOnlySetup(t, model.MQTTOurs)
	f2.replies[2] = `{"StatusFWR":{"Version":"16.0.0(release-minimal)","Hardware":"ESP8266EX"}}`
	if err := p2.Refresh(context.Background(), id2, "admin"); err == nil {
		t.Fatal("a minimal image in a Status 2 answer was accepted")
	}
	if got, _ := st2.GetDevice(context.Background(), id2); got.Identity.Variant != "" {
		t.Fatalf("variant %q taken from MQTT", got.Identity.Variant)
	}

	cur.MAC = ""
	if err := st.UpdateDevice(context.Background(), cur); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" {
		t.Fatalf("MAC %q bound from an MQTT answer", got.MAC)
	}
}

// Why (#79): MQTT is used only for a device on OUR broker; any other device keeps the HTTP
// error (the page says why it could not be reached), and nothing is published.
func TestRefreshUsesMQTTOnlyForDevicesOnOurBroker(t *testing.T) {
	for _, c := range []model.MQTTCapability{model.MQTTOtherBroker, model.MQTTNone, model.MQTTUnknown} {
		t.Run(string(c), func(t *testing.T) {
			p, _, id, f := mqttOnlySetup(t, c)
			if err := p.Refresh(context.Background(), id, "admin"); !errors.Is(err, tasmota.ErrUnreachable) {
				t.Fatalf("err = %v, want the HTTP error", err)
			}
			if len(f.asked) != 0 {
				t.Fatalf("asked %v over MQTT for a device not on our broker", f.asked)
			}
		})
	}
	// Our broker, but MQTT cannot answer either: the HTTP error is what the user needs.
	p, _, id, f := mqttOnlySetup(t, model.MQTTOurs)
	f.err = errMQTTUnavailable
	if err := p.Refresh(context.Background(), id, "admin"); !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("err = %v, want the HTTP error", err)
	}
}

// Why (safety rule 2, #87): the MQTT path asks one Status N at a time per device, keeps one
// second between questions on the whole bus, takes only a live (not retained) reply on the
// device's own topic, and audits every question. Run against the simulator and a real broker.
func TestMQTTAskStatusAgainstASimulatedDevice(t *testing.T) {
	br, err := simulator.StartBroker()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = br.Close() }) // test teardown
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures", MQTTURL: br.URL()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() }) // test teardown
	dev, err := fleet.Add(ctx, simulator.DeviceSpec{Name: "esp32-plug", Profile: simulator.ProfileESP32, Version: "15.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(dev)
	d := oursDevice(1, dev.Topic())
	e := newIDEnv(d)
	bus, err := mqttbus.Connect(ctx, mqttbus.Options{BrokerURL: br.URL(), ClientID: "tasmofleet-test"})
	if err != nil {
		t.Fatal(err)
	}
	e.m.bus = bus
	e.m.cmd = mqttcmd.New(bus, mqttcmd.Options{})
	e.m.now = time.Now
	go e.m.run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for !bus.Connected() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	start := time.Now()
	raw5, err := e.m.askStatus(ctx, d, 5, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(string(raw5)), ctl.MAC()) {
		t.Fatalf("Status 5 answer %s lacks the MAC %s", raw5, ctl.MAC())
	}
	raw2, err := e.m.askStatus(ctx, d, 2, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw2), "StatusFWR") {
		t.Fatalf("Status 2 answer %s", raw2)
	}
	if time.Since(start) < mqttIDGap {
		t.Fatal("two questions on the bus within one second")
	}
	var asked []string
	for _, c := range dev.Commands() {
		asked = append(asked, strings.ToUpper(strings.TrimSpace(c)))
	}
	if strings.Join(asked, ",") != "STATUS 5,STATUS 2" {
		t.Fatalf("device received %q", asked)
	}
	ev, ok := e.audit.find("identity.mqtt_ask")
	if !ok || ev.Actor != "admin" {
		t.Fatalf("question not audited: %+v", e.audit.actions())
	}
}

// Why (one command at a time per device): while the background reader waits for its Status 2
// answer, a refresh does not put a second question to the same device.
func TestMQTTAskStatusWaitsForNoOtherQuestion(t *testing.T) {
	d := oursDevice(1, "plug")
	e := newIDEnv(d)
	e.m.asking["plug"] = true
	if _, err := e.m.askStatus(context.Background(), d, 5, "admin"); !errors.Is(err, errMQTTBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if len(e.bus.sent()) != 0 {
		t.Fatalf("sent %v", e.bus.sent())
	}
}

// credLog records every command and whether its Target carried the stored credentials.
type credLog struct {
	tasmota.Client
	mu    sync.Mutex
	sent  []string // "Status 5" or "Status 5 +cred"
	auth  bool     // the device has a web password: commands without credentials get ErrAuth
	reply map[string]string
}

func (c *credLog) Command(ctx context.Context, t tasmota.Target, cmd string) (json.RawMessage, error) {
	c.mu.Lock()
	entry := cmd
	if t.HasCredentials() {
		entry += " +cred"
	}
	c.sent = append(c.sent, entry)
	c.mu.Unlock()
	if c.auth && !t.HasCredentials() {
		return nil, tasmota.ErrAuth
	}
	if r, ok := c.reply[cmd]; ok {
		return json.RawMessage(r), nil
	}
	return c.Client.Command(ctx, t, cmd)
}

func (c *credLog) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	if c.auth && !t.HasCredentials() {
		return tasmota.RootInfo{}, tasmota.ErrAuth
	}
	return c.Client.RootPage(ctx, t)
}

func (c *credLog) withCredentials() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, s := range c.sent {
		if strings.HasSuffix(s, " +cred") {
			out = append(out, s)
		}
	}
	return out
}

// Why (#96 review): the MAC is checked before the stored password goes anywhere. Status 5 is
// asked without credentials first; a device without a web password answers it, and then the
// whole probe needs no credentials, so a device with another MAC never sees the password.
// Only a device that asks for a password (401) gets Status 5 again with the credentials.
func TestRefreshAsksTheMACWithoutCredentialsFirst(t *testing.T) {
	const otherMAC = `{"StatusNET":{"Hostname":"x","IPAddress":"192.0.2.10","Mac":"DE:AD:BE:EF:00:01"}}`
	setup := func(t *testing.T) (*Prober, *store.SQLite, model.DeviceID) {
		p, st, id, _, _ := raceSetup(t)
		cur, _ := st.GetDevice(context.Background(), id)
		cur.HTTPUser, cur.HTTPPassword = "admin", "stored-pw"
		if err := st.UpdateDevice(context.Background(), cur); err != nil {
			t.Fatal(err)
		}
		return p, st, id
	}
	t.Run("no web password, another MAC", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client, reply: map[string]string{"Status 5": otherMAC}}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if got := c.withCredentials(); len(got) != 0 {
			t.Fatalf("credentials went to another device: %v", got)
		}
	})
	t.Run("no web password, the stored device", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); err != nil {
			t.Fatal(err)
		}
		if got := c.withCredentials(); len(got) != 0 {
			t.Fatalf("a device without a web password got the credentials: %v", got)
		}
	})
	t.Run("web password, another MAC", func(t *testing.T) {
		p, _, id := setup(t)
		c := &credLog{Client: p.Client, auth: true, reply: map[string]string{"Status 5": otherMAC}}
		p.Client = c
		if err := p.Refresh(context.Background(), id, "admin"); !isRefusal(err) {
			t.Fatalf("err = %v, want a refusal", err)
		}
		if strings.Join(c.sent, ",") != "Status 5,Status 5 +cred" {
			t.Fatalf("sent %v, want Status 5 without, then with credentials, then nothing", c.sent)
		}
	})
}

// storedMinimal stores a simulated device that runs minimal firmware, with mac and an identity
// an earlier HTTP probe stored.
func storedMinimal(t *testing.T, mac string) (*Prober, *store.SQLite, model.DeviceID, model.Identity) {
	t.Helper()
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "minimal-plug", Profile: simulator.ProfileESP8266Min})
	st := newTestStore(t)
	d.Name, d.MAC = "plug", mac
	id, err := st.CreateDevice(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	before := model.Identity{Family: model.ChipESP8266, Hardware: "ESP8285N08", Version: "15.0.0", Image: "tasmota", Variant: "tasmota",
		DetectedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	if err := st.UpdateIdentity(context.Background(), id, before); err != nil {
		t.Fatal(err)
	}
	p.Devices = st
	return p, st, id, before
}

// Why (#96 review L2): minimal firmware answers no Status 5, so the MAC of a device that has
// one stored cannot be checked. The refresh must not then take the answers as the stored
// device's: the stored identity and its time stay, and the page says why.
func TestRefreshOfMinimalFirmwareWithAStoredMACKeepsTheIdentity(t *testing.T) {
	p, st, id, before := storedMinimal(t, "aa:bb:cc:00:00:01")
	var sent []string
	p.Client = recorder{override{Client: p.Client}, &sent}
	err := p.Refresh(context.Background(), id, "admin")
	if !isRefusal(err) {
		t.Fatalf("err = %v, want a refusal that explains the unchecked MAC", err)
	}
	if strings.Join(sent, ",") != "Status 5,Status 2" {
		t.Fatalf("sent %v, want nothing after Status 2 decided minimal", sent)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Identity.Version != before.Version || got.Identity.Variant != before.Variant || !got.Identity.DetectedAt.Equal(before.DetectedAt) {
		t.Fatalf("identity %+v, want the stored %+v", got.Identity, before)
	}
	// Without a stored MAC there is nothing to check: the minimal identity is stored.
	p2, st2, id2, _ := storedMinimal(t, "")
	if err := p2.Refresh(context.Background(), id2, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st2.GetDevice(context.Background(), id2); got.Identity.Variant != "minimal" {
		t.Fatalf("variant %q, want minimal", got.Identity.Variant)
	}
}

// Why (#96 review L4): only a real MAC is bound. An all-zero or malformed MAC in the answer would
// bind a value every broken device shares, and the next refresh would compare with it.
func TestRefreshBindsOnlyAValidMAC(t *testing.T) {
	for _, mac := range []string{"00:00:00:00:00:00", "not-a-mac", ""} {
		t.Run(mac, func(t *testing.T) {
			p, st, id, _, _ := raceSetup(t)
			audit := &idAudit{}
			p.Audit = audit
			cur, _ := st.GetDevice(context.Background(), id)
			cur.MAC = ""
			if err := st.UpdateDevice(context.Background(), cur); err != nil {
				t.Fatal(err)
			}
			p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": `{"StatusNET":{"Hostname":"h","IPAddress":"192.0.2.10","Mac":"` + mac + `"}}`}}
			if err := p.Refresh(context.Background(), id, "admin"); err != nil {
				t.Fatal(err)
			}
			if got, _ := st.GetDevice(context.Background(), id); got.MAC != "" {
				t.Fatalf("bound %q", got.MAC)
			}
			if _, ok := audit.find("device.mac_bound"); ok {
				t.Fatal("a binding was audited")
			}
		})
	}
}

// Why (#96 review L6): between the refresh's Status 5 and Status 2 the background reader may put
// its own Status 2 question to the device. The refresh waits for its turn once instead of failing.
func TestRefreshOverMQTTWaitsOnceWhenTheDeviceIsBusy(t *testing.T) {
	p, st, id, f := mqttOnlySetup(t, model.MQTTOurs)
	p.busyWait = time.Millisecond
	f.busy = map[int]int{2: 1}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.asked) != 5 || f.asked[2] != 2 {
		t.Fatalf("asked %v, want 5, 2, 2, 1, plain Status", f.asked)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Identity.Version != "16.0.0" {
		t.Fatalf("version %q", got.Identity.Version)
	}
}

func newTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	return st
}

// Why (#104): a device added while it still had Tasmota's default DeviceName is stored under
// the name "Tasmota", which tasmofleet filled in itself. If it is then renamed on the device and
// refreshed before any probe recorded the DeviceName, the stored name must follow it, else the
// header and the list keep the stale "Tasmota". A name the user typed is still kept.
func TestRefreshReplacesTheDefaultNameTasmofleetFilledIn(t *testing.T) {
	p, st, id, d, _ := raceSetup(t)
	d.Name = "Tasmota"
	if err := st.UpdateDevice(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	status := `{"Status":{"DeviceName":"Irrigation Led Ring","Topic":"plug_a"}}`
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status": status}}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Name != "Irrigation Led Ring" {
		t.Fatalf("Name = %q, want the DeviceName the device reports now", got.Name)
	}
}

// statusDeviceNameReply is the plain Status answer of #125 (stat/tasmota_568ED0/STATUS) after the
// DeviceName was changed on the device.
const statusDeviceNameReply = `{"Status":{"Module":1,"DeviceName":"Modbus Lüftung Test","FriendlyName":["Tasmota"],"Topic":"tasmota_568ED0","ButtonTopic":"0","Power":"0","PowerLock":"0","PowerOnState":3,"LedState":1,"LedMask":"FFFF","SaveData":1,"SaveState":1,"SwitchTopic":"0","ButtonRetain":0,"SwitchRetain":0,"SensorRetain":0,"PowerRetain":0,"InfoRetain":0,"StateRetain":0,"StatusRetain":0}}`

// Why (#125): a device on our broker that HTTP cannot reach is refreshed over MQTT, and that
// refresh has to bring the DeviceName along (it only read the identity before, so the name never
// followed). The plain Status is the Status object only, never Status 0. A name tasmofleet
// recorded as the device's own follows; one set by hand stays and the page offers the DeviceName.
func TestRefreshOverMQTTReadsTheDeviceName(t *testing.T) {
	p, st, id, f := mqttOnlySetup(t, model.MQTTOurs)
	f.replies[statusObject] = statusDeviceNameReply
	cur, _ := st.GetDevice(context.Background(), id)
	// Recorded at an earlier probe: the stored name is the device's own and follows.
	if err := st.UpdateProbeName(context.Background(), id, store.ProbeGuard{Address: cur.Address, Port: cur.Port, MAC: cur.MAC}, cur.Name, ""); err != nil {
		t.Fatal(err)
	}
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDevice(context.Background(), id)
	if got.Name != "Modbus Lüftung Test" || got.DeviceName != "Modbus Lüftung Test" {
		t.Fatalf("name %q device name %q", got.Name, got.DeviceName)
	}
	if got.Identity.Version != "16.0.0" || !got.Identity.DetectedAt.IsZero() {
		t.Fatalf("identity %+v: the name read must not change how the identity is stored", got.Identity)
	}

	// A hand-set name stays; the device's name is recorded for the page to offer.
	got.Name = "Lüftung Keller"
	if err := st.UpdateDevice(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	f.replies[statusObject] = strings.Replace(statusDeviceNameReply, "Modbus Lüftung Test", "Modbus Lüftung Büro", 1)
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetDevice(context.Background(), id)
	if got.Name != "Lüftung Keller" || got.DeviceName != "Modbus Lüftung Büro" {
		t.Fatalf("name %q device name %q, want the hand-set name kept and the device's recorded", got.Name, got.DeviceName)
	}
}

// Why (#125): the DeviceName is a nicety. A device that does not answer the plain Status over MQTT
// still gets its identity refreshed, and the refresh succeeds.
func TestRefreshOverMQTTWithoutAStatusAnswerStillSucceeds(t *testing.T) {
	p, st, id, _ := mqttOnlySetup(t, model.MQTTOurs) // no reply for the plain Status: a timeout
	if err := p.Refresh(context.Background(), id, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDevice(context.Background(), id); got.Identity.Version != "16.0.0" || got.DeviceName != "" {
		t.Fatalf("version %q device name %q", got.Identity.Version, got.DeviceName)
	}
}
