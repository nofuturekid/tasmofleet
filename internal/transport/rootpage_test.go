package transport

import (
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// infoMQTT is the fake channel with the INFO1 cache of the real one.
type infoMQTT struct {
	*fakeMQTT
	info map[string]mqttcmd.Info
}

func (m infoMQTT) Info(topic string) (mqttcmd.Info, bool) { i, ok := m.info[topic]; return i, ok }

func rootRig(t *testing.T, info1 string) (*rig, infoMQTT) {
	t.Helper()
	g := newRig(t)
	m := infoMQTT{fakeMQTT: g.mq, info: map[string]mqttcmd.Info{}}
	if info1 != "" {
		m.info["plug"] = mqttcmd.Info{Info1: []byte(info1)}
	}
	g.r.SetMQTT(m)
	return g, m
}

// Why (D9/D10): over MQTT the main page does not exist; the image tag in INFO1 is the only proof that
// a device runs minimal firmware, and the language build cannot be read at all, so it must come back
// as "not known" and never as English.
func TestRootPageOverMQTTFromInfo1(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Module":"Sonoff Basic","Version":"15.1.0(release-minimal)","GroupTopic":"cmnd/tasmotas/"}}`)
	ri, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target())
	if err != nil {
		t.Fatal(err)
	}
	if ri.Version != "15.1.0" || ri.Image != "release-minimal" || !ri.Minimal {
		t.Fatalf("root = %+v", ri)
	}
	if ri.LangKnown || ri.Lang != "" {
		t.Fatalf("an INFO1 must not claim a language: %+v", ri)
	}
	if g.http.n() != 0 {
		t.Fatal("a job pinned to MQTT touched HTTP")
	}
}

// Why: an INFO1 from before a restart is dropped by the channel (LWT Offline); with none left the page
// must fail, so ProbeIdentity fails and Guard never runs on a guess.
func TestRootPageOverMQTTWithoutInfo1FailsAsBadResponse(t *testing.T) {
	g, _ := rootRig(t, "")
	_, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target())
	if !errors.Is(err, ErrNoInfo) || !errors.Is(err, tasmota.ErrBadResponse) {
		t.Fatalf("err = %v, want ErrNoInfo (an ErrBadResponse)", err)
	}
	if g.http.n() != 0 {
		t.Fatal("fell back to HTTP")
	}
}

func TestRootPageOverMQTTRefusesUnreadableInfo1(t *testing.T) {
	for _, in := range []string{`{"Info1":{"Version":"nonsense"}}`, `{"Info1":{"Version":"15.1.0()"}}`, `not json`, `{}`} {
		g, _ := rootRig(t, in)
		if _, err := g.r.RootPage(WithPath(t.Context(), MQTT), g.target()); !errors.Is(err, tasmota.ErrBadResponse) {
			t.Fatalf("%s: err = %v", in, err)
		}
	}
}

// Why (D5): a job that pinned HTTP never reads INFO1, and a device whose HTTP fails reads the page
// over MQTT without a pin.
func TestRootPagePathSelection(t *testing.T) {
	g, _ := rootRig(t, `{"Info1":{"Version":"15.1.0(release-tasmota)"}}`)
	if ri, err := g.r.RootPage(WithPath(t.Context(), HTTP), g.target()); err != nil || ri.Version != "" {
		t.Fatalf("pinned HTTP must use the (empty) HTTP page, got %+v %v", ri, err)
	}
	g.mem.Fail(7, HTTPFailUnreachable)
	ri, err := g.r.RootPage(t.Context(), g.target())
	if err != nil || ri.Version != "15.1.0" {
		t.Fatalf("unpinned, HTTP failing: %+v %v", ri, err)
	}
}
