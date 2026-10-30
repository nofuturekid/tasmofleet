// Package devicecmd sends single commands to single devices on behalf of a
// user: the power toggle and the console. Every send is checked by
// cmdpolicy before any network I/O and audited with secrets redacted.
//
// Bulk commands (M2-4) and the settings editor (M2-2) have their own
// services; they consult cmdpolicy too but do not go through here.
package devicecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Errors. Callers match with errors.Is; cmdpolicy's errors (ErrInvalid,
// ErrForbidden, ErrRole) are returned unchanged by Run.
var (
	ErrBadAction   = errors.New("devicecmd: power action must be ON, OFF or TOGGLE")
	ErrNoRelay     = errors.New("devicecmd: the device has no such relay")
	ErrUnconfirmed = errors.New("devicecmd: command published over MQTT but the device did not confirm it in time")
)

// maxRelay is the highest relay index offered (Tasmota's usual limit).
const maxRelay = 8

// Service is the device command API used by the web layer.
type Service interface {
	// Power switches relay (1-based) to action (ON, OFF or TOGGLE) and returns
	// the state the device reported ("ON"/"OFF").
	Power(ctx context.Context, actor string, id model.DeviceID, relay int, action string) (state string, err error)
	// Run sends one free-form command after checking it against cmdpolicy for
	// the principal's role. It returns the device's JSON reply.
	Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error)
}

// DeviceGetter reads inventory devices (satisfied by inventory.Service).
type DeviceGetter interface {
	Get(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// PowerObserver is told the state a device confirmed to a relay command sent over HTTP
// (satisfied by health.Service).
type PowerObserver interface {
	NotePower(id model.DeviceID, relay int, state string)
}

// Options configure the service.
type Options struct {
	Devices DeviceGetter
	Client  tasmota.Client
	Audit   audit.Recorder
	// MQTT is optional. It is used for Power only when the device's MQTT
	// capability is MQTTOurs and the connection is usable. The command channel
	// serialises per device and waits for the reply (8 s by default).
	MQTT  mqttcmd.Doer
	Clock clock.Clock
	// Health is optional; see PowerObserver.
	Health PowerObserver
}

type service struct{ o Options }

// New returns the service. Devices, Client and Audit are required.
func New(o Options) Service {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	return &service{o: o}
}

func target(id model.DeviceID) string { return "device:" + strconv.FormatInt(int64(id), 10) }

func (s *service) record(ctx context.Context, actor, action string, id model.DeviceID, detail map[string]any, err error) {
	res := "ok"
	if err != nil {
		res = err.Error()
	}
	b, merr := json.Marshal(detail)
	if merr != nil {
		b = []byte(`{}`)
	}
	s.o.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: action, Target: target(id), Detail: string(b), Result: res})
}

func (s *service) Power(ctx context.Context, actor string, id model.DeviceID, relay int, action string) (string, error) {
	action = strings.ToUpper(strings.TrimSpace(action))
	switch action {
	case "ON", "OFF", "TOGGLE":
	default:
		return "", ErrBadAction
	}
	if relay < 1 || relay > maxRelay {
		return "", fmt.Errorf("%w: relay %d (valid: 1 to %d)", ErrNoRelay, relay, maxRelay)
	}
	// Built from validated values, and still judged by the same policy on
	// both paths (MQTT publishes the same name and action).
	if v, cerr := cmdpolicy.Classify(fmt.Sprintf("Power%d %s", relay, action)); cerr != nil || v.Class != cmdpolicy.ClassOperator {
		return "", fmt.Errorf("devicecmd: power command is not operator-level: %w", cmdpolicy.ErrForbidden)
	}
	d, err := s.o.Devices.Get(ctx, id)
	if err != nil {
		return "", err
	}
	via := "http"
	var state string
	if s.useMQTT(d) {
		via = "mqtt"
		state, err = s.powerMQTT(ctx, d, relay, action)
	} else {
		state, err = s.powerHTTP(ctx, d, relay, action)
		if err == nil && s.o.Health != nil {
			s.o.Health.NotePower(id, relay, state)
		}
	}
	s.record(ctx, actor, "device.power", id,
		map[string]any{"relay": relay, "action": action, "via": via, "state": state}, err)
	return state, err
}

