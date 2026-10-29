package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Stateful settings: commands that set a value change the device state, the
// following Status replies and /dl dumps show it, and the value follows the
// flash rules of the real firmware: it lives in RAM (runtime.kv) until a
// save event copies it to flash (persisted.kv): SaveData, Restart, an OTA,
// a restart caused by the setting itself, or any change while SaveData > 0.
// A PowerCycle loses unsaved values; the minimal image has none of these
// commands and never saves.

var (
	reSOWords   = regexp.MustCompile(`"SetOption":\["([0-9A-Fa-f]+)","([0-9A-Fa-f]+)","([0-9A-Fa-f]+)","([0-9A-Fa-f]+)","([0-9A-Fa-f]+)","([0-9A-Fa-f]+)"\]`)
	reSOArray   = regexp.MustCompile(`"SetOption":\[[^\]]*\]`)
	reFriendly  = regexp.MustCompile(`"FriendlyName":\[[^\]]*\]`)
	reTZ        = regexp.MustCompile(`"Timezone":(99|-?\d+|"[^"]*")`)
	reTeleP     = regexp.MustCompile(`"TelePeriod":\d+`)
	rePowerOn   = regexp.MustCompile(`"PowerOnState":\d+`)
	reSaveData  = regexp.MustCompile(`"SaveData":\d+`)
	reHostname  = regexp.MustCompile(`"Hostname":"[^"]*"`)
	reTopicKey  = regexp.MustCompile(`"Topic":"[^"]*"`)
	reMqttHost  = regexp.MustCompile(`"MqttHost":"[^"]*"`)
	reMqttPort  = regexp.MustCompile(`"MqttPort":\d+`)
	reMqttUser  = regexp.MustCompile(`"MqttUser":"[^"]*"`)
	reFriendlyN = regexp.MustCompile(`"([^"]*)"`)
	reSettingSO = regexp.MustCompile(`^setoption(\d+)$`)
)

var restartKeys = map[string]bool{
	"hostname": true, "topic": true, "fulltopic": true,
	"mqtthost": true, "mqttport": true, "mqttuser": true, "mqttpassword": true,
}

const (
	defTimeDst = "0,0,3,1,2,120"
	defTimeStd = "0,0,10,1,3,60"
)

// soInitial returns SetOption words 0..5 as in the capture.
func (d *simDevice) soInitial() (w [6]uint64, param string) {
	m := reSOWords.FindStringSubmatch(string(d.cap.files["status-3.json"]))
	if m == nil {
		return w, strings.Repeat("00", 18)
	}
	for i := 0; i < 6; i++ {
		if i == 1 {
			param = m[2]
			continue
		}
		w[i], _ = strconv.ParseUint(m[i+1], 16, 32)
	}
	return w, param
}

// soLoc maps a SetOption number to its word index and bit (toggles) or
// param byte (32..49).
func soLoc(n int) (word, bit int, isParam bool, ok bool) {
	switch {
	case n >= 0 && n <= 31:
		return 0, n, false, true
	case n >= 32 && n <= 49:
		return 1, n - 32, true, true
	case n >= 50 && n <= 81:
		return 2, n - 50, false, true
	case n >= 82 && n <= 113:
		return 3, n - 82, false, true
	case n >= 114 && n <= 145:
		return 4, n - 114, false, true
	case n >= 146 && n <= 177:
		return 5, n - 146, false, true
	}
	return 0, 0, false, false
}

