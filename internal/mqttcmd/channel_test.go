package mqttcmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

type pub struct {
	topic, command, payload string
	at                      time.Time
}

func (p pub) String() string { return strings.TrimSpace(p.topic + " " + p.command + " " + p.payload) }

// fakeBus is a bus whose device side the test scripts. A reply emitted from inside Command
// arrives before Command returns: the fastest device there is, which is what #272 was about.
type fakeBus struct {
	mu   sync.Mutex
	subs []fakeSub
	cmds []pub
	down bool
	// longPrefix, if set, replaces "cmnd/" in CommandTopic: a device behind a longer FullTopic.
	longPrefix string
	onPub      func(b *fakeBus, p pub)
}

type fakeSub struct {
	kinds map[mqttbus.Kind]bool
	h     func(mqttbus.Message)
}

func (b *fakeBus) Subscribe(kinds []mqttbus.Kind, h func(mqttbus.Message)) func() {
	s := fakeSub{kinds: map[mqttbus.Kind]bool{}, h: h}
	for _, k := range kinds {
		s.kinds[k] = true
	}
	b.mu.Lock()
	b.subs = append(b.subs, s)
	b.mu.Unlock()
	return func() {}
}

func (b *fakeBus) CommandTopic(topic, command string) string {
	if b.longPrefix != "" {
		return b.longPrefix + topic + "/" + command
	}
	return "cmnd/" + topic + "/" + command
}

func (b *fakeBus) Connected() bool { return !b.down }

func (b *fakeBus) Command(_ context.Context, topic, command, payload string) error {
	p := pub{topic, command, payload, time.Now()}
	b.mu.Lock()
	b.cmds = append(b.cmds, p)
	hook := b.onPub
	b.mu.Unlock()
	if hook != nil {
		hook(b, p)
	}
	return nil
}

func (b *fakeBus) emit(kind mqttbus.Kind, topic, suffix, payload string, retained bool) {
	b.mu.Lock()
	subs := append([]fakeSub(nil), b.subs...)
	b.mu.Unlock()
	for _, s := range subs {
		if s.kinds[kind] {
			s.h(mqttbus.Message{Kind: kind, Topic: topic, Suffix: suffix, Payload: []byte(payload), Retained: retained, At: time.Now()})
		}
	}
}

func (b *fakeBus) result(topic, payload string) {
	b.emit(mqttbus.KindResult, topic, "RESULT", payload, false)
}

func (b *fakeBus) sent() []pub {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]pub(nil), b.cmds...)
}

func plug() model.Device { return model.Device{ID: 1, Topic: "plug"} }

func fast() Options {
	return Options{Gap: time.Millisecond, ReplyWait: 300 * time.Millisecond, BacklogQuiet: 60 * time.Millisecond, BacklogMax: time.Second}
}

func newChan(b *fakeBus) *Channel { return New(b, fast()) }

// Why: the heap rule. Two callers for one device must never have two commands in flight, or a
// slow ESP8266 answers neither; the second waits until the first has its reply.
func TestTwoCommandsToOneDeviceAreSerialised(t *testing.T) {
	b := &fakeBus{}
	var mu sync.Mutex
	inflight, maxInflight := 0, 0
	b.onPub = func(b *fakeBus, p pub) {
		mu.Lock()
		inflight++
		maxInflight = max(maxInflight, inflight)
		mu.Unlock()
		go func() {
			time.Sleep(40 * time.Millisecond) // the device is busy
			mu.Lock()
			inflight--
			mu.Unlock()
			b.result(p.topic, `{"`+p.command+`":"Done"}`)
		}()
	}
	c := newChan(b)
	var wg sync.WaitGroup
	for _, cmd := range []string{"TelePeriod 60", "Hostname x"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Do(t.Context(), plug(), cmd); err != nil {
				t.Errorf("%s: %v", cmd, err)
			}
		}()
	}
	wg.Wait()
	if maxInflight != 1 || len(b.sent()) != 2 {
		t.Fatalf("max in flight = %d, sent %v", maxInflight, b.sent())
	}
}

