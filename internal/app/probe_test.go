package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

func probeFixture(t *testing.T, spec simulator.DeviceSpec) (*Prober, model.Device) {
	t.Helper()
	fleet, err := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fleet.Close() })
	dev, err := fleet.Add(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	host, port := splitAddr(t, dev.Addr())
	client := tasmota.NewHTTPClient(tasmota.Options{AllowedNetworks: loopbackNets()})
	d := model.Device{Name: "", Address: host, Port: port, HTTPUser: spec.User, HTTPPassword: spec.Password}
	return &Prober{Client: client, Gap: time.Millisecond}, d
}

// Why: adding a device must fill MAC, topic and identity from the device so
// that OTA preflight and health can rely on them, and must never send Status 0.
func TestDescribeFillsIdentityWithoutStatus0(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_4M})
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.MAC == "" || got.Topic == "" || got.Name == "" || got.Identity.Family != model.ChipESP8266 || got.Identity.Version == "" {
		t.Fatalf("incomplete: %+v", got)
	}
	if got.MQTT == model.MQTTUnknown {
		t.Fatal("MQTT capability must be probed")
	}
}

// Why: minimal firmware only answers a few commands; the probe has to take
// version and family from Upgrade and the main page instead of failing.
func TestDescribeMinimalFirmware(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "m", Profile: simulator.ProfileESP8266Min})
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.Variant != "minimal" || got.Identity.Family != model.ChipESP8266 {
		t.Fatalf("identity: %+v", got.Identity)
	}
}

type fixedBroker struct{ host string }

func (b fixedBroker) Same(host string, port int) bool { return host == b.host && port == 1883 }

// override answers selected commands itself and forwards the rest.
type override struct {
	tasmota.Client
	reply map[string]string
}

func (o override) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	if r, ok := o.reply[c]; ok {
		return json.RawMessage(r), nil
	}
	return o.Client.Command(ctx, t, c)
}

// Why: "another broker" was shown for every device when tasmofleet had no
// broker at all. The probe must record where the device reports to and
// classify it relative to OUR broker.
func TestDescribeMQTTClassification(t *testing.T) {
	const status6 = `{"StatusMQT":{"MqttHost":"mqtt.example.internal","MqttPort":1883,"MqttUser":"secret-user"}}`
	cases := []struct {
		name     string
		broker   brokerMatcher
		reply    map[string]string
		wantCap  model.MQTTCapability
		wantHost string
	}{
		{"no tasmofleet broker", nil, nil, model.MQTTOtherBroker, "mqtt.example.internal"},
		{"same broker", fixedBroker{"mqtt.example.internal"}, nil, model.MQTTOurs, "mqtt.example.internal"},
		{"different broker", fixedBroker{"other.lan"}, nil, model.MQTTOtherBroker, "mqtt.example.internal"},
		{"empty MqttHost", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": `{"StatusMQT":{"MqttHost":"","MqttPort":1883}}`}, model.MQTTNone, ""},
		{"SetOption3 off", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": status6, "SetOption3": `{"SetOption3":"OFF"}`}, model.MQTTNone, "mqtt.example.internal"},
		{"SetOption3 on", fixedBroker{"mqtt.example.internal"}, map[string]string{"Status 6": status6, "SetOption3": `{"SetOption3":"ON"}`}, model.MQTTOurs, "mqtt.example.internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
			p.Client = override{Client: p.Client, reply: tc.reply}
			p.Broker = tc.broker
			got, err := p.Describe(context.Background(), d)
			if err != nil {
				t.Fatal(err)
			}
			if got.MQTT != tc.wantCap || got.MQTTHost != tc.wantHost {
				t.Fatalf("MQTT = %q host %q, want %q host %q", got.MQTT, got.MQTTHost, tc.wantCap, tc.wantHost)
			}
			if tc.wantHost != "" && got.MQTTPort != 1883 {
				t.Fatalf("port = %d", got.MQTTPort)
			}
			if b, _ := json.Marshal(got); strings.Contains(string(b), "secret-user") {
				t.Fatal("the device's MQTT user leaked into the device record")
			}
		})
	}
}

const (
	status5Net  = `{"StatusNET":{"Hostname":"plug-1","IPAddress":"192.0.2.10","Gateway":"192.0.2.1","Mac":"AA:BB:CC:00:00:01"}}`
	status11Net = `{"StatusSTS":{"Wifi":{"SSId":"IoT","BSSId":"02:00:5E:00:00:01","Channel":11,"RSSI":50}}}`
)

// recorder remembers every command, to prove the probe never sends Status 0.
type recorder struct {
	override
	sent *[]string
}

func (r recorder) Command(ctx context.Context, t tasmota.Target, c string) (json.RawMessage, error) {
	*r.sent = append(*r.sent, c)
	return r.override.Command(ctx, t, c)
}

// Why: the device page shows hostname, IP, gateway and Wi-Fi; they only exist on the device.
// A probe (first contact after import, "Refresh identity") reads Status 5 and Status 11, one
// command at a time and never Status 0, and hands the facts to the caller.
func TestDescribeReadsNetworkFactsWithoutStatus0(t *testing.T) {
	p, d := probeFixture(t, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M})
	var sent []string
	p.Client = recorder{override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status 11": status11Net}}, &sent}
	p.Now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	got, err := p.Describe(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	want := model.NetFacts{Hostname: "plug-1", IP: "192.0.2.10", Gateway: "192.0.2.1", SSID: "IoT", BSSID: "02:00:5E:00:00:01", Channel: 11, ReadAt: p.Now()}
	if got.Net != want {
		t.Fatalf("Net = %+v, want %+v", got.Net, want)
	}
	for _, c := range sent {
		if c == "Status 0" || c == "Status" {
			t.Fatalf("Status 0 was sent: %v", sent)
		}
	}
}

// Why: Status 11 is a nice-to-have. When it fails the probe must still succeed, and the Wi-Fi
// facts read earlier stay (a wired board or a flaky reply must not blank them).
func TestRefreshKeepsEarlierWifiFactsWhenStatus11Fails(t *testing.T) {
	p, st, id, _, _ := raceSetup(t)
	p.Client = override{Client: p.Client, reply: map[string]string{"Status 5": status5Net, "Status 11": status11Net}}
	if err := p.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	p.Client = override{Client: p.Client.(override).Client, reply: map[string]string{"Status 5": status5Net, "Status 11": `{"Command":"Error"}`}}
	if err := p.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDevice(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Net.Hostname != "plug-1" || d.Net.Gateway != "192.0.2.1" || d.Net.SSID != "IoT" || d.Net.Channel != 11 {
		t.Fatalf("facts after a Status 11 failure: %+v", d.Net)
	}
}