// settingFrom reads a setting by lower-case key: the changed value from kv,
// else the capture's. It only reads immutable device fields, so render can
// call it without the device lock.
func (d *simDevice) settingFrom(kv map[string]string, key string) string {
	if v, ok := kv[key]; ok {
		return v
	}
	if m := reSettingSO.FindStringSubmatch(key); m != nil {
		n, _ := strconv.Atoi(m[1])
		word, bit, isParam, ok := soLoc(n)
		if !ok {
			return "0"
		}
		w, param := d.soInitial()
		if isParam {
			if 2*bit+2 <= len(param) {
				b, _ := strconv.ParseUint(param[2*bit:2*bit+2], 16, 8)
				return strconv.FormatUint(b, 10)
			}
			return "0"
		}
		return strconv.FormatUint(w[word]>>bit&1, 10)
	}
	status0 := string(d.cap.files["status-0.json"])
	num := func(re string, def string) string {
		if m := regexp.MustCompile(re).FindStringSubmatch(status0); m != nil {
			return m[1]
		}
		return def
	}
	switch key {
	case "teleperiod":
		if m := regexp.MustCompile(`"TelePeriod":(\d+)`).FindStringSubmatch(string(d.cap.files["teleperiod.json"])); m != nil {
			return m[1]
		}
		return "300"
	case "poweronstate":
		return num(`"PowerOnState":(\d+)`, "0")
	case "savedata":
		return num(`"SaveData":(\d+)`, "0")
	case "timezone":
		if m := reTZ.FindStringSubmatch(status0); m != nil {
			return strings.Trim(m[1], `"`)
		}
		return "99"
	case "sleep":
		return "50"
	case "ledstate":
		return "1"
	case "hostname":
		return d.topic
	case "topic":
		return d.topic
	case "fulltopic":
		return "%prefix%/%topic%/"
	case "mqtthost":
		return num(`"MqttHost":"([^"]*)"`, "")
	case "mqttport":
		return num(`"MqttPort":(\d+)`, "1883")
	case "mqttuser":
		return num(`"MqttUser":"([^"]*)"`, "")
	case "mqttpassword":
		return ""
	case "ntpserver1":
		return "pool.ntp.org"
	case "ntpserver2":
		return "nl.pool.ntp.org"
	case "ntpserver3":
		return "0.nl.pool.ntp.org"
	case "timedst":
		return defTimeDst
	case "timestd":
		return defTimeStd
	}
	if strings.HasPrefix(key, "friendlyname") {
		n, _ := strconv.Atoi(strings.TrimPrefix(key, "friendlyname"))
		names := reFriendlyN.FindAllStringSubmatch(string(reFriendly.Find(d.cap.files["status-0.json"])), -1)
		// names[0] is the key itself
		if n >= 1 && n < len(names) {
			return names[n][1]
		}
		return ""
	}
	return ""
}

func ruleReply(key, v string) string {
	p := strings.Split(v, ",")
	if len(p) != 6 {
		return `{"` + key + `":{}}`
	}
	return fmt.Sprintf(`{"%s":{"Hemisphere":%s,"Week":%s,"Month":%s,"Day":%s,"Hour":%s,"Offset":%s}}`, key, p[0], p[1], p[2], p[3], p[4], p[5])
}

// normTimezone parses what Tasmota accepts and returns the stored form.
func normTimezone(v string) (string, bool) {
	if v == "99" {
		return "99", true
	}
	if h, err := strconv.Atoi(v); err == nil {
		if h < -13 || h > 14 {
			return "", false
		}
		return fmt.Sprintf("%+03d:00", h), true
	}
	var h, m int
	if _, err := fmt.Sscanf(v, "%d:%d", &h, &m); err != nil || h < -13 || h > 14 || m < 0 || m > 59 {
		return "", false
	}
	sign := "+"
	if strings.HasPrefix(v, "-") {
		sign = "-"
		h = -h
	}
	return fmt.Sprintf("%s%02d:%02d", sign, h, m), true
}

func parseRule(v string) (string, bool) {
	p := strings.Split(strings.ReplaceAll(v, " ", ""), ",")
	if len(p) != 6 {
		return "", false
	}
	lims := [6][2]int{{0, 1}, {0, 4}, {1, 12}, {1, 7}, {0, 23}, {-900, 900}}
	for i, s := range p {
		n, err := strconv.Atoi(s)
		if err != nil || n < lims[i][0] || n > lims[i][1] {
			return "", false
		}
		p[i] = strconv.Itoa(n)
	}
	return strings.Join(p, ","), true
}

func (d *simDevice) set(key, val string) { d.runtime.kv[key] = val }

func (d *simDevice) get(key string) string { return d.settingFrom(d.runtime.kv, key) }

