package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Errors. Callers match with errors.Is / errors.As.
var (
	ErrUnknownField = errors.New("settings: unknown field")
	ErrNotQueryable = errors.New("settings: command cannot be read")
)

// ValidationError lists every rejected field of one Apply call. When it is
// returned nothing was sent to the device.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "settings: rejected: " + strings.Join(parts, "; ")
}

// Is makes errors.Is(err, ErrInvalid) true for validation failures.
func (e *ValidationError) Is(target error) bool { return target == ErrInvalid }

// Result is the outcome of one field. Command is the redacted command text
// (secret values are "***"); Err is a short sentence, never raw device output.
type Result struct {
	Key     string
	Command string
	Err     string
}

// OK reports whether the device accepted the command.
func (r Result) OK() bool { return r.Err == "" }

// Service reads and writes the curated settings of one device.
type Service interface {
	// Read queries every non-secret field, one command at a time. Fields the
	// firmware does not know are left out. Values are normalised: ON/OFF
	// become 1/0.
	Read(ctx context.Context, id model.DeviceID) (map[string]string, error)
	// ReadKeys queries only the given commands (curated fields or SetOptionN).
	ReadKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error)
	// Apply validates all changes first (nothing is sent when one is
	// invalid), then sends one command per field, in form order.
	Apply(ctx context.Context, actor string, id model.DeviceID, changes map[string]string) ([]Result, error)
	// Save persists RAM settings with one explicit SaveData (see Manager.Save).
	Save(ctx context.Context, actor string, id model.DeviceID, saveData string) (Result, error)
}

// DeviceGetter is the part of the store the service needs.
type DeviceGetter interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// Options configure the service.
type Options struct {
	Devices     DeviceGetter
	Client      tasmota.Client
	Audit       audit.Recorder // optional
	ReadTimeout time.Duration  // whole Read, default 60 s
}

// Manager implements Service.
type Manager struct{ o Options }

var _ Service = (*Manager)(nil)

// New returns the settings service.
func New(o Options) *Manager {
	if o.ReadTimeout == 0 {
		o.ReadTimeout = 60 * time.Second
	}
	return &Manager{o: o}
}

// Read implements Service.
func (m *Manager) Read(ctx context.Context, id model.DeviceID) (map[string]string, error) {
	var keys []string
	for _, f := range Fields {
		if !f.Secret && !f.PolicyOnly {
			keys = append(keys, f.Key)
		}
	}
	return m.readKeys(ctx, id, keys)
}

// ReadKeys implements Service.
func (m *Manager) ReadKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	for _, k := range keys {
		if !Queryable(k) {
			return nil, fmt.Errorf("%w: %q", ErrNotQueryable, k)
		}
		if f, ok := Lookup(k); ok && f.Secret {
			return nil, fmt.Errorf("%w: %q is write-only", ErrNotQueryable, k)
		}
	}
	return m.readKeys(ctx, id, keys)
}

func (m *Manager) readKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error) {
	d, err := m.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.o.ReadTimeout)
	defer cancel()
	t := tasmota.TargetFor(d)
	out := make(map[string]string, len(keys))
	statusReplies := map[string]map[string]json.RawMessage{} // Status command -> parsed sections
	for _, k := range keys {
		if src, ok := statusSource[k]; ok {
			sections, done := statusReplies[src.cmd]
			if !done {
				raw, err := m.o.Client.Command(ctx, t, src.cmd)
				if err != nil && !errors.Is(err, tasmota.ErrUnknownCommand) {
					return out, fmt.Errorf("read %s: %w", src.cmd, err)
				}
				if err == nil {
					_ = json.Unmarshal(raw, &sections) // unparsable: sections stays nil, values stay unknown
				}
				statusReplies[src.cmd] = sections
			}
			if v, ok := statusValue(sections, src.section, src.field); ok {
				out[k] = v
			}
			continue
		}
		if slices.Contains(SideEffectQueries, k) {
			return out, fmt.Errorf("%w: %q has a side effect when queried bare", ErrNotQueryable, k)
		}
		if !Queryable(k) { // right before the send: whatever the caller checked, a denied name never leaves
			return out, fmt.Errorf("%w: %q", ErrNotQueryable, k)
		}
		raw, err := m.o.Client.Command(ctx, t, k) // one at a time; a bare name queries
		switch {
		case errors.Is(err, tasmota.ErrUnknownCommand):
			continue
		case err != nil:
			return out, fmt.Errorf("read %s: %w", k, err)
		}
		if Generic(k) {
			raw = cmdpolicy.ScrubReply(raw) // an uncurated reply may hold anything
		}
		v, ok := replyValue(k, raw)
		if !ok {
			continue // an unparsable reply is "unknown", never a guess
		}
		out[k] = safeValue(k, v)
	}
	return out, nil
}

