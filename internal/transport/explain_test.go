package transport

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: the device page must show the same sentences Choose acts on; if the wrappers drifted from
// the chooser the page would say "usable" while an action is refused.
func TestExplainersMatchTheChooser(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	f := Facts{Now: now, HTTP: HTTPState{State: HTTPFailing, Kind: HTTPFailAuth, Since: now}}
	if got := MQTTUnusable(f, false); got != mqttBlocker(f, false) || got == "" {
		t.Fatalf("MQTTUnusable = %q", got)
	}
	if got := f.HTTP.Describe(); got != "the device refused the web password" {
		t.Fatalf("Describe = %q", got)
	}
	f.FirmwareBaseURL = true
	f.FWReach = model.FWReach{State: model.FWReachNo, At: now.Add(-25 * time.Hour)}
	if EffectiveFWReach(f) != model.FWReachUnknown || FirmwareServerBlocker(f) != "" {
		t.Fatal("a 'no' older than 24 h must read as unknown")
	}
	f.FWReach.At = now.Add(-time.Hour)
	if EffectiveFWReach(f) != model.FWReachNo || FirmwareServerBlocker(f) == "" {
		t.Fatal("a recent 'no' must block")
	}
}
