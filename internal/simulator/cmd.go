package simulator

import (
	"regexp"
	"strconv"
	"strings"
)

// outMsg is an MQTT message a command produces.
type outMsg struct {
	topic   string
	payload string
	retain  bool
}

// minimalCmds is the whole command table of the minimal (and safeboot)
// image. Everything else answers {"Command":"Unknown"}.
var minimalCmds = map[string]bool{"upgrade": true, "upload": true, "otaurl": true, "seriallog": true, "restart": true}

// queryFiles maps commands to the captured reply that is replayed as is.
// Parameters of these commands are not interpreted (documented limitation).
var queryFiles = map[string]string{
	"state": "state.json", "module": "module.json", "modules": "modules.json",
	"template": "template.json", "hostname": "hostname.json", "topic": "topic.json",
	"teleperiod": "teleperiod.json", "webserver": "webserver.json",
	"fallbacktopic": "fallbacktopic.json", "gpio": "gpio.json", "gpios": "gpios.json",
	"weblog": "loglevel.json",
}

var reIndex = regexp.MustCompile(`^(.*?)(\d+)$`)

// splitCmd splits "Status0", "Power ON", "FooBar2 x y" into the lower-case
// base name, the numeric suffix of the first word and the rest.
func splitCmd(cmd string) (name, base string, idx int, hasIdx bool, data string) {
	name, data, _ = strings.Cut(cmd, " ")
	data = strings.TrimSpace(data)
	base = strings.ToLower(name)
	if m := reIndex.FindStringSubmatch(base); m != nil && m[1] != "" {
		if n, err := strconv.Atoi(m[2]); err == nil {
			base, idx, hasIdx = m[1], n, true
		}
	}
	return name, base, idx, hasIdx, data
}

func unknownReply(base string, idx int, hasIdx bool, data string) []byte {
	in := strings.ToUpper(base)
	if hasIdx && idx != 1 {
		in += strconv.Itoa(idx)
	}
	if data != "" {
		in += " " + data
	}
	return []byte(`{"Command":"Unknown","Input":` + jsonString(in) + `}`)
}

func errorReply(in string) []byte {
	return []byte(`{"Command":"Error","Input":` + jsonString(in) + `}`)
}

// exec runs one command line like Tasmota's ExecuteCommand and returns the
// JSON reply plus the MQTT messages it produces.
func (d *simDevice) exec(cmd string) ([]byte, []outMsg) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execLocked(cmd)
}

// execMQTT is exec for a command that arrived over MQTT: the file transfer
// needs the length of the topic and whether it was the group topic.
func (d *simDevice) execMQTT(cmd string, topicLen int, group bool) ([]byte, []outMsg) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mf.topicSize, d.mf.group = uint32(topicLen), group //nolint:gosec // G115: a topic is far below 4 GiB
	defer func() { d.mf.group = false }()
	return d.execLocked(cmd)
}