func (s *service) useMQTT(d model.Device) bool {
	return s.o.MQTT != nil && d.MQTT == model.MQTTOurs && d.Topic != "" && s.o.MQTT.Connected()
}

func (s *service) powerHTTP(ctx context.Context, d model.Device, relay int, action string) (string, error) {
	raw, err := s.o.Client.Command(ctx, tasmota.TargetFor(d), fmt.Sprintf("Power%d %s", relay, action))
	if err != nil {
		return "", err
	}
	return powerState(raw, relay)
}

// powerState reads the relay state out of a Power reply ({"POWER1":"ON"}).
func powerState(raw json.RawMessage, relay int) (string, error) {
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		return "", fmt.Errorf("devicecmd: unreadable power reply: %w", tasmota.ErrBadResponse)
	}
	// A one-relay device names its relay POWER, a multi-relay device POWERn.
	keys := []string{"POWER" + strconv.Itoa(relay)}
	if relay == 1 {
		keys = append(keys, "POWER")
	}
	for _, k := range keys {
		var v string
		if json.Unmarshal(reply[k], &v) == nil && v != "" {
			return strings.ToUpper(v), nil
		}
	}
	return "", fmt.Errorf("%w: relay %d", ErrNoRelay, relay)
}

// powerMQTT goes through the command channel: one command per device at a time, the reply is the
// RESULT that names the relay, and a button press or a retained report is never taken for it.
func (s *service) powerMQTT(ctx context.Context, d model.Device, relay int, action string) (string, error) {
	raw, err := s.o.MQTT.Do(ctx, d, fmt.Sprintf("Power%d %s", relay, action))
	if errors.Is(err, tasmota.ErrTimeout) {
		return "", fmt.Errorf("%w (relay %d)", ErrUnconfirmed, relay)
	}
	if err != nil {
		return "", err
	}
	state, err := powerState(raw, relay)
	if err != nil {
		return "", err
	}
	if (action == "ON" || action == "OFF") && state != action {
		// A reply that disagrees with the request is not its confirmation (a button press, a late reply).
		return "", fmt.Errorf("%w (relay %d reported %s, asked %s)", ErrUnconfirmed, relay, state, action)
	}
	return state, nil
}

func (s *service) Run(ctx context.Context, p model.Principal, id model.DeviceID, cmd string) (json.RawMessage, error) {
	// The role check lives here, not in the callers. What is classified is
	// exactly what is sent: Prepare returns the canonical text.
	send, v, err := cmdpolicy.Prepare(cmd, p.Role, cmdpolicy.PrepareOpts{})
	// Redacted shows arguments only for known-harmless commands: a refused
	// command may hold a secret, directly or wrapped in another command.
	if err != nil {
		s.record(ctx, p.Subject, "command.rejected", id, map[string]any{"command": cmdpolicy.Redacted(cmd), "class": v.Class.String()}, err)
		return nil, err
	}
	logged := cmdpolicy.Redacted(send)
	d, err := s.o.Devices.Get(ctx, id)
	if err != nil {
		s.record(ctx, p.Subject, "command.send", id, map[string]any{"command": logged, "class": v.Class.String()}, err)
		return nil, err
	}
	// The console is typed by a person: a reply with other keys than the command's (Template) is the answer.
	cctx, trace := transport.WithTrace(transport.WithConsole(ctx))
	raw, err := s.o.Client.Command(cctx, tasmota.TargetFor(d), send)
	s.record(ctx, p.Subject, "command.send", id, map[string]any{"command": logged, "class": v.Class.String(), "via": trace.Via()}, err)
	if err != nil {
		return nil, err
	}
	// Replies can carry credentials (MQTT user, URLs with userinfo, rules).
	return json.RawMessage(cmdpolicy.ScrubReply(raw)), nil
}
