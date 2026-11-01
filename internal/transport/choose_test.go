package transport

import (
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

var t0 = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// base is a device HTTP reaches, that is also fully usable over MQTT with a set password and a
// fresh GroupTopic. Each test breaks one fact.
func base() Facts {
	return Facts{
		Now:             t0,
		HTTP:            HTTPState{State: HTTPOK},
		FirmwareBaseURL: true,
		MQTT: MQTTFacts{
			BusConnected: true, Ours: true, Topic: "kitchen", TopicExclusive: true,
			GroupTopic: "tasmotas", GroupTopicReadAt: t0.Add(-time.Hour), LastProbeAt: t0.Add(-time.Hour),
			LWTOnline: true,
		},
		Password: Password{Mode: model.MQTTPasswordOwn},
	}
}

// httpDown makes HTTP fail the way an http_unreachable device does (#45).
func httpDown(f Facts) Facts {
	f.HTTP = HTTPState{State: HTTPFailing, Kind: HTTPFailUnreachable, Since: t0.Add(-time.Hour)}
	return f
}

func push(f Facts, httpPush Path, allow bool) Facts {
	f.Request = Request{Delivery: model.DeliveryPush, AllowMQTTUpload: allow, HTTPPush: httpPush,
		MQTTUploadFit: MQTTFit{OK: true, NeedKB: 1900, RoomKB: 2500}}
	return f
}

func TestChooseOrder(t *testing.T) {
	noPW := func(f Facts) Facts { f.Password = Password{}; return f }
	badPW := func(f Facts) Facts { f.Password.BadAt = t0.Add(-time.Minute); return f }
	fwNo := func(f Facts) Facts {
		f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
		return f
	}
	noBase := func(f Facts) Facts { f.FirmwareBaseURL = false; return f }
	mqttOff := func(f Facts) Facts { f.MQTT.BusConnected = false; return f }
	cases := []struct {
		name                           string
		f                              Facts
		commands, backup, restore, upd Path
	}{
		// Why: HTTP is the established path with synchronous replies; nothing changes for devices
		// HTTP reaches, even when MQTT would work too.
		{"http ok, mqtt usable: HTTP everywhere", base(), HTTP, HTTP, HTTP, DownloadHTTP},
		{"http unknown counts as not failing", func() Facts { f := base(); f.HTTP = HTTPState{}; return f }(), HTTP, HTTP, HTTP, DownloadHTTP},
		// Why: this is #45. MQTT is what makes an http_unreachable device manageable.
		{"http failing, mqtt usable, password set", httpDown(base()), MQTT, MQTT, MQTT, DownloadMQTT},
		// Why: a transfer carries the password; without one nothing is published. Commands do not need it.
		{"http failing, no password: transfers off, commands on", noPW(httpDown(base())), MQTT, None, None, DownloadMQTT},
		// Why: a refused password (Error 1) blocks transfers until changed, not commands.
		{"http failing, password refused", badPW(httpDown(base())), MQTT, None, None, DownloadMQTT},
		// Why: no MQTT, no HTTP: nothing to do, and the reason must say so.
		{"http failing, bus down", mqttOff(httpDown(base())), None, None, None, None},
		// Why: a device that cannot fetch from the firmware server cannot download an update. A
		// restore over HTTP is pushed to the device (#430), so it stays on HTTP, like the backup.
		{"http ok, device does not reach firmware server", fwNo(base()), HTTP, HTTP, HTTP, None},
		// Why: without FIRMWARE_BASE_URL no device can fetch anything (D12); a restore needs nothing
		// from the firmware server (#430).
		{"http ok, no FIRMWARE_BASE_URL", noBase(base()), HTTP, HTTP, HTTP, None},
		{"http ok, no base URL, no password", noPW(noBase(base())), HTTP, HTTP, HTTP, None},
		// Why: with HTTP failing and no firmware server there is no download; update is None.
		{"http failing, fw no", fwNo(httpDown(base())), MQTT, MQTT, MQTT, None},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Choose(c.f)
			if p.Commands != c.commands || p.Backup != c.backup || p.Restore != c.restore || p.Update != c.upd {
				t.Fatalf("got commands=%s backup=%s restore=%s update=%s\nreasons: %v", p.Commands, p.Backup, p.Restore, p.Update, p.Reasons)
			}
			if len(p.Reasons) != 5 {
				t.Fatalf("want one reason per action, got %d", len(p.Reasons))
			}
			for _, r := range p.Reasons {
				if strings.TrimSpace(r.Text) == "" {
					t.Errorf("empty reason for %s", r.Action)
				}
			}
		})
	}
}

