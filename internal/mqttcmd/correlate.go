package mqttcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// otherKeys lists the commands whose RESULT does not carry the command's name as a key. Checked
// against Tasmota v16 (support_command.ino): Template answers {"NAME":..,"GPIO":..,"FLAG":..,
// "BASE":..} (CmndTemplate -> TemplateJson); Gpios publishes its lines itself (ShowGpios) and
// clears the response. Module, Modules and Gpio do carry their name (S_JSON_COMMAND_NVALUE_SVALUE,
// "Modules", "GPIOn"/"GPIO"), so they are matched like any other command. Only DoLoose, the
// console, takes the first RESULT for these; an automated read needs a key match.
var otherKeys = map[string]bool{"template": true, "gpios": true}

// fileCommands carry the device's MQTT password in their payload and answer on their own topics.
var fileCommands = map[string]bool{"filedownload": true, "fileupload": true, "upload": true}

// executors are commands that run other commands, or publish messages that do: Json (CmndJson ->
// Backlog0, so {"Status":0} is a Status 0), Publish and Publish2 (to the device's own cmnd topic),
// Rule and RuleTimer (DO Status 0), WebSend and WebQuery (to any host, a device included), Br
// (Berry), Script and UfsRun (runs a script file). Status 0 is never sent, so a command that can send it on our behalf is
// never published either, alone or inside a Backlog. The console and bulk keep them for devices
// reached over HTTP (cmdpolicy asks an admin to confirm there); over MQTT there is no such path.
var executors = map[string]bool{
	"json": true, "publish": true, "publish2": true, "rule": true, "ruletimer": true,
	"websend": true, "webquery": true, "br": true, "script": true, "ufsrun": true,
}

// ruleSwitch reports a Rule command that only reads or switches a rule ("Rule1", "Rule1 1",
// "Rule2 off"): no rule text is stored, so nothing new can run.
func ruleSwitch(r *request) bool {
	if r.base != "rule" {
		return false
	}
	switch strings.ToLower(r.args) {
	case "", "0", "1", "2", "4", "5", "on", "off", "toggle", "stop", "once", "stoponce":
		return true
	}
	return false
}

// request is one parsed command.
type request struct {
	name    string // as sent, "Power1"
	args    string
	base    string // lower case, no trailing digits, synonyms resolved
	status  string // expected suffix for Status: "STATUS" or "STATUS5"; "" for other commands
	redact  bool   // the reply may echo a credential
	backlog []*request
}

// baseOf is the lower-case command name without its index. It resolves Tasmota's synonyms the
// way cmdpolicy does, so SO19 and SetOption19 are the same command.
func baseOf(name string) string {
	b := strings.ToLower(strings.TrimRight(name, "0123456789"))
	if b == "so" {
		return "setoption"
	}
	return b
}

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// parseRequest checks cmd with cmdpolicy's grammar and returns what to publish and how to
// recognise the answer. Status 0 and everything that is not a plain Status 1..13 is refused, in
// every spelling and inside a Backlog.
func parseRequest(cmd string) (*request, error) {
	v, err := cmdpolicy.Classify(cmd)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRefused, err)
	}
	cmd = strings.Trim(cmd, " ")
	name, args, _ := strings.Cut(cmd, " ")
	r := &request{name: name, args: strings.TrimSpace(args), base: baseOf(name), redact: v.Redact}
	idx := name[len(strings.TrimRight(name, "0123456789")):]
	switch {
	case fileCommands[r.base]:
		return nil, refuse("%s carries the device's MQTT password; file transfers have their own API", r.name)
	case executors[r.base] && !ruleSwitch(r):
		return nil, refuse("%s can run other commands (Status 0 among them), so it is not sent over MQTT", r.name)
	case r.base == "backlog":
		if idx == "2" || idx == "3" {
			return nil, refuse("%s publishes no results, so nothing could be matched", r.name)
		}
		return parseBacklog(r)
	case r.base == "status":
		arg := r.args
		switch {
		case idx != "" && arg != "":
			return nil, refuse("%s %s: which Status is meant?", r.name, arg)
		case idx != "":
			arg = idx
		}
		if arg == "" {
			r.status = "STATUS"
			return r, nil
		}
		n, err := strconv.Atoi(arg)
		if err != nil || strconv.Itoa(n) != arg || n < 1 || n > 13 {
			return nil, refuse("Status %s is not a plain Status 1 to 13 (Status 0 is never sent)", arg)
		}
		r.status = "STATUS" + arg
	}
	return r, nil
}