// Why: a bus-wide gap between publishes to different devices keeps a fleet from being asked at
// once after a broker outage.
func TestPublishesAreSpacedByTheGap(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"`+p.command+`":"x"}`) }
	o := fast()
	o.Gap = 80 * time.Millisecond
	c := New(b, o)
	var wg sync.WaitGroup
	for _, topic := range []string{"a", "b", "c"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Do(t.Context(), model.Device{Topic: topic}, "Hostname"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	s := b.sent()
	if len(s) != 3 {
		t.Fatalf("sent %v", s)
	}
	slicesSortByTime(s)
	for i := 1; i < len(s); i++ {
		if gap := s[i].at.Sub(s[i-1].at); gap < 70*time.Millisecond {
			t.Errorf("publishes %d and %d only %v apart", i-1, i, gap)
		}
	}
}

func slicesSortByTime(s []pub) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j].at.Before(s[i].at) {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// Why (safety rule 2, heap bursts corrupt ESP8266 replies): Status 0 is never published, in any
// spelling Tasmota reads as 0, alone or hidden in a Backlog. The bus must stay silent.
func TestStatus0IsNeverPublished(t *testing.T) {
	for _, cmd := range []string{
		"Status 0", "status 0", "Status0", "status0", "Status  0", "Status 00", "Status +0", "Status 0x0", "Status off",
		"Status1 0", "Status 14", "Status -1", "Status 1 2", "Status 01",
		"Backlog Power ON; Status 0", "Backlog Status0", "Backlog0 status 00", "backlog Status 5;Status 0",
		// Tasmota strips leading "Backlog" words from every part (CmndBacklog): these are Status 0.
		"Backlog BacklogStatus 0", "Backlog Power1 1;backlogstatus0", "Backlog  BACKLOGBacklogStatus 0", "Backlog Backlog Status 0",
	} {
		b := &fakeBus{}
		c := newChan(b)
		if _, err := c.Do(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: err = %v, want ErrRefused", cmd, err)
		}
		if _, err := c.DoLoose(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) {
			t.Errorf("%q (loose): err = %v, want ErrRefused", cmd, err)
		}
		if s := b.sent(); len(s) != 0 {
			t.Errorf("%q published %v", cmd, s)
		}
	}
}

// Why: the plain Status and the numbered ones the readers use must keep working, with the number
// as the payload of cmnd/<topic>/Status.
func TestStatusNumbersAreAllowedAndAnsweredOnTheirTopic(t *testing.T) {
	for cmd, suffix := range map[string]string{"Status": "STATUS", "Status 2": "STATUS2", "Status2": "STATUS2", "status 13": "STATUS13", "Status 10": "STATUS10"} {
		b := &fakeBus{}
		b.onPub = func(b *fakeBus, p pub) {
			b.emit(mqttbus.KindStatus, p.topic, "STATUS3", `{"StatusLOG":{}}`, false) // another Status: not ours
			b.result(p.topic, `{"Status":"noise"}`)                                   // a RESULT is no answer to a Status
			b.emit(mqttbus.KindStatus, p.topic, suffix, `{"Answer":1}`, false)
		}
		got, err := newChan(b).Do(t.Context(), plug(), cmd)
		if err != nil || string(got) != `{"Answer":1}` {
			t.Errorf("%q: %s, %v", cmd, got, err)
		}
	}
}

// Why: a device that announced Offline gets nothing: the command would be lost, and the caller
// must know nothing was sent (so a router may try another path).
func TestOfflineDeviceGetsNoPublish(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"Hostname":"x"}`) }
	c := newChan(b)
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Offline", true)
	_, err := c.Do(t.Context(), plug(), "Hostname")
	if !errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrUnreachable", err)
	}
	if len(b.sent()) != 0 {
		t.Fatalf("published %v to an offline device", b.sent())
	}
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Online", false)
	if _, err := c.Do(t.Context(), plug(), "Hostname"); err != nil {
		t.Fatalf("after Online: %v", err)
	}
}