func (d *simDevice) execLocked(cmd string) ([]byte, []outMsg) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return []byte(`{"WARNING":"Enter command cmnd="}`), nil
	}
	name, base, idx, hasIdx, data := splitCmd(cmd)
	result := func(b []byte) ([]byte, []outMsg) {
		return b, []outMsg{{topic: "stat/" + d.topic + "/RESULT", payload: string(b)}}
	}
	if d.minimalLocked() && !minimalCmds[base] {
		return result(unknownReply(base, idx, hasIdx, data))
	}
	if body, ok := d.settingCmdLocked(base, idx, hasIdx, data); ok {
		return result(body)
	}
	if body, ok := d.ufsCmdLocked(base, idx, hasIdx, data); ok {
		return result(body)
	}
	v := d.viewLocked()
	switch base {
	case "status":
		return d.statusLocked(base, idx, hasIdx, data, v)
	case "power":
		return d.powerLocked(idx, hasIdx, data, v)
	case "pulsetime", "interlock":
		if data != "" {
			return result(unknownReply(base, idx, hasIdx, data)) // only the bare read is simulated
		}
		return result(d.readSettingLocked(base))
	case "otaurl":
		switch {
		case data == "1":
			d.runtime.otaURL = d.defaultOtaURL()
		case data != "":
			d.runtime.otaURL = data
		}
		return result([]byte(`{"OtaUrl":` + jsonString(d.runtime.otaURL) + `}`))
	case "upgrade", "upload":
		key := "Upgrade"
		if base == "upload" {
			key = "Upload"
		}
		if data == "1" || (len(data) >= 3 && newerVersion(data, d.version)) {
			if d.phase == PhaseRun {
				d.armLocked(cmd)
				d.phase0StartLocked()
			}
			return result([]byte(`{"` + key + `":` + jsonString("Version "+d.version+" from "+d.runtime.otaURL) + `}`))
		}
		return result([]byte(`{"` + key + `":` + jsonString("1 or >"+d.version+" to upgrade") + `}`))
	case "restart":
		if data == "1" || data == "99" {
			d.commitLocked() // SettingsSaveAll before the restart (no-op in minimal)
			d.spawn(d.restartSoon)
			return result([]byte(`{"Restart":"Restarting"}`))
		}
		return result([]byte(`{"Restart":"1 to restart, 2 to halt"}`))
	case "filedownload":
		return d.fileDownloadLocked(data)
	case "fileupload":
		return d.fileUploadLocked(data)
	case "seriallog":
		if n, err := strconv.Atoi(data); err == nil && n >= 0 && n <= 4 {
			d.runtime.serialLog = n
		}
		if d.runtime.serialLog >= 0 {
			n := strconv.Itoa(d.runtime.serialLog)
			return result([]byte(`{"SerialLog":{"` + n + `":{"Active":"` + n + `"}}}`))
		}
		return result(d.render("seriallog.json", v))
	case "backlog":
		if data == "" {
			return result(d.render("backlog-empty.json", v))
		}
		// Backlog itself answers nothing (CmndBacklog: ResponseClear). Each queued command
		// runs later and publishes its own messages; the merged body is what /cm returns.
		var parts [][]byte
		var out []outMsg
		for _, seg := range strings.Split(data, ";") {
			if strings.TrimSpace(seg) == "" {
				continue
			}
			b, msgs := d.execLocked(seg)
			parts = append(parts, b)
			out = append(out, msgs...)
		}
		return mergeObjects(parts...), out
	}
	_ = name
	if f, ok := queryFiles[base]; ok {
		return result(d.render(f, v))
	}
	return result(unknownReply(base, idx, hasIdx, data))
}

func (d *simDevice) defaultOtaURL() string {
	if d.esp32 {
		return "http://ota.tasmota.com/tasmota32/release/tasmota32.bin"
	}
	return "http://ota.tasmota.com/tasmota/release/tasmota.bin.gz"
}

var statusSuffix = map[string]string{
	"Status": "STATUS", "StatusPRM": "STATUS1", "StatusFWR": "STATUS2", "StatusLOG": "STATUS3",
	"StatusMEM": "STATUS4", "StatusNET": "STATUS5", "StatusMQT": "STATUS6", "StatusTIM": "STATUS7",
	"StatusSNS": "STATUS8", "StatusPTH": "STATUS9", "StatusSTS": "STATUS11",
}

func (d *simDevice) statusLocked(base string, idx int, hasIdx bool, data string, v view) ([]byte, []outMsg) {
	arg := data
	if arg == "" && hasIdx {
		arg = strconv.Itoa(idx)
	}
	pub := func(suffix string, b []byte) outMsg {
		return outMsg{topic: "stat/" + d.topic + "/" + suffix, payload: string(b)}
	}
	if arg == "" {
		// "Status" alone returns only the Status object (first member of Status 0).
		for _, m := range topMembers(d.render("status-0.json", v)) {
			if m.key == "Status" {
				b := mergeObjects([]byte("{" + string(m.raw) + "}"))
				return b, []outMsg{pub("STATUS", b)}
			}
		}
	}
	n, err := strconv.Atoi(arg)
	file := "status-" + arg + ".json"
	if err != nil || n < 0 || n > 11 || d.cap.files[file] == nil {
		b := errorReply("STATUS " + arg)
		return b, []outMsg{{topic: "stat/" + d.topic + "/RESULT", payload: string(b)}}
	}
	body := d.render(file, v)
	if n == 0 {
		var out []outMsg
		for _, m := range topMembers(body) {
			if s, ok := statusSuffix[m.key]; ok {
				out = append(out, pub(s, []byte("{"+string(m.raw)+"}")))
			}
		}
		return body, out
	}
	return body, []outMsg{pub("STATUS"+arg, body)}
}