// safeValue keeps URL credentials out of what is read from a device: from the
// curated URL and from every generic value.
func safeValue(key, v string) string {
	if key == "OtaUrl" || Generic(key) {
		return RedactURLCredentials(v)
	}
	return v
}

func statusValue(sections map[string]json.RawMessage, section, field string) (string, bool) {
	raw, ok := sections[section]
	if !ok {
		return "", false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	val, ok := obj[field]
	if !ok {
		return "", false
	}
	return scalar(val)
}

// replyValue extracts the value of a reply like {"Topic":"x"} to the query key and
// normalises it: ON/OFF to 1/0, TimeDst objects to "H,W,M,D,h,off".
func replyValue(key string, raw json.RawMessage) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || len(obj) != 1 {
		return "", false
	}
	for k, val := range obj {
		if !strings.EqualFold(k, key) { // e.g. {"Command":"ok"} answers nothing we asked
			return "", false
		}
		return replyScalar(key, val)
	}
	return "", false
}

// activeReplyCommands answer with S_JSON_COMMAND_NVALUE_ACTIVE_NVALUE in Tasmota v16
// (support_command.ino: CmndSleep, CmndSeriallog, CmndSyslog). Every other command is read as is.
var activeReplyCommands = []string{"Sleep", "SerialLog", "SysLog"}

// replyScalar is scalar plus the one reply shape that is not a scalar: configured/active.
func replyScalar(cmd string, val json.RawMessage) (string, bool) {
	if slices.ContainsFunc(activeReplyCommands, func(c string) bool { return strings.EqualFold(c, cmd) }) {
		if v, ok := sleepConfigured(val); ok {
			return v, true
		}
	}
	return scalar(val)
}

// sleepConfigured reads the configured value out of Tasmota's Sleep reply (SerialLog and SysLog use
// the same format). Sleep answers
// {"Sleep":{"50":{"Active":"100"}}}: the object key is Settings->sleep (what a rule sets), "Active"
// the value in effect now (it differs while dynamic sleep adapts). Only the configured one is
// comparable with a rule; any other shape is not this reply.
func sleepConfigured(val json.RawMessage) (string, bool) {
	var outer map[string]map[string]json.RawMessage
	if json.Unmarshal(val, &outer) != nil || len(outer) != 1 {
		return "", false
	}
	for k, inner := range outer {
		if _, ok := inner["Active"]; !ok || len(inner) != 1 {
			return "", false
		}
		if n, err := strconv.Atoi(k); err != nil || n < 0 || strconv.Itoa(n) != k {
			return "", false
		}
		return k, true
	}
	return "", false
}

func scalar(val json.RawMessage) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(val))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		switch strings.ToUpper(t) {
		case "ON":
			return "1", true
		case "OFF":
			return "0", true
		}
		return t, true
	case json.Number:
		return t.String(), true
	case map[string]any:
		return ruleString(t)
	}
	return "", false
}

// ruleString renders {"Hemisphere":0,"Week":0,"Month":3,"Day":1,"Hour":2,"Offset":120}.
func ruleString(m map[string]any) (string, bool) {
	var parts []string
	for _, k := range []string{"Hemisphere", "Week", "Month", "Day", "Hour", "Offset"} {
		n, ok := m[k].(json.Number)
		if !ok {
			return "", false
		}
		parts = append(parts, n.String())
	}
	return strings.Join(parts, ","), true
}

