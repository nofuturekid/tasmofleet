package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

type fakeFactsDevices struct{ devs []model.Device }

func (f fakeFactsDevices) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	for _, d := range f.devs {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, errors.New("not found")
}
func (f fakeFactsDevices) ListDevices(context.Context) ([]model.Device, error) { return f.devs, nil }

type fakeFactsHealth map[model.DeviceID]model.Health

func (f fakeFactsHealth) Get(id model.DeviceID) model.Health { return f[id] }

type fakeMemory struct {
	kind  transport.HTTPFailKind
	since time.Time
	ok    bool
}

func (m fakeMemory) HTTPFailure(model.DeviceID) (transport.HTTPFailKind, time.Time, bool) {
	return m.kind, m.since, m.ok
}

var factsNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func newFacts(devs []model.Device, h fakeFactsHealth, mem HTTPMemory) *TransportFacts {
	return &TransportFacts{
		Devices: fakeFactsDevices{devs}, Health: h, Memory: mem,
		BusConnected: func() bool { return true }, FullTopic: func() string { return "%prefix%/%topic%/" },
		FirmwareBaseURL: func() string { return "http://fw" }, BrokerPassword: func() string { return "pw" }, Now: func() time.Time { return factsNow },
	}
}

// Why (review H1): a message to a topic another device listens on, as Topic or as GroupTopic
// (fresh or stale, own included), reaches the wrong device; Facts must say so for every shape of it.
func TestFactsTopicExclusive(t *testing.T) {
	cases := []struct {
		name string
		devs []model.Device
		want bool // for device 1
	}{
		{"alone", []model.Device{{ID: 1, Topic: "a"}}, true},
		{"others different, default group topics", []model.Device{{ID: 1, Topic: "a", GroupTopic: "tasmotas"}, {ID: 2, Topic: "b", GroupTopic: "tasmotas"}}, true},
		{"same topic", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "a"}}, false},
		{"case differs: MQTT topics are case sensitive", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "A"}}, true},
		{"B's GroupTopic is A's Topic", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "b", GroupTopic: "a"}}, false},
		{"B's stale GroupTopic is A's Topic", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "b", GroupTopic: "a", GroupTopicReadAt: time.Time{}}}, false},
		{"own GroupTopic is the Topic", []model.Device{{ID: 1, Topic: "a", GroupTopic: "a"}}, false},
		{"expanded topics collide through FullTopic", []model.Device{
			{ID: 1, Topic: "a", FullTopic: "%prefix%/%topic%/"},
			{ID: 2, Topic: "a", FullTopic: "x/%topic%/"}}, false},
		{"B's group topic expands to A's command topic", []model.Device{
			{ID: 1, Topic: "a"}, {ID: 2, Topic: "b", GroupTopic: "a", FullTopic: "%prefix%/%topic%"}}, false},
		{"trailing slash is not a difference", []model.Device{
			{ID: 1, Topic: "a", FullTopic: "%prefix%/%topic%"}, {ID: 2, Topic: "a"}}, false},
		{"FullTopic without %topic%", []model.Device{{ID: 1, Topic: "a", FullTopic: "%prefix%/room/"}}, false},
		{"FullTopic with an unexpandable placeholder", []model.Device{{ID: 1, Topic: "a", FullTopic: "%prefix%/%hostname%/%topic%/"}}, false},
		// Why (review recheck 1): unknown is not exclusive; a neighbour whose stem cannot be known
		// might listen on ours (fixed topic, %hostname%, %id%).
		{"B's FullTopic has no %topic%", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Topic: "x", FullTopic: "%prefix%/kitchen/"}}, false},
		{"B's FullTopic is only the prefix", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Topic: "", FullTopic: "%prefix%/"}}, false},
		{"B's FullTopic has %hostname%", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Topic: "b", FullTopic: "%prefix%/%hostname%/"}}, false},
		// Why (review M2): GetTopic_P collapses "//", and a stem that is a level prefix of ours overlaps.
		{"B's // collapses onto A's stem", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Topic: "chen", FullTopic: "%prefix%//kit%topic%/"}}, false},
		{"B's stem is a level prefix of A's", []model.Device{{ID: 1, Topic: "kitchen"}, {ID: 2, Topic: "k", FullTopic: "%prefix%/%topic%/%topic%/"}}, true},
		{"A's stem lies below B's", []model.Device{{ID: 1, Topic: "kitchen", FullTopic: "%prefix%/room/%topic%/"}, {ID: 2, Topic: "room", FullTopic: "%prefix%/%topic%/"}}, false},
		{"B listens on a fixed level above A", []model.Device{{ID: 1, Topic: "x", FullTopic: "%prefix%/kitchen/%topic%/"}, {ID: 2, Topic: "kitchen"}}, false},
		{"B without topic and FullTopic is HTTP only", []model.Device{{ID: 1, Topic: "a"}, {ID: 2}}, true},
		{"similar names do not overlap", []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "ab"}}, true},
		{"no topic", []model.Device{{ID: 1}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := newFacts(c.devs, fakeFactsHealth{}, nil).Facts(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if f.MQTT.TopicExclusive != c.want {
				t.Errorf("exclusive = %v", f.MQTT.TopicExclusive)
			}
		})
	}
	if _, err := newFacts(nil, fakeFactsHealth{}, nil).Facts(context.Background(), 99); err == nil {
		t.Error("unknown device must fail")
	}
}