// Why: a down bus is "nothing was sent", wrapped so callers can tell it from a device problem.
func TestBusDownIsUnreachable(t *testing.T) {
	b := &fakeBus{down: true}
	_, err := newChan(b).Do(t.Context(), plug(), "Hostname")
	if !errors.Is(err, tasmota.ErrUnreachable) || !errors.Is(err, mqttbus.ErrNotConnected) || len(b.sent()) != 0 {
		t.Fatalf("err = %v, sent %v", err, b.sent())
	}
}

// Why: retained messages are old news (the broker replays them to every new subscriber); taking
// one as the answer would report a stale value as the result of this command.
func TestRetainedResultIsNotAnAnswer(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		b.emit(mqttbus.KindResult, p.topic, "RESULT", `{"Hostname":"stale"}`, true)
		b.result(p.topic, `{"Hostname":"fresh"}`)
	}
	got, err := newChan(b).Do(t.Context(), plug(), "Hostname")
	if err != nil || string(got) != `{"Hostname":"fresh"}` {
		t.Fatalf("%s, %v", got, err)
	}
}

// Why (live finding 5): a button press publishes {"POWER":"ON"} on the same topic while we ask
// TelePeriod. The press must not be taken for the answer; only the right key is. And when only
// noise arrives the outcome is a timeout, never the noise.
func TestForeignResultIsIgnored(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		b.result(p.topic, `{"POWER":"ON"}`)
		b.result(p.topic, `{"TelePeriod":60}`)
	}
	got, err := newChan(b).Do(t.Context(), plug(), "TelePeriod")
	if err != nil || string(got) != `{"TelePeriod":60}` {
		t.Fatalf("%s, %v", got, err)
	}
	b2 := &fakeBus{}
	b2.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"POWER":"ON"}`) }
	if _, err := newChan(b2).Do(t.Context(), plug(), "TelePeriod"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout (the press is not an answer)", err)
	}
}

// Why: the reply key follows Tasmota's naming, not our spelling: Power1 is answered by POWER1 or
// POWER, SO19 by SetOption19.
func TestKeysMatchTheCommandNameCaseAndIndexInsensitive(t *testing.T) {
	cases := []struct{ cmd, reply string }{
		{"Power1 ON", `{"POWER1":"ON"}`}, {"Power1 ON", `{"POWER":"ON"}`}, {"power ON", `{"POWER":"ON"}`},
		{"SO19 1", `{"SetOption19":"ON"}`}, {"SetOption19 1", `{"SetOption19":"ON"}`}, {"Rule1 1", `{"Rule1":{"State":"ON"}}`},
	}
	for _, tc := range cases {
		b := &fakeBus{}
		b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, tc.reply) }
		if got, err := newChan(b).Do(t.Context(), plug(), tc.cmd); err != nil || string(got) != tc.reply {
			t.Errorf("%q: %s, %v", tc.cmd, got, err)
		}
	}
}

// Why: the commands whose reply has other keys (Template) work on the console (DoLoose), but an
// automated read refuses to guess: it needs a key match.
func TestTemplateTakesTheFirstResultOnlyOnTheConsole(t *testing.T) {
	reply := `{"NAME":"Plug","GPIO":[0],"FLAG":0,"BASE":18}`
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, reply) }
	got, err := newChan(b).DoLoose(t.Context(), plug(), "Template")
	if err != nil || string(got) != reply {
		t.Fatalf("loose: %s, %v", got, err)
	}
	if _, err := newChan(b).Do(t.Context(), plug(), "Template"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("strict: err = %v, want ErrTimeout", err)
	}
	// Loose is not "anything goes": a command with a normal reply still needs its key.
	b2 := &fakeBus{}
	b2.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"POWER":"ON"}`) }
	if _, err := newChan(b2).DoLoose(t.Context(), plug(), "TelePeriod"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("loose TelePeriod: err = %v", err)
	}
}