// Why: every Path value must be explainable in the UI; a Path without a sentence would render blank.
func TestEveryPathHasADescription(t *testing.T) {
	for _, p := range AllPaths {
		if p.Describe() == "" {
			t.Errorf("%q has no description", p)
		}
	}
	if len(AllPaths) != 8 {
		t.Errorf("AllPaths lists %d values; update the test when a Path is added", len(AllPaths))
	}
}

// Why: a person reads these sentences; the exact text of the ones that tell them what to do is
// part of the contract (#261 acceptance criteria).
func TestReasonTexts(t *testing.T) {
	f := httpDown(base())
	f.Password = Password{}
	p := Choose(f)
	if got := p.Reason(ActBackup); !strings.Contains(got, "enter the device's MQTT password to back up over MQTT") {
		t.Errorf("backup reason = %q", got)
	}
	if got := p.Reason(ActRestore); !strings.Contains(got, "enter the device's MQTT password to restore over MQTT") {
		t.Errorf("restore reason = %q", got)
	}
	f.Password.BadAt = t0.Add(-time.Hour)
	f.Password.Mode = model.MQTTPasswordOwn
	if got := Choose(f).Reason(ActBackup); !strings.Contains(got, "refused its MQTT password") {
		t.Errorf("bad password reason = %q", got)
	}
	if got := Choose(base()).Reason(ActCommands); !strings.Contains(got, "HTTP") {
		t.Errorf("commands reason = %q", got)
	}
	for kind, want := range map[HTTPFailKind]string{
		HTTPFailUnreachable: "cannot reach the device over HTTP",
		HTTPFailOutside:     "DEVICE_NETWORKS",
		HTTPFailAPIRefused:  "SetOption128",
		HTTPFailAuth:        "web password",
	} {
		f := httpDown(base())
		f.HTTP.Kind = kind
		f.MQTT.BusConnected = false
		if got := Choose(f).Reason(ActCommands); !strings.Contains(got, want) {
			t.Errorf("kind %s: reason %q lacks %q", kind, got, want)
		}
	}
}

// Why: a Path.Reason lookup for an action that has none must not invent a sentence.
func TestReasonUnknownAction(t *testing.T) {
	if got := Choose(base()).Reason(Action("nope")); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestMQTTUsability(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*Facts)
		want string // substring of the Commands reason when MQTT is rejected
	}{
		{"bus down", func(f *Facts) { f.MQTT.BusConnected = false }, "not connected to its MQTT broker"},
		// Why: a device on another broker never sees our messages.
		{"other broker", func(f *Facts) { f.MQTT.Ours = false }, "does not use TasmoFleet's MQTT broker"},
		{"no topic", func(f *Facts) { f.MQTT.Topic = "" }, "no MQTT topic"},
		// Why: a message to a shared topic reaches two devices; a transfer would hit the wrong one.
		{"shared topic", func(f *Facts) { f.MQTT.TopicExclusive = false }, "another device listens on this topic"},
		// Why: we publish under our FullTopic; the device listens under its own.
		{"foreign full topic", func(f *Facts) { f.MQTT.FullTopic = "x/%topic%/%prefix%/" }, "FullTopic"},
		{"our full topic differs, device default", func(f *Facts) { f.MQTT.OurFullTopic = "fleet/%prefix%/%topic%/" }, "FullTopic"},
		// Why: a group topic equal to the topic means every command reaches the whole group.
		{"topic is group topic", func(f *Facts) { f.MQTT.GroupTopic = f.MQTT.Topic }, "GroupTopic"},
		// Why: without LWT Online the device may not be there; a publish would vanish (nothing sent).
		{"lwt not online", func(f *Facts) { f.MQTT.LWTOnline = false }, "not online on MQTT"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := httpDown(base())
			c.mod(&f)
			p := Choose(f)
			if p.Commands != None || p.Backup != None || p.Restore != None || p.Update != None {
				t.Fatalf("MQTT unusable but got %+v", p)
			}
			if got := p.Reason(ActCommands); !strings.Contains(got, c.want) {
				t.Errorf("reason %q lacks %q", got, c.want)
			}
		})
	}
	t.Run("equal full topic and explicit default are fine", func(t *testing.T) {
		f := httpDown(base())
		f.MQTT.FullTopic, f.MQTT.OurFullTopic = "%prefix%/%topic%/", ""
		if Choose(f).Commands != MQTT {
			t.Fatal("explicit default FullTopic must equal the empty one")
		}
		f.MQTT.FullTopic, f.MQTT.OurFullTopic = "fleet/%prefix%/%topic%/", "fleet/%prefix%/%topic%/"
		if Choose(f).Commands != MQTT {
			t.Fatal("equal custom FullTopic must pass")
		}
	})
}

