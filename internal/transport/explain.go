package transport

import "github.com/nofuturekid/tasmofleet/internal/model"

// The explanations the device page shows next to the decision. They expose the sentences Choose
// builds from, so the UI never words a reason a second time.

// MQTTUnusable says why the device cannot be reached over MQTT, "" when it can. strict asks for
// what writes and transfers need (a known, unshared GroupTopic); false is enough for reads.
func MQTTUnusable(f Facts, strict bool) string { return mqttBlocker(f, strict) }

// FirmwareServerBlocker says why the device cannot fetch from TasmoFleet's firmware server
// (FIRMWARE_BASE_URL missing, or a recorded "no"), "" when it may.
func FirmwareServerBlocker(f Facts) string { return fwBlocker(f) }

// EffectiveFWReach is the firmware-server reach with the 24 h expiry of a "no" applied, as Choose
// reads it.
func EffectiveFWReach(f Facts) model.FWReachState { return fwReach(f) }

// Describe is the short word for a failing HTTP path, without the date.
func (h HTTPState) Describe() string { return h.failText() }
