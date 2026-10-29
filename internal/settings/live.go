package settings

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
)

// LiveReadable reports whether a setting can be read from a device without
// side effects: through a Status block, or through a bare query that only
// reports. A setting whose bare query acts (SideEffectQueries) and that has no
// Status block is not readable live; the policy view then says so instead of
// querying it.
func LiveReadable(cmd string) bool {
	if !Queryable(cmd) {
		return false
	}
	if _, ok := statusSource[cmd]; ok {
		return true
	}
	for _, bad := range SideEffectQueries {
		if bad == cmd {
			return false
		}
	}
	return true
}

// canonicalKey maps a reply key to the curated command name (the device may
// change the case). Any other readable command keeps its reply spelling; the
// caller matches it to a rule case-insensitively.
func canonicalKey(k string) (string, bool) {
	if cmd, ok := curatedKey(k); ok {
		return cmd, true
	}
	if _, curated := Curated(k); !curated && Queryable(k) {
		return k, true
	}
	return "", false
}

func curatedKey(k string) (string, bool) {
	for _, f := range Fields {
		if !f.Secret && strings.EqualFold(f.Key, k) {
			return f.Key, true
		}
	}
	if len(k) > len("SetOption") && strings.EqualFold(k[:9], "SetOption") && setOptionKeyRe.MatchString("SetOption"+k[9:]) {
		return "SetOption" + k[9:], true
	}
	return "", false
}

// ParseReply extracts policy-relevant settings from an MQTT message payload
// the device published on its own: stat/<topic>/STATUS (the Status object),
// STATUS3, and RESULT messages that answer a single setting command, whoever
// sent it. Anything else yields nil. Values are normalised like Read.
func ParseReply(payload []byte) map[string]string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil || len(obj) != 1 {
		return nil
	}
	out := map[string]string{}
	for k, raw := range obj {
		if k == "Status" || k == "StatusLOG" {
			for cmd, src := range statusSource {
				if src.section != k {
					continue
				}
				if v, ok := statusValue(obj, src.section, src.field); ok {
					out[cmd] = v
				}
			}
			break
		}
		cmd, ok := canonicalKey(k)
		if !ok {
			break
		}
		if Generic(cmd) {
			raw = cmdpolicy.ScrubReply(raw)
		}
		if v, ok := replyScalar(cmd, raw); ok {
			out[cmd] = safeValue(cmd, replyNumber(cmd, v))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// replyNumber turns SaveData's "Every 30 second(s)" into 30.
func replyNumber(cmd, v string) string {
	if cmd != "SaveData" {
		return v
	}
	if rest, ok := strings.CutPrefix(v, "Every "); ok {
		if n, err := strconv.Atoi(strings.Fields(rest + " ")[0]); err == nil {
			return strconv.Itoa(n)
		}
	}
	return v
}