// Why (review #264 L2): the GroupTopic is stale if INFO1/Status 1 did not carry it; a stale value
// must not vouch that the topic is unshared. Commands are lenient (the channel refuses known
// group topics itself), every other MQTT action needs a fresh value.
func TestGroupTopicStaleness(t *testing.T) {
	cases := []struct {
		name    string
		mod     func(*MQTTFacts)
		known   bool
		disqual bool // known and equal to the topic
	}{
		{"fresh, different", func(m *MQTTFacts) {}, true, false},
		{"empty", func(m *MQTTFacts) { m.GroupTopic = "" }, false, false},
		{"never stamped", func(m *MQTTFacts) { m.GroupTopicReadAt = time.Time{} }, false, false},
		{"older than last probe", func(m *MQTTFacts) { m.LastProbeAt = m.GroupTopicReadAt.Add(time.Second) }, false, false},
		{"same instant as probe", func(m *MQTTFacts) { m.LastProbeAt = m.GroupTopicReadAt }, true, false},
		{"fresh and equal", func(m *MQTTFacts) { m.GroupTopic = m.Topic }, true, true},
		// Why: a stale value equal to the topic still adds a refusal; staleness only ever removes trust.
		{"stale and equal", func(m *MQTTFacts) { m.GroupTopic = m.Topic; m.LastProbeAt = m.GroupTopicReadAt.Add(time.Second) }, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := httpDown(base())
			c.mod(&f.MQTT)
			if got := groupTopicKnown(f.MQTT); got != c.known {
				t.Fatalf("known = %v", got)
			}
			p := Choose(f)
			wantCmd := MQTT
			if c.disqual {
				wantCmd = None
			}
			if p.Commands != wantCmd {
				t.Errorf("commands = %s", p.Commands)
			}
			// writes follow the transfers' strict rule
			wantWrites := MQTT
			if !c.known || c.disqual {
				wantWrites = None
			}
			if p.Writes != wantWrites {
				t.Errorf("writes = %s, want %s", p.Writes, wantWrites)
			}
			wantXfer := MQTT
			if !c.known || c.disqual {
				wantXfer = None
			}
			if p.Backup != wantXfer || p.Restore != wantXfer {
				t.Errorf("transfers = %s/%s, want %s", p.Backup, p.Restore, wantXfer)
			}
			if !c.known && !c.disqual && p.Backup == None && !strings.Contains(p.Reason(ActBackup), "GroupTopic") {
				t.Errorf("reason does not mention GroupTopic: %q", p.Reason(ActBackup))
			} else if p.Backup == None && !c.known && !c.disqual && !strings.Contains(p.Reason(ActBackup), "refresh the device's identity") {
				t.Errorf("reason does not say how to fix it: %q", p.Reason(ActBackup))
			}
		})
	}
}

// Why (D11): "no" is a snapshot of a network that can change; after 24 h the device is asked
// again. "yes" never expires.
func TestFWReachExpiry(t *testing.T) {
	for _, c := range []struct {
		name string
		r    model.FWReach
		want Path
	}{
		{"no, fresh", model.FWReach{State: model.FWReachNo, At: t0.Add(-23 * time.Hour)}, None},
		{"no, exactly 24h", model.FWReach{State: model.FWReachNo, At: t0.Add(-24 * time.Hour)}, None},
		{"no, older than 24h counts as unknown", model.FWReach{State: model.FWReachNo, At: t0.Add(-24*time.Hour - time.Second)}, DownloadHTTP},
		{"yes, old", model.FWReach{State: model.FWReachYes, At: t0.Add(-72 * time.Hour)}, DownloadHTTP},
		{"unknown", model.FWReach{}, DownloadHTTP},
		// Why: a "no" that cannot be dated cannot expire, so it is not trusted (review L1).
		{"no without a time is unknown", model.FWReach{State: model.FWReachNo}, DownloadHTTP},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := base()
			f.FWReach = c.r
			if got := Choose(f).Update; got != c.want {
				t.Errorf("update = %s, want %s", got, c.want)
			}
		})
	}
	f := base()
	f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
	if got := Choose(f).Reason(ActUpdate); !strings.Contains(got, "does not reach TasmoFleet's firmware server") || !strings.Contains(got, "2026-10-30 11:00 UTC") {
		t.Errorf("reason = %q", got)
	}
}