// Why: {"Command":"Unknown"} is the device saying it does not know the command (minimal
// firmware); but only when it is about OUR command, or another poller's failure would fail ours.
func TestCommandUnknownAndErrorAreMappedForOurCommandOnly(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		b.result(p.topic, `{"Command":"Unknown","Input":"SOMETHINGELSE 1"}`)
		b.result(p.topic, `{"Command":"Unknown","Input":"HOSTNAME"}`)
	}
	if _, err := newChan(b).Do(t.Context(), plug(), "Hostname"); !errors.Is(err, tasmota.ErrUnknownCommand) {
		t.Fatalf("err = %v, want ErrUnknownCommand", err)
	}
	b2 := &fakeBus{}
	b2.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"Command":"Error","Input":"STATUS 99"}`) }
	if _, err := newChan(b2).Do(t.Context(), plug(), "Status 13"); !errors.Is(err, tasmota.ErrCommandError) {
		t.Fatalf("err = %v, want ErrCommandError", err)
	}
	b3 := &fakeBus{}
	b3.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"Command":"Unknown","Input":"OTHER"}`) }
	if _, err := newChan(b3).Do(t.Context(), plug(), "Hostname"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("a stranger's Unknown answered our command: %v", err)
	}
}

// Why: no answer is an unknown outcome (the command may have run). The channel reports it and
// sends exactly once; resending is the caller's decision.
func TestNoReplyIsATimeoutAndIsNeverResent(t *testing.T) {
	b := &fakeBus{}
	_, err := newChan(b).Do(t.Context(), plug(), "Restart 1")
	if !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(b.sent()); n != 1 {
		t.Fatalf("published %d times", n)
	}
}

// Why: a restart publishes LWT Offline. A command that is waiting for its answer must end then
// with an unknown outcome (not an "unreachable", which would claim nothing was sent).
func TestOfflineWhileWaitingIsAnUnknownOutcome(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		go func() {
			time.Sleep(20 * time.Millisecond)
			b.emit(mqttbus.KindLWT, p.topic, "LWT", "Offline", true)
		}()
	}
	o := fast()
	o.ReplyWait = 5 * time.Second
	_, err := New(b, o).Do(t.Context(), plug(), "Upgrade 1")
	if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrUnreachable) {
		t.Fatalf("err = %v, want ErrTimeout only", err)
	}
}

// Why: Tasmota v16 publishes nothing for the Backlog itself and one RESULT per queued command
// (docs/research/tasmota-mqtt-transport.md section 4). All replies are collected and merged in
// order, later keys winning, like the merged /cm reply the callers know.
func TestBacklogCollectsOneReplyPerQueuedCommand(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) {
		go func() {
			b.result(p.topic, `{"TelePeriod":60}`)
			time.Sleep(5 * time.Millisecond)
			b.emit(mqttbus.KindStatus, p.topic, "STATUS5", `{"StatusNET":{"IPAddress":"192.0.2.9"}}`, false)
			b.result(p.topic, `{"POWER":"ON"}`)
			b.result(p.topic, `{"TelePeriod":120}`)
		}()
	}
	got, err := newChan(b).Do(t.Context(), plug(), "Backlog TelePeriod 60; Status 5; Power ON; TelePeriod 120")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(got, &m) != nil || string(m["TelePeriod"]) != "120" || string(m["POWER"]) != `"ON"` || m["StatusNET"] == nil {
		t.Fatalf("merged = %s", got)
	}
	if !strings.HasPrefix(string(got), `{"TelePeriod":120,"StatusNET"`) {
		t.Errorf("order not kept: %s", got)
	}
	if s := b.sent(); len(s) != 1 || s[0].command != "Backlog" {
		t.Fatalf("sent %v", s)
	}
}