// powerLocked is Power<n>: relay n (default 1); Power0 addresses all relays.
func (d *simDevice) powerLocked(idx int, hasIdx bool, data string, v view) ([]byte, []outMsg) {
	n := 1
	// A single-relay device answers every index with its one relay (the
	// captures do); a multi-relay device knows only its own.
	if hasIdx && len(d.relays) > 1 {
		n = idx
	}
	if n < 0 || n > len(d.relays) {
		return unknownReply("power", idx, hasIdx, data), nil
	}
	targets := []int{n - 1}
	if n == 0 {
		targets = targets[:0]
		for i := range d.relays {
			targets = append(targets, i)
		}
	}
	set := true
	for _, i := range targets {
		switch strings.ToLower(data) {
		case "on", "1", "true":
			d.relays[i] = true
		case "off", "0", "false":
			d.relays[i] = false
		case "toggle", "2":
			d.relays[i] = !d.relays[i]
		default:
			set = false
		}
	}
	var body []byte
	var out []outMsg
	for _, i := range targets {
		key := "POWER"
		if len(d.relays) > 1 {
			key += strconv.Itoa(i + 1)
		}
		state := onOff(d.relays[i])
		body = []byte(`{"` + key + `":"` + state + `"}`)
		out = append(out, outMsg{topic: "stat/" + d.topic + "/" + key, payload: state})
	}
	if set {
		out = append([]outMsg{{topic: "stat/" + d.topic + "/RESULT", payload: string(body)}}, out...)
	}
	_ = v
	return body, out
}

// newerVersion mirrors Tasmota's NewerVersion: 2-4 dotted numeric fields
// packed into a uint32, compared as numbers. A letter after the third field
// counts as the fourth byte.
func newerVersion(candidate, current string) bool {
	c, ok1 := packVersion(candidate)
	cur, ok2 := packVersion(current)
	return ok1 && ok2 && c > cur
}

func packVersion(s string) (uint32, bool) {
	fields := strings.Split(strings.TrimSpace(s), ".")
	if len(fields) < 2 || len(fields) > 4 {
		return 0, false
	}
	var v uint32
	for i := 0; i < 4; i++ {
		var b uint32
		if i < len(fields) {
			f := fields[i]
			var letter byte
			if i == 2 && len(f) > 0 && f[len(f)-1] >= 'a' && f[len(f)-1] <= 'z' {
				letter, f = f[len(f)-1]-'a'+1, f[:len(f)-1]
			}
			n, err := strconv.Atoi(f)
			if err != nil || n < 0 || n > 255 {
				return 0, false
			}
			b = uint32(n)
			if i == 2 && letter > 0 {
				v = v<<8 | b
				v = v<<8 | uint32(letter)
				return v, true
			}
		}
		v = v<<8 | b
	}
	return v, true
}

// readSettingLocked answers the bare PulseTime and Interlock queries.
func (d *simDevice) readSettingLocked(base string) []byte {
	if base == "interlock" {
		on := "OFF"
		if d.spec.Interlock != "" {
			on = "ON"
		}
		return []byte(`{"Interlock":"` + on + `","Groups":` + jsonString(d.spec.Interlock) + `}`)
	}
	set := make([]string, 8)
	for i := range set {
		n := 0
		if i < len(d.spec.PulseTimes) {
			n = d.spec.PulseTimes[i]
		}
		set[i] = strconv.Itoa(n)
	}
	zeros := strings.TrimSuffix(strings.Repeat("0,", 8), ",")
	return []byte(`{"PulseTime":{"Set":[` + strings.Join(set, ",") + `],"Remaining":[` + zeros + `]}}`)
}