// Why: HTTP-reachable push devices keep today's decision from the OTA engine; the real-device
// tested paths of 0.18/0.19 must not regress, even with MQTT usable and the checkbox ticked.
func TestUpdatePushKeepsHTTPDecision(t *testing.T) {
	for _, hp := range []Path{PushHTTP, PushSafeboot} {
		f := push(base(), hp, true)
		if got := Choose(f).Update; got != hp {
			t.Errorf("HTTPPush %s: got %s", hp, got)
		}
	}
	// The per-device fallback to a download keeps its note.
	f := push(base(), DownloadHTTP, true)
	f.Request.HTTPPushNote = "not enough free flash for an upload"
	p := Choose(f)
	if p.Update != DownloadHTTP || !strings.Contains(p.Reason(ActUpdate), "not enough free flash for an upload") {
		t.Errorf("fallback: %s %q", p.Update, p.Reason(ActUpdate))
	}
	// Zero HTTPPush reads as the fallback.
	f = push(base(), "", false)
	if got := Choose(f).Update; got != DownloadHTTP {
		t.Errorf("zero HTTPPush: %s", got)
	}
	// A fallback to download for a device that cannot fetch is no path at all.
	f = push(base(), DownloadHTTP, true)
	f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
	if got := Choose(f).Update; got != None {
		t.Errorf("fw no: %s", got)
	}
	// Pull never picks a push path, whatever the engine suggests.
	f = base()
	f.Request.HTTPPush = PushHTTP
	if got := Choose(f).Update; got != DownloadHTTP {
		t.Errorf("pull with PushHTTP hint: %s", got)
	}
}

func TestUpdatePushOverMQTT(t *testing.T) {
	t.Run("all conditions: PushMQTT", func(t *testing.T) {
		p := Choose(push(httpDown(base()), PushHTTP, true))
		if p.Update != PushMQTT {
			t.Fatalf("got %s: %s", p.Update, p.Reason(ActUpdate))
		}
	})
	// Why: experimental stays opt-in; without the checkbox the upload is never chosen, and the
	// download over MQTT remains.
	t.Run("no checkbox: DownloadMQTT", func(t *testing.T) {
		p := Choose(push(httpDown(base()), PushHTTP, false))
		if p.Update != DownloadMQTT || !strings.Contains(p.Reason(ActUpdate), "not allowed") {
			t.Fatalf("got %s %q", p.Update, p.Reason(ActUpdate))
		}
	})
	// Why: never try-and-see; the fit is decided before anything is sent (D19).
	t.Run("does not fit: DownloadMQTT", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, true)
		f.Request.MQTTUploadFit = MQTTFit{NeedKB: 1900, RoomKB: 1000, Reason: "the image does not fit"}
		p := Choose(f)
		if p.Update != DownloadMQTT || !strings.Contains(p.Reason(ActUpdate), "does not fit") {
			t.Fatalf("got %s %q", p.Update, p.Reason(ActUpdate))
		}
		f.Request.MQTTUploadFit.Reason = ""
		if r := Choose(f).Reason(ActUpdate); !strings.Contains(r, "needs 1900 KB, 1000 KB room") {
			t.Errorf("fallback fit text: %q", r)
		}
	})
	// Why: the upload carries the password; no usable password, no upload.
	t.Run("no password: DownloadMQTT", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, true)
		f.Password = Password{}
		if p := Choose(f); p.Update != DownloadMQTT {
			t.Fatalf("got %s", p.Update)
		}
		f = push(httpDown(base()), PushHTTP, true)
		f.Password.BadAt = t0
		if p := Choose(f); p.Update != DownloadMQTT {
			t.Fatalf("bad password: got %s", p.Update)
		}
	})
	// Why: the download needs the device to reach the firmware server; with "no" the only way left
	// is the upload, and without it there is none.
	t.Run("fw no and upload refused: None", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, false)
		f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
		if p := Choose(f); p.Update != None {
			t.Fatalf("got %s", p.Update)
		}
	})
	t.Run("fw no, upload allowed: PushMQTT", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, true)
		f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
		if p := Choose(f); p.Update != PushMQTT {
			t.Fatalf("got %s", p.Update)
		}
	})
	// Why: nothing can be sent over an unusable MQTT, checkbox or not.
	t.Run("mqtt unusable: None", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, true)
		f.MQTT.LWTOnline = false
		f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
		p := Choose(f)
		if p.Update != None {
			t.Fatalf("got %s", p.Update)
		}
		r := p.Reason(ActUpdate)
		for _, want := range []string{"cannot reach the device over HTTP", "firmware server", "MQTT is not usable"} {
			if !strings.Contains(r, want) {
				t.Errorf("reason %q lacks %q", r, want)
			}
		}
	})
	// Why: a stale GroupTopic could hide a shared topic; a firmware upload there would flash the group.
	t.Run("stale GroupTopic blocks the upload", func(t *testing.T) {
		f := push(httpDown(base()), PushHTTP, true)
		f.MQTT.GroupTopic = ""
		if p := Choose(f); p.Update != None && p.Update != DownloadMQTT {
			t.Fatalf("got %s", p.Update)
		} else if p.Update != None {
			t.Fatalf("download over MQTT must also wait for a known GroupTopic, got %s", p.Update)
		}
	})
}