// settingCmdLocked executes the settings commands; ok is false for
// everything else.
func (d *simDevice) settingCmdLocked(base string, idx int, hasIdx bool, data string) (body []byte, ok bool) {
	changed := false
	save := false
	switch base {
	case "teleperiod":
		if n, err := strconv.Atoi(data); err == nil && n >= 0 {
			switch {
			case n == 1:
				n = 300
			case n > 0 && n < 10:
				n = 10
			case n > 3600:
				n = 3600
			}
			d.set("teleperiod", strconv.Itoa(n))
			changed = true
		}
		body = []byte(`{"TelePeriod":` + d.get("teleperiod") + `}`)
	case "poweronstate":
		if n, err := strconv.Atoi(data); err == nil && n >= 0 && n <= 5 {
			d.set("poweronstate", strconv.Itoa(n))
			changed = true
		}
		body = []byte(`{"PowerOnState":` + d.get("poweronstate") + `}`)
	case "savedata":
		if n, err := strconv.Atoi(data); err == nil && n >= 0 && n <= 3600 {
			d.set("savedata", strconv.Itoa(n))
		}
		save = true // SaveData (also bare) calls SettingsSaveAll
		if v := d.get("savedata"); v == "0" {
			body = []byte(`{"SaveData":"Off"}`)
		} else {
			body = []byte(`{"SaveData":"` + v + ` seconds"}`)
		}
	case "sleep", "ledstate":
		// Plain numbers that no Status block reports: the way a generic policy rule (any
		// command answering a bare query) is read and set. Sleep 0..250, LedState 0..8.
		hi := map[string]int{"sleep": 250, "ledstate": 8}[base]
		if n, err := strconv.Atoi(data); err == nil && n >= 0 && n <= hi {
			d.set(base, strconv.Itoa(n))
			changed = true
		}
		name := map[string]string{"sleep": "Sleep", "ledstate": "LedState"}[base]
		body = []byte(`{"` + name + `":` + d.get(base) + `}`)
		if base == "sleep" {
			// Tasmota v16 CmndSleep: {"Sleep":{"<Settings->sleep>":{"Active":"<TasmotaGlobal.sleep>"}}}
			// (S_JSON_COMMAND_NVALUE_ACTIVE_NVALUE). The simulator has no dynamic sleep: both are equal.
			body = []byte(`{"Sleep":{"` + d.get(base) + `":{"Active":"` + d.get(base) + `"}}}`)
		}
	case "setoption":
		if !hasIdx {
			return nil, false
		}
		_, _, isParam, valid := soLoc(idx)
		if !valid {
			return errorReply("SETOPTION" + strconv.Itoa(idx)), true
		}
		key := "setoption" + strconv.Itoa(idx)
		if data != "" {
			v := strings.ToLower(data)
			switch {
			case isParam:
				if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 255 {
					d.set(key, strconv.Itoa(n))
					changed = true
				}
			case v == "0" || v == "off":
				d.set(key, "0")
				changed = true
			case v == "1" || v == "on":
				d.set(key, "1")
				changed = true
			}
		}
		v := d.get(key)
		switch {
		case isParam:
			body = []byte(`{"SetOption` + strconv.Itoa(idx) + `":` + v + `}`)
		case v == "1":
			body = []byte(`{"SetOption` + strconv.Itoa(idx) + `":"ON"}`)
		default:
			body = []byte(`{"SetOption` + strconv.Itoa(idx) + `":"OFF"}`)
		}
	case "timezone":
		if data != "" {
			if v, valid := normTimezone(data); valid {
				d.set("timezone", v)
				changed = true
			}
		}
		if v := d.get("timezone"); v == "99" {
			body = []byte(`{"Timezone":99}`)
		} else {
			body = []byte(`{"Timezone":` + jsonString(v) + `}`)
		}
	case "timedst", "timestd":
		if data != "" {
			if v, valid := parseRule(data); valid {
				d.set(base, v)
				changed = true
			}
		}
		key := "TimeDst"
		if base == "timestd" {
			key = "TimeStd"
		}
		body = []byte(ruleReply(key, d.get(base)))
	case "hostname", "topic", "fulltopic", "mqtthost", "mqttuser", "mqttport":
		if data != "" && data != "0" && data != "1" && (base != "mqttport" || isPort(data)) {
			d.set(base, data)
			changed = true
		}
		names := map[string]string{"hostname": "Hostname", "topic": "Topic", "fulltopic": "FullTopic", "mqtthost": "MqttHost", "mqttuser": "MqttUser", "mqttport": "MqttPort"}
		if base == "mqttport" {
			body = []byte(`{"MqttPort":` + d.get(base) + `}`)
		} else {
			body = []byte(`{"` + names[base] + `":` + jsonString(d.get(base)) + `}`)
		}
	case "mqttpassword":
		if data != "" {
			d.set("mqttpassword", data)
			changed = true
		}
		body = []byte(`{"MqttPassword":"****"}`) // never echoed (index 2 form)
	case "friendlyname", "ntpserver":
		if !hasIdx || idx < 1 || idx > map[string]int{"friendlyname": 4, "ntpserver": 3}[base] {
			return nil, false
		}
		key := base + strconv.Itoa(idx)
		if data != "" && data != "0" && data != "1" {
			d.set(key, data)
			changed = true
		}
		name := "FriendlyName"
		if base == "ntpserver" {
			name = "NtpServer"
		}
		body = []byte(`{"` + name + strconv.Itoa(idx) + `":` + jsonString(d.get(key)) + `}`)
	default:
		return nil, false
	}
	if changed && restartKeys[base] {
		save = true
		if !d.restartQueued && d.phase == PhaseRun {
			d.restartQueued = true
			d.spawn(d.restartSoon)
		}
	}
	if changed && d.get("savedata") != "0" {
		save = true // periodic saving, approximated as immediate
	}
	if save {
		d.commitLocked()
	}
	return body, true
}

