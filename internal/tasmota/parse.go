package tasmota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strings"
)

// Device output is untrusted and sometimes damaged (spliced replies from
// heap bursts, raw control characters in names). Every parser goes through
// decodeObject, which neutralises control characters, requires a JSON
// object and maps the known non-data shapes to sentinel errors.

func badResponse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrBadResponse, fmt.Sprintf(format, a...))
}

func stripControl(b []byte) []byte {
	return bytes.Map(func(r rune) rune {
		if r < 0x20 {
			return ' '
		}
		return r
	}, b)
}

// decodeObject parses raw as one JSON object. Control characters are
// replaced by spaces: outside strings they are whitespace, inside strings
// strict JSON would reject them.
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(stripControl(raw), &m); err != nil {
		return nil, badResponse("not a JSON object")
	}
	if m == nil {
		return nil, badResponse("null reply")
	}
	return m, nil
}

func stringField(m map[string]json.RawMessage, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// classifyObject maps the reply shapes that carry no data to errors. A nil
// result means "a normal reply, interpret the keys yourself".
func classifyObject(m map[string]json.RawMessage) error {
	if cmd, _ := stringField(m, "Command"); cmd == "Unknown" {
		return ErrUnknownCommand
	} else if cmd == "Error" {
		return ErrCommandError
	}
	if w, ok := stringField(m, "WARNING"); ok && strings.HasPrefix(w, "Need user") {
		return ErrAuth
	}
	return nil
}

// classifyReply validates a raw /cm reply body for the client.
func classifyReply(raw []byte) error {
	m, err := decodeObject(raw)
	if err != nil {
		return err
	}
	return classifyObject(m)
}

// section decodes the object under key (e.g. "StatusFWR") into out.
func section(raw json.RawMessage, key string, out any) (map[string]json.RawMessage, error) {
	m, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := classifyObject(m); err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok {
		return nil, badResponse("missing %s", key)
	}
	if err := json.Unmarshal(stripControl(v), out); err != nil {
		return nil, badResponse("bad %s", key)
	}
	return m, nil
}

// ParseStatus2 extracts StatusFWR. A {"Command":"Unknown"} answer (minimal
// firmware) yields ErrUnknownCommand.
func ParseStatus2(raw json.RawMessage) (StatusFWR, error) {
	var s StatusFWR
	if _, err := section(raw, "StatusFWR", &s); err != nil {
		return StatusFWR{}, err
	}
	return s, nil
}

// ParseStatus4 extracts StatusMEM.
func ParseStatus4(raw json.RawMessage) (StatusMEM, error) {
	var s StatusMEM
	if _, err := section(raw, "StatusMEM", &s); err != nil {
		return StatusMEM{}, err
	}
	return s, nil
}

var powerKey = regexp.MustCompile(`^POWER[0-9]*$`)

// ParseStatus11 extracts StatusSTS including POWERn fields.
func ParseStatus11(raw json.RawMessage) (StatusSTS, error) {
	var s StatusSTS
	if _, err := section(raw, "StatusSTS", &s); err != nil {
		return StatusSTS{}, err
	}
	var all map[string]json.RawMessage
	if _, err := section(raw, "StatusSTS", &all); err != nil {
		return StatusSTS{}, err
	}
	for k, v := range all {
		var val string
		if powerKey.MatchString(k) && json.Unmarshal(v, &val) == nil {
			if s.Power == nil {
				s.Power = map[string]string{}
			}
			s.Power[k] = val
		}
	}
	return s, nil
}

var (
	upgradeIdle     = regexp.MustCompile(`^1 or >(\S+) to upgrade$`)
	upgradeAccepted = regexp.MustCompile(`^Version (\S+) from `)
)

// ParseUpgradeProbe parses the answer to a bare "Upgrade" command.
func ParseUpgradeProbe(raw json.RawMessage) (UpgradeProbe, error) {
	m, err := decodeObject(raw)
	if err != nil {
		return UpgradeProbe{}, err
	}
	if err := classifyObject(m); err != nil {
		return UpgradeProbe{}, err
	}
	// "Upload" is the same command under another name and echoes its key.
	s, ok := stringField(m, "Upgrade")
	if !ok {
		s, ok = stringField(m, "Upload")
	}
	if !ok {
		return UpgradeProbe{}, badResponse("missing Upgrade")
	}
	if g := upgradeIdle.FindStringSubmatch(s); g != nil {
		return UpgradeProbe{Idle: true, Version: g[1]}, nil
	}
	if g := upgradeAccepted.FindStringSubmatch(s); g != nil {
		return UpgradeProbe{Version: g[1]}, nil
	}
	if s == "Safeboot" {
		return UpgradeProbe{}, nil
	}
	return UpgradeProbe{}, badResponse("unrecognised Upgrade reply")
}

// ParseOtaURL extracts the value of an "OtaUrl" answer.
func ParseOtaURL(raw json.RawMessage) (string, error) {
	m, err := decodeObject(raw)
	if err != nil {
		return "", err
	}
	if err := classifyObject(m); err != nil {
		return "", err
	}
	s, ok := stringField(m, "OtaUrl")
	if !ok {
		return "", badResponse("missing OtaUrl")
	}
	return s, nil
}

var (
	rootFooter = regexp.MustCompile(`Tasmota (\S+) \(([^)<>]*)\) by Theo Arends`)
	rootLang   = regexp.MustCompile(`(?is)<html[^>]*\slang\s*=\s*["']([^"']*)["']`)
	rootTitle  = regexp.MustCompile(`(?is)<title>(.*?)</title>`)
)

// ParseRootPage extracts RootInfo from the HTML of GET /.
func ParseRootPage(page []byte) (RootInfo, error) {
	g := rootFooter.FindSubmatch(page)
	if g == nil {
		return RootInfo{}, badResponse("no Tasmota footer on main page")
	}
	info := RootInfo{Version: string(g[1]), Image: string(g[2])}
	if l := rootLang.FindSubmatch(page); l != nil {
		info.Lang = html.UnescapeString(string(l[1]))
	}
	if t := rootTitle.FindSubmatch(page); t != nil {
		info.Title = html.UnescapeString(strings.TrimSpace(string(t[1])))
	}
	lower := bytes.ToLower(page)
	info.Minimal = bytes.Contains(lower, []byte("minimal firmware")) ||
		bytes.Contains(lower, []byte("<h3>safeboot")) ||
		strings.Contains(strings.ToLower(info.Image), "minimal")
	return info, nil
}