func TestFactsHTTPState(t *testing.T) {
	d := []model.Device{{ID: 1}}
	since := factsNow.Add(-time.Hour)
	cases := []struct {
		name string
		h    model.Health
		mem  HTTPMemory
		want transport.HTTPState
	}{
		{"nothing known", model.Health{}, nil, transport.HTTPState{}},
		{"recent success", model.Health{SeenHTTP: factsNow.Add(-14 * time.Minute)}, nil, transport.HTTPState{State: transport.HTTPOK}},
		// Why: after 15 minutes without a success HTTP is unknown, not ok.
		{"old success", model.Health{SeenHTTP: factsNow.Add(-15 * time.Minute)}, nil, transport.HTTPState{}},
		{"failure remembered", model.Health{}, fakeMemory{transport.HTTPFailAuth, since, true},
			transport.HTTPState{State: transport.HTTPFailing, Kind: transport.HTTPFailAuth, Since: since}},
		// Why: a success after the failure began means the memory is stale; do not route around a working HTTP.
		{"success after failure", model.Health{SeenHTTP: factsNow.Add(-time.Minute)}, fakeMemory{transport.HTTPFailUnreachable, since, true},
			transport.HTTPState{State: transport.HTTPOK}},
		{"memory without failure", model.Health{}, fakeMemory{}, transport.HTTPState{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := newFacts(d, fakeFactsHealth{1: c.h}, c.mem).Facts(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if f.HTTP != c.want {
				t.Errorf("HTTP = %+v, want %+v", f.HTTP, c.want)
			}
		})
	}
}

// Why: Choose can only judge a GroupTopic stale against the last Status 1 read; the Status 1 stamp
// (OTAURLReadAt) and an HTTP probe's DetectedAt both are such reads, the later one counts.
func TestFactsLastProbeAndMQTT(t *testing.T) {
	stamp := factsNow.Add(-2 * time.Hour)
	det := factsNow.Add(-time.Hour)
	d := model.Device{ID: 1, Topic: "t", MQTT: model.MQTTOurs, FullTopic: "x/%topic%/", GroupTopic: "g", GroupTopicReadAt: stamp,
		OTAURLReadAt: stamp, Identity: model.Identity{DetectedAt: det}, MQTTPasswordMode: model.MQTTPasswordOwn,
		FWReach: model.FWReach{State: model.FWReachNo, At: stamp}}
	f, err := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, nil).Facts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	m := f.MQTT
	if !m.LastProbeAt.Equal(det) || !m.LWTOnline || !m.Ours || m.GroupTopic != "g" || m.FullTopic != "x/%topic%/" || m.OurFullTopic != "%prefix%/%topic%/" || !m.BusConnected {
		t.Errorf("mqtt facts: %+v", m)
	}
	if f.Password.Mode != model.MQTTPasswordOwn || f.FWReach.State != model.FWReachNo || !f.FirmwareBaseURL || !f.Now.Equal(factsNow) {
		t.Errorf("facts: %+v", f)
	}
	d.OTAURLReadAt = factsNow // Status 1 newer than the identity
	f, _ = newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Offline"}}, nil).Facts(context.Background(), 1)
	if !f.MQTT.LastProbeAt.Equal(factsNow) || f.MQTT.LWTOnline {
		t.Errorf("mqtt facts: %+v", f.MQTT)
	}
	s := newFacts([]model.Device{d}, fakeFactsHealth{}, nil)
	s.FirmwareBaseURL = func() string { return "" }
	if f, _ = s.Facts(context.Background(), 1); f.FirmwareBaseURL {
		t.Error("empty FIRMWARE_BASE_URL must read as not configured")
	}
}