// Why: a queued command without a reply (Delay) must not hold the Backlog until the 10 s cap;
// it ends a quiet period after the last reply. A Backlog nobody answers is a timeout.
func TestBacklogEndsWhenQuietAndTimesOutWithoutAnyReply(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { go b.result(p.topic, `{"Hostname":"x"}`) }
	start := time.Now()
	got, err := newChan(b).Do(t.Context(), plug(), "Backlog Hostname x; Delay 10; Restart 1")
	if err != nil || string(got) != `{"Hostname":"x"}` || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("%s, %v after %v", got, err, time.Since(start))
	}
	if _, err := newChan(&fakeBus{}).Do(t.Context(), plug(), "Backlog Hostname x"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

// Why: Unknown for one queued command is surfaced, not swallowed in a merge.
func TestBacklogSurfacesAnUnknownQueuedCommand(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { go b.result(p.topic, `{"Command":"Unknown","Input":"NOPE"}`) }
	if _, err := newChan(b).Do(t.Context(), plug(), "Backlog Nope"); !errors.Is(err, tasmota.ErrUnknownCommand) {
		t.Fatalf("err = %v", err)
	}
}

// Why (#274): WebPassword and MqttPassword echo the new secret in their RESULT, which travels on
// stat/<topic>/RESULT. Nothing the channel returns may contain it.
func TestSecretSettingRepliesAreScrubbed(t *testing.T) {
	for cmd, reply := range map[string]string{
		"WebPassword hunter2hunter2":                     `{"WebPassword":"hunter2hunter2"}`,
		"MqttPassword hunter2hunter2":                    `{"MqttPassword":"hunter2hunter2"}`,
		"Backlog Hostname x; WebPassword hunter2hunter2": `{"Hostname":"x","WebPassword":"hunter2hunter2"}`,
	} {
		b := &fakeBus{}
		b.onPub = func(b *fakeBus, p pub) { go b.result(p.topic, reply) }
		got, err := newChan(b).Do(t.Context(), plug(), cmd)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if strings.Contains(string(got), "hunter2") {
			t.Errorf("%q: the reply carries the secret: %s", cmd, got)
		}
	}
}

// Why: the file transfer commands carry the device's MQTT password and answer on other topics;
// they have their own API, so the generic channel refuses them (also the echo in an Unknown reply
// must never exist).
func TestFileCommandsAreRefused(t *testing.T) {
	for _, cmd := range []string{`FileUpload {"Password":"x","Type":2}`, `fileupload`, `FileDownload 0`, `Backlog FileDownload 0`, `Upload 1`} {
		b := &fakeBus{}
		if _, err := newChan(b).Do(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
			t.Errorf("%q: err = %v, sent %v", cmd, err, b.sent())
		}
	}
}

// Why: a group topic would reach every device of the group, and wildcards or levels would reach
// other topics; the channel addresses exactly one device.
func TestTopicsThatReachMoreThanOneDeviceAreRefused(t *testing.T) {
	for _, d := range []model.Device{
		{Topic: "tasmotas", GroupTopic: "tasmotas"}, {Topic: "tasmotas"}, {Topic: "x", GroupTopic: "x"}, {Topic: "a/b"}, {Topic: "+"}, {Topic: "#"}, {Topic: ""}, {Topic: " x"},
	} {
		b := &fakeBus{}
		if _, err := newChan(b).Do(t.Context(), d, "Hostname"); !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
			t.Errorf("topic %q: err = %v, sent %v", d.Topic, err, b.sent())
		}
	}
}