// Why: a pull request that ends in None tells the person how the experimental upload would
// rescue it, but only when it really would.
func TestNoneHintsAtMQTTUpload(t *testing.T) {
	const hint = "choose Push where possible and allow upload over MQTT (experimental)"
	f := httpDown(base())
	f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
	f.Request.MQTTUploadFit = MQTTFit{OK: true}
	p := Choose(f)
	if p.Update != None || !strings.HasSuffix(p.Reason(ActUpdate), hint) {
		t.Fatalf("pull: %s %q", p.Update, p.Reason(ActUpdate))
	}
	// push without the checkbox says the same
	g := push(f, PushHTTP, false)
	if r := Choose(g).Reason(ActUpdate); !strings.HasSuffix(r, hint) {
		t.Errorf("push without checkbox: %q", r)
	}
	// does not fit: no hint, it would not help
	f.Request.MQTTUploadFit = MQTTFit{OK: false, NeedKB: 3000, RoomKB: 1000}
	if r := Choose(f).Reason(ActUpdate); strings.Contains(r, hint) {
		t.Errorf("hint although the image does not fit: %q", r)
	}
	// no password: no hint
	f.Request.MQTTUploadFit = MQTTFit{OK: true}
	f.Password = Password{}
	if r := Choose(f).Reason(ActUpdate); strings.Contains(r, hint) {
		t.Errorf("hint without password: %q", r)
	}
	// with the checkbox ticked and the upload possible there is no None at all
	if p := Choose(push(httpDown(base()), PushHTTP, true)); p.Update != PushMQTT {
		t.Fatalf("got %s", p.Update)
	}
}

// Why: the spec's example sentence for the case where nothing works.
func TestNoPathSentence(t *testing.T) {
	f := push(httpDown(base()), PushHTTP, true)
	f.FWReach = model.FWReach{State: model.FWReachNo, At: t0.Add(-time.Hour)}
	f.Request.MQTTUploadFit = MQTTFit{NeedKB: 1900, RoomKB: 1000}
	got := Choose(f).Reason(ActUpdate)
	if !strings.HasPrefix(got, "no path: ") || !strings.Contains(got, "needs 1900 KB, 1000 KB room") {
		t.Errorf("got %q", got)
	}
}

// Why: a password that is unusable for a reason of its own (no broker password) must show that reason.
func TestPasswordUnusableReason(t *testing.T) {
	f := httpDown(base())
	f.Password = Password{Mode: model.MQTTPasswordBroker, Unusable: "TasmoFleet has no broker password configured"}
	p := Choose(f)
	if p.Backup != None || p.Commands != MQTT || !strings.Contains(p.Reason(ActBackup), "no broker password") {
		t.Fatalf("%s %s %q", p.Backup, p.Commands, p.Reason(ActBackup))
	}
}