// Why: the fit comes from fwselect.MQTTUploadFits with the real identity, so a layout or free-space
// problem reaches Choose as data and the upload is refused before anything is sent (D19).
func TestForUpdateFitsFromIdentity(t *testing.T) {
	d := model.Device{ID: 1, Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", FreeKB: 2000}}
	s := newFacts([]model.Device{d}, fakeFactsHealth{}, nil)
	f, err := s.ForUpdate(context.Background(), 1, UpdateRequest{Delivery: model.DeliveryPush, AllowMQTTUpload: true, ImageSize: 1 << 20,
		UpLayout: fwselect.UpLayoutUnknown, HTTPPush: transport.PushHTTP, HTTPPushNote: "n"})
	if err != nil {
		t.Fatal(err)
	}
	r := f.Request
	if !r.MQTTUploadFit.OK || r.MQTTUploadFit.NeedKB != 1024 || r.MQTTUploadFit.RoomKB != 2000-192 || r.Delivery != model.DeliveryPush || !r.AllowMQTTUpload || r.HTTPPush != transport.PushHTTP || r.HTTPPushNote != "n" {
		t.Errorf("request: %+v", r)
	}
	f, _ = s.ForUpdate(context.Background(), 1, UpdateRequest{ImageSize: 1 << 20, UpLayout: fwselect.UpLayoutSafeboot})
	if f.Request.MQTTUploadFit.OK || f.Request.MQTTUploadFit.Reason == "" {
		t.Errorf("safeboot layout must be refused with a reason: %+v", f.Request.MQTTUploadFit)
	}
	if _, err := s.ForUpdate(context.Background(), 9, UpdateRequest{}); err == nil {
		t.Error("unknown device must fail")
	}
}

// Why: broker mode sends MQTT_PASSWORD; with none configured a transfer would carry an empty
// password, so Facts must mark it unusable and Choose must say why.
func TestFactsBrokerModeWithoutBrokerPassword(t *testing.T) {
	d := model.Device{ID: 1, Topic: "t", MQTT: model.MQTTOurs, MQTTPasswordMode: model.MQTTPasswordBroker,
		GroupTopic: "g", GroupTopicReadAt: factsNow, HTTPPassword: ""}
	s := newFacts([]model.Device{d}, fakeFactsHealth{1: {LWT: "Online"}}, fakeMemory{transport.HTTPFailUnreachable, factsNow.Add(-time.Hour), true})
	f, _ := s.Facts(context.Background(), 1)
	if f.Password.Unusable != "" {
		t.Fatalf("with a broker password: %+v", f.Password)
	}
	if p := transport.Choose(f); p.Backup != transport.MQTT {
		t.Fatalf("backup = %s", p.Backup)
	}
	s.BrokerPassword = func() string { return "" }
	f, _ = s.Facts(context.Background(), 1)
	p := transport.Choose(f)
	if p.Backup != transport.None || p.Commands != transport.MQTT {
		t.Fatalf("backup=%s commands=%s", p.Backup, p.Commands)
	}
	if got := p.Reason(transport.ActBackup); !contains(got, "TasmoFleet has no broker password configured") {
		t.Errorf("reason = %q", got)
	}
	d.MQTTPasswordMode = model.MQTTPasswordOwn // own mode does not depend on the broker password
	f, _ = newFacts([]model.Device{d}, fakeFactsHealth{}, nil).Facts(context.Background(), 1)
	s2 := newFacts([]model.Device{d}, fakeFactsHealth{}, nil)
	s2.BrokerPassword = func() string { return "" }
	if f, _ = s2.Facts(context.Background(), 1); f.Password.Unusable != "" {
		t.Errorf("own mode: %+v", f.Password)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// Why (review M3, 7): devices whose GroupTopic was never read are counted (HTTP-only ones are not),
// and the bus's real command prefix is what stems are expanded with.
func TestFactsOthersGroupUnreadAndPrefix(t *testing.T) {
	devs := []model.Device{
		{ID: 1, Topic: "a", GroupTopic: "g", GroupTopicReadAt: factsNow},
		{ID: 2, Topic: "b"},                                              // never read
		{ID: 3, Topic: "c", GroupTopic: "g"},                             // not stamped
		{ID: 4, Topic: "d", GroupTopic: "g", GroupTopicReadAt: factsNow}, // read
		{ID: 5}, // HTTP only
	}
	f, err := newFacts(devs, fakeFactsHealth{}, nil).Facts(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if f.MQTT.OthersGroupUnread != 2 {
		t.Errorf("unread = %d", f.MQTT.OthersGroupUnread)
	}
	// B's stem is "cmd/a/b/<prefix>": below A's stem only when the bus's prefix is "cmd".
	devs = []model.Device{{ID: 1, Topic: "a"}, {ID: 2, Topic: "b", FullTopic: "cmd/a/%topic%"}}
	s := newFacts(devs, fakeFactsHealth{}, nil)
	if f, _ := s.Facts(context.Background(), 1); !f.MQTT.TopicExclusive {
		t.Error("default prefix cmnd: stems cmnd/a and cmd/a/b/cmnd are apart")
	}
	s.CommandPrefix = func() string { return "cmd" }
	if f, _ := s.Facts(context.Background(), 1); f.MQTT.TopicExclusive {
		t.Error("prefix cmd: stem cmd/a is a level prefix of cmd/a/b/cmd")
	}
}

// Why (security scan, sibling-gate parity): another device's GroupTopic is trusted under the same rule
// as the device's own: stamped and not older than that device's last probe. A stale foreign value
// (a probe since had no GroupTopic) must count as unread, or a write could go to a topic that device
// shares.
func TestOthersGroupUnreadUsesTheOwnFreshnessRule(t *testing.T) {
	t0 := factsNow
	me := model.Device{ID: 1, Topic: "a", GroupTopic: "tasmotas", GroupTopicReadAt: t0}
	for _, tc := range []struct {
		name  string
		other model.Device
		want  int
	}{
		{"fresh", model.Device{ID: 2, Topic: "b", GroupTopic: "tasmotas", GroupTopicReadAt: t0}, 0},
		{"never stamped", model.Device{ID: 2, Topic: "b", GroupTopic: "tasmotas"}, 1},
		{"empty", model.Device{ID: 2, Topic: "b", GroupTopicReadAt: t0}, 1},
		{"older than its last Status 1", model.Device{ID: 2, Topic: "b", GroupTopic: "tasmotas", GroupTopicReadAt: t0.Add(-time.Hour), OTAURLReadAt: t0}, 1},
		{"older than its identity detection", model.Device{ID: 2, Topic: "b", GroupTopic: "tasmotas", GroupTopicReadAt: t0.Add(-time.Hour), Identity: model.Identity{DetectedAt: t0}}, 1},
		{"no topic: not in play", model.Device{ID: 2, GroupTopicReadAt: t0.Add(-time.Hour), OTAURLReadAt: t0}, 0},
	} {
		f, err := newFacts([]model.Device{me, tc.other}, nil, nil).Facts(t.Context(), 1)
		if err != nil || f.MQTT.OthersGroupUnread != tc.want {
			t.Errorf("%s: unread = %d, want %d (%v)", tc.name, f.MQTT.OthersGroupUnread, tc.want, err)
		}
	}
}

// Why: a device whose last will says Offline cannot receive a command (ours are never retained), so one
// dead device must not block MQTT writes for the fleet. An unknown will still counts, and the dead
// device's topic still collides in TopicExclusive.
func TestOthersGroupUnreadSkipsBrokerConfirmedOfflineDevices(t *testing.T) {
	me := model.Device{ID: 1, Topic: "a", GroupTopic: "tasmotas", GroupTopicReadAt: factsNow}
	other := model.Device{ID: 2, Topic: "b", GroupTopic: "a"} // unread (never stamped) and its group is a's topic
	for _, tc := range []struct {
		lwt  string
		want int
	}{{"Offline", 0}, {"", 1}, {"Online", 1}} {
		f, err := newFacts([]model.Device{me, other}, fakeFactsHealth{2: {LWT: tc.lwt}}, nil).Facts(t.Context(), 1)
		if err != nil || f.MQTT.OthersGroupUnread != tc.want {
			t.Errorf("LWT %q: unread = %d, want %d (%v)", tc.lwt, f.MQTT.OthersGroupUnread, tc.want, err)
		}
		if f.MQTT.TopicExclusive {
			t.Errorf("LWT %q: the offline device's GroupTopic is a's topic; exclusivity must stay false", tc.lwt)
		}
	}
}