// Why: a switch must be confirmed by its own relay. A multi-relay device reports POWER1 for
// relay 1; taking that as the answer to Power2 would show relay 2 as switched when it is not.
func TestAReplyForAnotherRelayDoesNotAnswer(t *testing.T) {
	ask := func(cmd, reply string) error {
		b := &fakeBus{}
		b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, reply) }
		_, err := newChan(b).Do(t.Context(), plug(), cmd)
		return err
	}
	if err := ask("Power2 ON", `{"POWER1":"ON"}`); !errors.Is(err, tasmota.ErrTimeout) {
		t.Errorf("Power2 answered by POWER1: %v", err)
	}
	if err := ask("Power2 ON", `{"POWER2":"ON"}`); err != nil {
		t.Errorf("Power2 by POWER2: %v", err)
	}
	if err := ask("Power0 ON", `{"POWER1":"ON"}`); err != nil {
		t.Errorf("Power0 (all relays) by POWER1: %v", err)
	}
	if err := ask("Power1 ON", `{"POWER":"ON"}`); err != nil {
		t.Errorf("Power1 by POWER (one-relay device): %v", err)
	}
}

// Why: a caller that gives up while queued behind another command must not publish afterwards.
func TestCancelledWhileQueuedPublishesNothing(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	c.sem("plug") <- struct{}{} // another command holds the device
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Do(ctx, plug(), "Hostname"); !errors.Is(err, context.DeadlineExceeded) || len(b.sent()) != 0 {
		t.Fatalf("err = %v, sent %v", err, b.sent())
	}
}

// Why: minimal-firmware detection reads INFO1. After a restart (LWT Offline) the INFO1 of the old
// boot is wrong (old version, old image), so the cache must drop it; a retained INFO1 is no
// reading of now either.
func TestInfoIsDroppedOnOfflineAndRetainedInfoIsIgnored(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"15.0.0"}}`, true)
	if _, ok := c.Info("plug"); ok {
		t.Fatal("a retained INFO1 was cached")
	}
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"15.0.0"}}`, false)
	b.emit(mqttbus.KindInfo, "plug", "INFO3", `{"Info3":{"RestartReason":"Software/System restart"}}`, false)
	i, ok := c.Info("plug")
	if !ok || !strings.Contains(string(i.Info1), "15.0.0") || !strings.Contains(string(i.Info3), "Software") {
		t.Fatalf("info = %+v, %v", i, ok)
	}
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Offline", true)
	if _, ok := c.Info("plug"); ok {
		t.Fatal("INFO1 of the previous boot survived the restart")
	}
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"16.0.0"}}`, false)
	if i, _ := c.Info("plug"); !strings.Contains(string(i.Info1), "16.0.0") {
		t.Fatalf("info = %+v", i)
	}
}

// Why: a broker full of foreign topics must not grow the tables; only inventory devices count.
func TestKnownLimitsTheTables(t *testing.T) {
	b := &fakeBus{}
	o := fast()
	o.Known = func(m mqttbus.Message) bool { return m.Topic == "plug" }
	c := New(b, o)
	b.emit(mqttbus.KindInfo, "stranger", "INFO1", `{}`, false)
	b.emit(mqttbus.KindLWT, "stranger", "LWT", "Offline", true)
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{}`, false)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.info) != 1 || len(c.lwt) != 0 {
		t.Fatalf("info %d, lwt %d", len(c.info), len(c.lwt))
	}
}

// Why: a Backlog inside a Backlog is refused (Tasmota strips the inner word, so it would only hide
// the real command from the checks); nothing is published.
func TestNestedBacklogIsRefused(t *testing.T) {
	for _, cmd := range []string{"Backlog Backlog Power ON", "Backlog Power ON; Backlog Power OFF", "Backlog backlog", "Backlog Backlog"} {
		b := &fakeBus{}
		if _, err := newChan(b).Do(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) || len(b.sent()) != 0 {
			t.Errorf("%q: err = %v, sent %v", cmd, err, b.sent())
		}
	}
}