// Apply implements Service.
func (m *Manager) Apply(ctx context.Context, actor string, id model.DeviceID, changes map[string]string) ([]Result, error) {
	var todo []Field
	bad := map[string]string{}
	for k := range changes {
		if _, ok := Lookup(k); !ok {
			return nil, fmt.Errorf("%w: %q", ErrUnknownField, k)
		}
	}
	for _, f := range Fields {
		v, ok := changes[f.Key]
		if !ok {
			continue
		}
		if f.PolicyOnly {
			bad[f.Key] = "can only be set by the fleet policy"
			continue
		}
		if err := checkText(v, MaxValueLen); err != nil {
			bad[f.Key] = trimInvalid(err)
			continue
		}
		if err := f.Validate(v); err != nil {
			bad[f.Key] = trimInvalid(err)
			continue
		}
		todo = append(todo, f)
	}
	if len(bad) > 0 {
		return nil, &ValidationError{Fields: bad}
	}
	// Commands that restart the device go last: a later command would race the reboot.
	sort.SliceStable(todo, func(i, j int) bool { return !RestartsDevice(todo[i].Key) && RestartsDevice(todo[j].Key) })
	d, err := m.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	t := tasmota.TargetFor(d)
	results := make([]Result, 0, len(todo))
	for i, f := range todo {
		cmd := f.Command(changes[f.Key])
		shown := cmd
		if f.Secret {
			shown = f.Key + " ***"
		}
		r := Result{Key: f.Key, Command: shown}
		_, err := m.o.Client.Command(ctx, t, cmd)
		if err != nil {
			r.Err = errorText(err)
		}
		results = append(results, r)
		m.audit(ctx, actor, id, r)
		if err != nil && (errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrAuth)) {
			for _, rest := range todo[i+1:] { // do not queue slow failures behind a dead device
				results = append(results, Result{Key: rest.Key, Command: rest.Key + " (not sent)", Err: "not sent: the device did not answer"})
			}
			break
		}
	}
	return results, nil
}

// Save writes the settings held in RAM to flash with one explicit
// "SaveData <current value>": Tasmota's SaveData handler calls
// SettingsSaveAll() whatever the value is, and re-sending the current value
// leaves the policy value untouched. Needed when SaveData is 0, where nothing
// else saves.
func (m *Manager) Save(ctx context.Context, actor string, id model.DeviceID, saveData string) (Result, error) {
	if err := intRange(0, 3600)(saveData); err != nil {
		return Result{}, &ValidationError{Fields: map[string]string{"SaveData": trimInvalid(err)}}
	}
	d, err := m.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return Result{}, err
	}
	cmd := "SaveData " + saveData
	r := Result{Key: "SaveData", Command: cmd}
	if _, err := m.o.Client.Command(ctx, tasmota.TargetFor(d), cmd); err != nil {
		r.Err = errorText(err)
	}
	m.audit(ctx, actor, id, r)
	return r, nil
}

func trimInvalid(err error) string { return strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": ") }

func errorText(err error) string {
	switch {
	case errors.Is(err, tasmota.ErrUnknownCommand):
		return "the firmware does not know this command"
	case errors.Is(err, tasmota.ErrAuth):
		return "the device refused the stored credentials"
	case errors.Is(err, tasmota.ErrUnreachable):
		return "the device is unreachable"
	case errors.Is(err, tasmota.ErrTimeout):
		return "the device did not answer in time"
	}
	return "the device answered with an error"
}

func (m *Manager) audit(ctx context.Context, actor string, id model.DeviceID, r Result) {
	if m.o.Audit == nil {
		return
	}
	detail, _ := json.Marshal(map[string]string{"command": r.Command}) // a string map always encodes
	res := "ok"
	if r.Err != "" {
		res = r.Err
	}
	m.o.Audit.Record(ctx, model.AuditEvent{
		Actor: actor, Action: "settings.set", Target: "device:" + strconv.FormatInt(int64(id), 10),
		Detail: string(detail), Result: res,
	})
}