func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}

// applySettings rewrites the captured text so changed settings show up in
// Status replies. Unchanged settings (empty kv) leave the capture untouched.
func (d *simDevice) applySettings(s string, kv map[string]string) string {
	if len(kv) == 0 {
		return s
	}
	has := func(k string) bool { _, ok := kv[k]; return ok }
	if has("teleperiod") {
		s = reTeleP.ReplaceAllLiteralString(s, `"TelePeriod":`+kv["teleperiod"])
	}
	if has("poweronstate") {
		s = rePowerOn.ReplaceAllLiteralString(s, `"PowerOnState":`+kv["poweronstate"])
	}
	if has("savedata") {
		s = reSaveData.ReplaceAllLiteralString(s, `"SaveData":`+kv["savedata"])
	}
	if has("timezone") {
		tz := kv["timezone"]
		if tz != "99" {
			tz = jsonString(tz)
		}
		s = reTZ.ReplaceAllLiteralString(s, `"Timezone":`+tz)
	}
	if has("hostname") {
		s = reHostname.ReplaceAllLiteralString(s, `"Hostname":`+jsonString(kv["hostname"]))
	}
	if has("topic") {
		s = reTopicKey.ReplaceAllLiteralString(s, `"Topic":`+jsonString(kv["topic"]))
	}
	if has("mqtthost") {
		s = reMqttHost.ReplaceAllLiteralString(s, `"MqttHost":`+jsonString(kv["mqtthost"]))
	}
	if has("mqttport") {
		s = reMqttPort.ReplaceAllLiteralString(s, `"MqttPort":`+kv["mqttport"])
	}
	if has("mqttuser") {
		s = reMqttUser.ReplaceAllLiteralString(s, `"MqttUser":`+jsonString(kv["mqttuser"]))
	}
	if reFriendly.MatchString(s) {
		var names []string
		n := len(reFriendlyN.FindAllString(string(reFriendly.Find(d.cap.files["status-0.json"])), -1)) - 1
		for i := 1; i <= max(n, len(d.spec.FriendlyNames), 1); i++ {
			names = append(names, jsonString(d.settingFrom(kv, "friendlyname"+strconv.Itoa(i))))
		}
		s = reFriendly.ReplaceAllLiteralString(s, `"FriendlyName":[`+strings.Join(names, ",")+`]`)
	}
	if reSOArray.MatchString(s) && d.soChanged(kv) {
		w, param := d.soInitial()
		b := []byte(param)
		for k, v := range kv {
			m := reSettingSO.FindStringSubmatch(k)
			if m == nil {
				continue
			}
			n, _ := strconv.Atoi(m[1])
			word, bit, isParam, ok := soLoc(n)
			val, _ := strconv.ParseUint(v, 10, 8)
			switch {
			case !ok:
			case isParam:
				copy(b[2*bit:], fmt.Sprintf("%02X", val))
			case val == 1:
				w[word] |= 1 << bit
			default:
				w[word] &^= 1 << bit
			}
		}
		arr := fmt.Sprintf(`"SetOption":["%08X","%s","%08X","%08X","%08X","%08X"]`, w[0], string(b), w[2], w[3], w[4], w[5])
		s = reSOArray.ReplaceAllLiteralString(s, arr)
	}
	return s
}