// Why: after a timeout the reply may still come. It must not be taken as the answer of the NEXT
// command with the same key (a stale value reported as the result of a new write): the topic stays
// busy until the late reply arrives, and that reply is dropped.
func TestALateReplyDoesNotAnswerTheNextCommand(t *testing.T) {
	b := &fakeBus{}
	n := 0
	var mu sync.Mutex
	b.onPub = func(b *fakeBus, p pub) {
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k == 2 {
			go b.result(p.topic, `{"Hostname":"new"}`)
		}
	}
	c := newChan(b)
	if _, err := c.Do(t.Context(), plug(), "Hostname"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("first: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		b.result("plug", `{"Hostname":"old"}`) // the late reply of the first command
	}()
	got, err := c.Do(t.Context(), plug(), "Hostname")
	if err != nil || string(got) != `{"Hostname":"new"}` {
		t.Fatalf("second: %s, %v", got, err)
	}
}

// Why: a device that never answers must not block its topic for ever; the hold ends after a grace.
func TestTheTopicIsReleasedAfterTheGrace(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	if _, err := c.Do(t.Context(), plug(), "Hostname"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatal(err)
	}
	b.onPub = func(b *fakeBus, p pub) { b.result(p.topic, `{"Hostname":"x"}`) }
	if _, err := c.Do(t.Context(), plug(), "Hostname"); err != nil {
		t.Fatalf("second command after the grace: %v", err)
	}
}

// Why: a caller that gives up AFTER the publish does not know whether the command ran: that is
// the unknown outcome (ErrTimeout, still matching the context error), unlike giving up while
// queued, where nothing was sent.
func TestGivingUpAfterThePublishIsAnUnknownOutcome(t *testing.T) {
	b := &fakeBus{}
	o := fast()
	o.ReplyWait = 5 * time.Second
	c := New(b, o)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Do(ctx, plug(), "Restart 1")
	if !errors.Is(err, tasmota.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) || len(b.sent()) != 1 {
		t.Fatalf("err = %v, sent %v", err, b.sent())
	}
	// Queued only: plain context error, not an unknown outcome.
	c.sem("other") <- struct{}{}
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	_, err = c.Do(ctx2, model.Device{Topic: "other"}, "Hostname")
	if errors.Is(err, tasmota.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued: err = %v", err)
	}
}

// Why: a retained LWT arrives only right after we subscribed; the device may have rebooted while we
// were away, so an INFO1 cached before is of unknown age and must not be used for minimal detection.
func TestRetainedLWTDropsTheInfo(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"15.0.0"}}`, false)
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Online", true)
	if _, ok := c.Info("plug"); ok {
		t.Fatal("INFO1 from before a retained LWT survived")
	}
	b.emit(mqttbus.KindInfo, "plug", "INFO1", `{"Info1":{"Version":"16.0.0"}}`, false)
	b.emit(mqttbus.KindLWT, "plug", "LWT", "Online", false)
	if _, ok := c.Info("plug"); !ok {
		t.Fatal("a live Online dropped the INFO1 that followed")
	}
}

// Why: with SetOption4 1 a device reports a switched relay on stat/<t>/POWERn and not on RESULT;
// that still confirms the switch (the old behaviour of Power over MQTT), for its own relay only.
func TestPowerTopicAnswersAPowerCommand(t *testing.T) {
	b := &fakeBus{}
	b.onPub = func(b *fakeBus, p pub) { b.emit(mqttbus.KindPower, p.topic, "POWER2", "ON", false) }
	got, err := newChan(b).Do(t.Context(), plug(), "Power2 ON")
	if err != nil || string(got) != `{"POWER2":"ON"}` {
		t.Fatalf("%s, %v", got, err)
	}
	if _, err := newChan(b).Do(t.Context(), plug(), "Power1 ON"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("POWER2 answered Power1: %v", err)
	}
	b2 := &fakeBus{}
	b2.onPub = func(b *fakeBus, p pub) { b.emit(mqttbus.KindPower, p.topic, "POWER", "ON", true) }
	if _, err := newChan(b2).Do(t.Context(), plug(), "Power ON"); !errors.Is(err, tasmota.ErrTimeout) {
		t.Fatalf("a retained POWER answered: %v", err)
	}
}

// Why: on shutdown a command in flight must not hold the process for its whole reply wait.
func TestCloseWakesACommandInFlight(t *testing.T) {
	b := &fakeBus{}
	o := fast()
	o.ReplyWait = time.Minute
	c := New(b, o)
	errc := make(chan error, 1)
	go func() { _, err := c.Do(t.Context(), plug(), "Hostname"); errc <- err }()
	waitFor(t, "the publish", func() bool { return len(b.sent()) == 1 })
	c.Close()
	select {
	case err := <-errc:
		// Why: the command was published; the device may have run it. "Nothing was sent" would
		// let a caller resend a Power TOGGLE or Restart.
		if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrUnreachable) {
			t.Fatalf("err = %v, want the unknown outcome (ErrTimeout)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the command")
	}
}

// Why: the same for a Backlog being collected when the process shuts down.
func TestCloseDuringABacklogIsAnUnknownOutcome(t *testing.T) {
	b := &fakeBus{}
	o := fast()
	o.ReplyWait = time.Minute
	c := New(b, o)
	errc := make(chan error, 1)
	go func() { _, err := c.Do(t.Context(), plug(), "Backlog Power ON; Power2 ON"); errc <- err }()
	waitFor(t, "the publish", func() bool { return len(b.sent()) == 1 })
	c.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrUnreachable) {
			t.Fatalf("err = %v, want the unknown outcome (ErrTimeout)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the Backlog")
	}
}

// Why: after Close a free semaphore must not win the select against the closed channel; the
// command is refused with nothing published.
func TestDoAfterCloseNeverPublishes(t *testing.T) {
	b := &fakeBus{}
	c := newChan(b)
	c.Close()
	for i := 0; i < 50; i++ {
		if _, err := c.Do(t.Context(), plug(), "Hostname"); !errors.Is(err, tasmota.ErrUnreachable) {
			t.Fatalf("err = %v", err)
		}
	}
	if s := b.sent(); len(s) != 0 {
		t.Fatalf("published after Close: %v", s)
	}
}

// Why: Json {"Status":0} is CmndJson -> Backlog0 -> Status 0, Publish sends it to the device's own
// command topic, a Rule runs it, WebSend/WebQuery/Br/Script can reach it too. Status 0 is never
// sent, so none of these may be published, alone or in a Backlog, in any spelling.
func TestCommandsThatRunOtherCommandsAreRefused(t *testing.T) {
	for _, cmd := range []string{
		`Json {"Status":0}`, `json {"Backlog":"Status 0"}`, `Publish cmnd/plug/Status 0`, `Publish2 cmnd/plug/Status 0`,
		`Rule1 ON System#Boot DO Status 0 ENDON`, `Rule ON System#Boot DO Status 0 ENDON`, `RuleTimer1 5`,
		`WebSend [192.0.2.1] Status 0`, `WebQuery http://192.0.2.1/cm?cmnd=Status%200 GET`, `Br print(1)`, `Script >D`, `UfsRun x.be`,
		`Backlog Power ON; Json {"Status":0}`, `Backlog Publish cmnd/plug/Status 0`,
	} {
		b := &fakeBus{}
		c := newChan(b)
		if _, err := c.Do(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) {
			t.Errorf("%q: err = %v, want ErrRefused", cmd, err)
		}
		if _, err := c.DoLoose(t.Context(), plug(), cmd); !errors.Is(err, ErrRefused) {
			t.Errorf("%q (loose): err = %v, want ErrRefused", cmd, err)
		}
		if s := b.sent(); len(s) != 0 {
			t.Errorf("%q published %v", cmd, s)
		}
	}
}