// Why (review H1): a topic that another device listens on, as its Topic or its GroupTopic (fresh or
// stale), makes every MQTT action reach the wrong device. Reads, writes and transfers all refuse.
func TestTopicExclusivityBlocksEveryAction(t *testing.T) {
	f := httpDown(base())
	f.MQTT.TopicExclusive = false
	p := Choose(f)
	for a, path := range map[Action]Path{ActCommands: p.Commands, ActWrites: p.Writes, ActBackup: p.Backup, ActRestore: p.Restore, ActUpdate: p.Update} {
		if path != None {
			t.Errorf("%s = %s", a, path)
		}
		if !strings.Contains(p.Reason(a), "another device listens on this topic (its Topic or GroupTopic)") {
			t.Errorf("%s reason %q", a, p.Reason(a))
		}
	}
}

// Why (review H1): the default group topic is shared by every fresh Tasmota; and a topic with
// wildcards, levels or whitespace is not one device's topic. This mirrors mqttcmd's checkTopic,
// which stays the second guard.
func TestTopicShape(t *testing.T) {
	for topic, ok := range map[string]bool{
		"kitchen": true, "kitchen_1": true, "Kitchen": true,
		"": false, "tasmotas": false, "a/b": false, "a+": false, "#": false, "+": false,
		"a b": false, " a": false, "a ": false, "a\tb": false, "a\nb": false,
	} {
		f := httpDown(base())
		f.MQTT.Topic = topic
		f.MQTT.GroupTopic = "grp"
		got := Choose(f).Commands == MQTT
		if got != ok {
			t.Errorf("topic %q: usable = %v, want %v", topic, got, ok)
		}
		if TopicShapeOK(topic) != (ok || topic == "tasmotas") {
			t.Errorf("TopicShapeOK(%q)", topic)
		}
	}
}

// Why (review M1): whatever Choose lets onto MQTT must also pass mqttcmd.checkTopic (non-empty, no
// '/+#', no surrounding space, not tasmotas, not the device's own GroupTopic), or the channel would
// refuse what the UI promised. Mirrors checkTopic; the reverse (checkTopic stricter than Choose)
// is what this guards against.
func TestChooseMQTTImpliesChannelTopicCheck(t *testing.T) {
	channelOK := func(topic, group string) bool {
		return topic != "" && !strings.ContainsAny(topic, "/+#") && strings.TrimSpace(topic) == topic &&
			topic != "tasmotas" && (group == "" || topic != group)
	}
	topics := []string{"", "k", "tasmotas", "a/b", "a+", "#", " a", "a ", "a b", "g"}
	groups := []string{"", "g", "k", "tasmotas"}
	for _, topic := range topics {
		for _, group := range groups {
			for _, fresh := range []bool{true, false} {
				f := httpDown(base())
				f.MQTT.Topic, f.MQTT.GroupTopic = topic, group
				if !fresh {
					f.MQTT.GroupTopicReadAt = time.Time{}
				}
				p := Choose(f)
				for a, path := range map[Action]Path{ActCommands: p.Commands, ActWrites: p.Writes, ActBackup: p.Backup, ActRestore: p.Restore, ActUpdate: p.Update} {
					if (path == MQTT || path == DownloadMQTT || path == PushMQTT) && !channelOK(topic, group) {
						t.Errorf("topic %q group %q fresh=%v: %s = %s but the channel refuses it", topic, group, fresh, a, path)
					}
				}
			}
		}
	}
}

// Why (review H1): writes (settings, Make compliant, console) hit the device like a transfer; an
// unknown own GroupTopic must not let them through, while reads (Status N) may pass.
func TestWritesNeedKnownGroupTopicReadsDoNot(t *testing.T) {
	f := httpDown(base())
	f.MQTT.GroupTopic = ""
	p := Choose(f)
	if p.Commands != MQTT || p.Writes != None {
		t.Fatalf("commands=%s writes=%s", p.Commands, p.Writes)
	}
	if !strings.Contains(p.Reason(ActWrites), "refresh the device's identity") {
		t.Errorf("writes reason %q", p.Reason(ActWrites))
	}
	if Choose(base()).Writes != HTTP {
		t.Error("writes use HTTP while it works")
	}
}