func (d *simDevice) soChanged(kv map[string]string) bool {
	for k := range kv {
		if reSettingSO.MatchString(k) {
			return true
		}
	}
	return false
}

// dumpMagic starts every simulated config dump. The real .dmp format is a
// binary Settings struct that only decode-config can read; the simulator
// writes the settings an operator cares about as JSON behind this magic,
// padded to 4096 bytes, and DecodeDump (used by the fake decode-config)
// turns it into decode-config style JSON.
const dumpMagic = "TFSIMDMP1\n"

// dumpLocked renders the RAM settings like /dl does.
func (d *simDevice) dumpLocked() []byte {
	kv := d.runtime.kv
	g := func(k string) string { return d.settingFrom(kv, k) }
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	flag := func(n int) int { return atoi(g("setoption" + strconv.Itoa(n))) }
	tz := g("timezone")
	hours, mins := 99, 0
	if tz != "99" {
		var h, m int
		_, _ = fmt.Sscanf(tz, "%d:%d", &h, &m)
		hours, mins = h, m
	}
	dst, std := strings.Split(g("timedst"), ","), strings.Split(g("timestd"), ",")
	names := []string{}
	for i := 1; i <= 4; i++ {
		names = append(names, g("friendlyname"+strconv.Itoa(i)))
	}
	doc := map[string]any{
		"header":      map[string]any{"data": map[string]any{"version": map[string]any{"name": d.version + ".0"}, "size": 4096}},
		"tele_period": atoi(g("teleperiod")), "poweronstate": atoi(g("poweronstate")), "save_data": atoi(g("savedata")),
		"hostname": g("hostname"), "mqtt_topic": g("topic"), "mqtt_fulltopic": g("fulltopic"),
		"mqtt_host": g("mqtthost"), "mqtt_port": atoi(g("mqttport")), "mqtt_user": g("mqttuser"), "mqtt_pwd": g("mqttpassword"),
		"friendlyname": names, "ntp_server": []string{g("ntpserver1"), g("ntpserver2"), g("ntpserver3")},
		"timezone": hours, "timezone_minutes": mins,
		"tflag": []any{map[string]any{"hemis": atoi(dst[0]), "week": atoi(dst[1]), "month": atoi(dst[2]), "dow": atoi(dst[3]), "hour": atoi(dst[4])},
			map[string]any{"hemis": atoi(std[0]), "week": atoi(std[1]), "month": atoi(std[2]), "dow": atoi(std[3]), "hour": atoi(std[4])}},
		"toffset": []int{atoi(dst[5]), atoi(std[5])},
		"ota_url": d.runtime.otaURL,
		"flag":    map[string]any{"save_state": flag(0), "mqtt_enabled": flag(3), "mqtt_response": flag(4), "hass_discovery": flag(19)},
	}
	b, err := json.Marshal(doc)
	if err != nil { // plain maps of scalars always encode
		return nil
	}
	out := append([]byte(dumpMagic), b...)
	for len(out) < 4096 {
		out = append(out, 0)
	}
	return out
}

// DecodeDump is the fake decode-config: it turns a dump written by a
// simulated device back into decode-config style JSON (keys as in the real
// decoder's output for the settings the simulator models).
func DecodeDump(dump []byte) ([]byte, error) {
	if len(dump) < len(dumpMagic) || string(dump[:len(dumpMagic)]) != dumpMagic {
		return nil, errors.New("not a simulator config dump")
	}
	body := dump[len(dumpMagic):]
	end := len(body)
	for end > 0 && body[end-1] == 0 {
		end--
	}
	var doc map[string]any
	if err := json.Unmarshal(body[:end], &doc); err != nil {
		return nil, fmt.Errorf("simulator dump: %w", err)
	}
	return json.MarshalIndent(doc, "", "  ")
}

// FakeDecoderVersion is what the fake decode-config reports for --version.
const FakeDecoderVersion = "decode-config v16.0.0.0 (tasmofleet simulator)"