func parseBacklog(r *request) (*request, error) {
	for _, part := range strings.Split(r.args, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Tasmota strips leading "Backlog" words from every part (CmndBacklog, case-insensitive, no
		// space needed), so "BacklogStatus 0" is Status 0. No part may start that way.
		if len(part) >= len("backlog") && strings.EqualFold(part[:len("backlog")], "backlog") {
			return nil, refuse("a Backlog part starts with \"Backlog\"")
		}
		sub, err := parseRequest(part)
		if err != nil {
			return nil, err
		}
		if sub.base == "backlog" {
			return nil, refuse("a Backlog inside a Backlog")
		}
		r.backlog = append(r.backlog, sub)
		r.redact = r.redact || sub.redact
	}
	if len(r.backlog) == 0 {
		return nil, refuse("an empty Backlog")
	}
	return r, nil
}

// verdict is the decision about one incoming message that belongs to a command: its reply, or
// the error a Command Unknown/Error reply stands for.
type verdict struct {
	reply []byte
	err   error
}

// match decides whether m is the answer to r. ok is false for a message that belongs to someone
// else (a button press, another poller, a retained message).
func (r *request) match(m mqttbus.Message, loose bool) (v verdict, ok bool) {
	if m.Retained {
		return verdict{}, false
	}
	if r.status != "" && m.Kind == mqttbus.KindStatus {
		if m.Suffix == r.status {
			return verdict{reply: m.Payload}, true
		}
		return verdict{}, false
	}
	if m.Kind == mqttbus.KindPower && r.base == "power" && (string(m.Payload) == "ON" || string(m.Payload) == "OFF") {
		// SetOption4 1 devices report a switched relay on stat/<t>/POWERn instead of RESULT.
		if keyNames(m.Suffix, r.name, r.base) {
			return verdict{reply: []byte(`{"` + m.Suffix + `":"` + string(m.Payload) + `"}`)}, true
		}
		return verdict{}, false
	}
	if m.Kind != mqttbus.KindResult {
		return verdict{}, false
	}
	keys, values, isObj := topLevel(m.Payload)
	if !isObj {
		return verdict{}, false
	}
	if cmdVal, has := values["Command"]; has {
		var word string
		if json.Unmarshal(cmdVal, &word) != nil || (word != "Unknown" && word != "Error") {
			return verdict{}, false
		}
		var input string
		if in, hasIn := values["Input"]; hasIn && json.Unmarshal(in, &input) == nil {
			first, _, _ := strings.Cut(strings.TrimSpace(input), " ")
			if baseOf(first) != r.base {
				return verdict{}, false // someone else's failed command
			}
		}
		// The Input is not returned: for a secret-setting command it would be the secret.
		if word == "Unknown" {
			return verdict{err: tasmota.ErrUnknownCommand}, true
		}
		return verdict{err: tasmota.ErrCommandError}, true
	}
	if r.status != "" {
		return verdict{}, false // a Status answers on STATUSn; any other RESULT is noise
	}
	for _, k := range keys {
		if keyNames(k, r.name, r.base) {
			return verdict{reply: m.Payload}, true
		}
	}
	if loose && otherKeys[r.base] {
		return verdict{reply: m.Payload}, true
	}
	return verdict{}, false
}

// matchesAny reports whether m would have answered r in any mode: a late reply to an abandoned request.
func (r *request) matchesAny(m mqttbus.Message) bool {
	if r.backlog != nil {
		for _, sub := range r.backlog {
			if _, ok := sub.match(m, true); ok {
				return true
			}
		}
		return false
	}
	_, ok := r.match(m, true)
	return ok
}

// keyNames says whether a reply key belongs to the command: "POWER1" and "POWER" answer Power1,
// "SetOption19" answers SO19. Case does not matter. The index matters: POWER1 does not answer
// Power2 (a multi-relay device reports its relays one by one, and a switch must be confirmed by
// its own relay). A missing index means 1, and index 0 ("Power0", all relays) takes any.
func keyNames(key, name, base string) bool {
	if strings.EqualFold(key, name) {
		return true
	}
	if baseOf(key) != base {
		return false
	}
	want, got := indexOf(name), indexOf(key)
	return want == "0" || want == got
}

// indexOf is the numeric suffix of a name, "1" when there is none.
func indexOf(name string) string {
	i := name[len(strings.TrimRight(name, "0123456789")):]
	if i == "" {
		return "1"
	}
	return i
}

// topLevel returns the top-level keys of a JSON object in order, and their raw values.
func topLevel(payload []byte) (keys []string, values map[string]json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, nil, false
	}
	values = map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		k, isStr := t.(string)
		var v json.RawMessage
		if !isStr || dec.Decode(&v) != nil {
			return nil, nil, false
		}
		keys = append(keys, k)
		values[k] = v
	}
	return keys, values, true
}