// Why (review L3, L4): a trailing '/' does not make FullTopics differ; an unrecognised password
// mode is not a password.
func TestFullTopicTrailingSlashAndPasswordMode(t *testing.T) {
	f := httpDown(base())
	f.MQTT.FullTopic, f.MQTT.OurFullTopic = "%prefix%/%topic%", "%prefix%/%topic%/"
	if Choose(f).Commands != MQTT {
		t.Error("trailing slash must not matter")
	}
	f = httpDown(base())
	f.Password.Mode = model.MQTTPasswordMode("weird")
	p := Choose(f)
	if p.Backup != None || p.Commands != MQTT || !strings.Contains(p.Reason(ActBackup), "not recognised") {
		t.Errorf("backup=%s %q", p.Backup, p.Reason(ActBackup))
	}
	f.Password.Mode = model.MQTTPasswordBroker
	if Choose(f).Backup != MQTT {
		t.Error("broker mode is a password")
	}
}

// Why (review M2): the stem is what the device subscribes to, expanded as Tasmota's GetTopic_P does:
// "//" collapses, a FullTopic without %prefix% gets "/%prefix%", the prefix is the bus's real one.
func TestCommandTopic(t *testing.T) {
	for _, c := range []struct {
		full, topic, prefix, want string
		ok                        bool
	}{
		{"", "k", "cmnd", "cmnd/k", true},
		{"%prefix%/%topic%/", "k", "cmnd", "cmnd/k", true},
		{"%prefix%/%topic%", "k", "cmnd", "cmnd/k", true},
		{"%prefix%/%topic%/", "k", "cmd", "cmd/k", true}, // the bus's own prefix
		{"fleet/%topic%/%prefix%/", "k", "cmnd", "fleet/k/cmnd", true},
		{"%prefix%//kit%topic%/", "chen", "cmnd", "cmnd/kitchen", true}, // "//" collapses
		{"%topic%", "k", "cmnd", "k/cmnd", true},                        // %prefix% appended
		{"%prefix%/room", "k", "cmnd", "", false},                       // no %topic%: every device shares it
		{"%prefix%/", "k", "cmnd", "", false},
		{"%prefix%/%hostname%/%topic%/", "k", "cmnd", "", false}, // cannot expand
		{"%prefix%/%id%/", "k", "cmnd", "", false},
	} {
		got, ok := CommandTopic(c.full, c.topic, c.prefix)
		if got != c.want || ok != c.ok {
			t.Errorf("CommandTopic(%q,%q,%q) = %q,%v", c.full, c.topic, c.prefix, got, ok)
		}
	}
}

// Why (review M2): Tasmota subscribes to stem + "/#", so a stem that is a level prefix of another
// overlaps with it; "a" and "ab" do not.
func TestStemsOverlap(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"cmnd/a", "cmnd/a", true},
		{"cmnd/a", "cmnd/a/b", true},
		{"cmnd/a/b", "cmnd/a", true},
		{"cmnd/a", "cmnd/ab", false},
		{"cmnd/ab", "cmnd/a", false},
		{"cmnd/a", "cmnd/b", false},
	} {
		if got := StemsOverlap(c.a, c.b); got != c.want {
			t.Errorf("StemsOverlap(%q,%q) = %v", c.a, c.b, got)
		}
	}
}

// Why (review M3): another device with an unread GroupTopic could be in a group this device's topic
// belongs to. Anything that is not a read refuses; reads go on.
func TestOthersGroupUnread(t *testing.T) {
	f := httpDown(base())
	f.MQTT.OthersGroupUnread = 2
	p := Choose(f)
	if p.Commands != MQTT {
		t.Errorf("reads must not wait for other devices: %s", p.Commands)
	}
	for a, path := range map[Action]Path{ActWrites: p.Writes, ActBackup: p.Backup, ActRestore: p.Restore, ActUpdate: p.Update} {
		if path != None || !strings.Contains(p.Reason(a), "2 other devices' group topics have not been read yet; refresh their identity") {
			t.Errorf("%s = %s %q", a, path, p.Reason(a))
		}
	}
	f.MQTT.OthersGroupUnread = 1
	if got := Choose(f).Reason(ActWrites); !strings.Contains(got, "1 other device's group topic has not been read yet; refresh its identity") {
		t.Errorf("singular: %q", got)
	}
	if Choose(base()).Writes != HTTP {
		t.Error("HTTP is unaffected")
	}
}
