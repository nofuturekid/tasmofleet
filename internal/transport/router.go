package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// DefaultFailFor is how long a failed HTTP call keeps the device's HTTP state "failing" (D4).
const DefaultFailFor = 10 * time.Minute

// ErrCutOff is returned for a write over MQTT that can cut the device off (a new topic, broker or login)
// when no person confirmed it (WithCutOffConsent). Nothing was published.
var ErrCutOff = errors.New("not sent")

// DeviceGetter reads one inventory device.
type DeviceGetter interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// DeviceFacts gathers Facts from a device snapshot the caller holds, so the facts and the send
// look at the same device. HTTPState is the cheap part (memory and health, no store).
type DeviceFacts interface {
	FactsFor(ctx context.Context, d model.Device) (Facts, error)
	HTTPState(id model.DeviceID) HTTPState
}

// MQTTDoer is the command channel (mqttcmd.Channel) as the router uses it.
type MQTTDoer interface {
	Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	Connected() bool
}

// Options configure the Router.
type Options struct {
	HTTP    tasmota.Client // required: the raw HTTP client
	Devices DeviceGetter   // required
	Facts   DeviceFacts    // set later with SetFacts: the facts need the health service
	Memory  *Memory        // optional; NewMemory otherwise
	Log     *slog.Logger
}

// Router implements tasmota.Client over two transports (D1). HTTP stays first; MQTT carries a call
// when HTTP is known to fail (Choose) or when the HTTP request provably never left (D4). Calls
// without a device (Target ID 0: scans, MAC verifiers) and passes of Uploader and BootSwitcher go
// to HTTP untouched.
type Router struct {
	o    Options
	http tasmota.Client // the raw client, recording what HTTP tells about the device
	mqtt atomic.Pointer[mqttHolder]
}

type mqttHolder struct{ d MQTTDoer }

var (
	_ tasmota.Client       = (*Router)(nil)
	_ tasmota.Uploader     = (*Router)(nil)
	_ tasmota.BootSwitcher = (*Router)(nil)
)

// NewRouter returns a router that uses HTTP only until SetMQTT is called.
func NewRouter(o Options) *Router {
	if o.Memory == nil {
		o.Memory = NewMemory(DefaultFailFor, time.Now)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Router{o: o, http: &observer{c: o.HTTP, mem: o.Memory}}
}

// SetMQTT attaches the command channel, once the broker connection exists. nil detaches it.
func (r *Router) SetMQTT(d MQTTDoer) {
	if d == nil {
		r.mqtt.Store(nil)
		return
	}
	r.mqtt.Store(&mqttHolder{d})
}

// SetFacts attaches the source of the facts. Until it is set (and MQTT attached) the router is the HTTP
// client it wraps. Call it before SetMQTT.
func (r *Router) SetFacts(f DeviceFacts) { r.o.Facts = f }

// Memory is the router's record of failing HTTP calls (app.HTTPMemory).
func (r *Router) Memory() *Memory { return r.o.Memory }

// HTTPOnly is the raw HTTP client with the router's memory attached: what it learns (a refused
// connection, an answer) is remembered, but it never routes to MQTT. The health poll, the prober
// and the discovery scan use it, because they measure or verify HTTP.
func (r *Router) HTTPOnly() tasmota.Client { return r.http }

func (r *Router) mqttDoer() MQTTDoer {
	if h := r.mqtt.Load(); h != nil && h.d.Connected() {
		return h.d
	}
	return nil
}

// ---- context

type ctxKey int

const (
	keyPath ctxKey = iota
	keyTrace
	keyLoose
	keyConsent
)

// WithPath pins the transport of every call made with ctx (D5): a job that chose its path (OTA,
// restore) is never rerouted and never fails over. Only HTTP and MQTT can be pinned.
func WithPath(ctx context.Context, p Path) context.Context {
	if p != HTTP && p != MQTT {
		return ctx
	}
	return context.WithValue(ctx, keyPath, p)
}

// PathFrom returns the pinned path of ctx.
func PathFrom(ctx context.Context) (Path, bool) {
	p, ok := ctx.Value(keyPath).(Path)
	return p, ok
}

// WithConsole marks calls typed by a person: over MQTT a reply with other keys than the command's
// name (Template, Gpios) is accepted as the answer (mqttcmd DoLoose).
func WithConsole(ctx context.Context) context.Context { return context.WithValue(ctx, keyLoose, true) }

// WithCutOffConsent marks calls a person confirmed after being told that the device is reached over MQTT
// only and that the change can cut TasmoFleet off from it (the console's confirmation). Without it the
// router publishes no such write over MQTT. Make compliant and bulk commands never carry it.
func WithCutOffConsent(ctx context.Context) context.Context {
	return context.WithValue(ctx, keyConsent, true)
}

// HasCutOffConsent reports whether ctx carries WithCutOffConsent.
func HasCutOffConsent(ctx context.Context) bool { c, _ := ctx.Value(keyConsent).(bool); return c }

// Trace records which transports carried the calls made under its context, for the audit "via".
type Trace struct {
	mu         sync.Mutex
	http, mqtt bool
	refused    bool // a call was stopped before anything was sent
}

// WithTrace returns a context whose calls are recorded in the returned Trace.
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, keyTrace, t), t
}

func (t *Trace) note(p Path) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch p {
	case MQTT:
		t.mqtt = true
	case None:
		t.refused = true
	case HTTP, DownloadHTTP, DownloadMQTT, PushHTTP, PushSafeboot, PushMQTT:
		t.http = true
	}
}

// Via is the audit value of "via": "http", "mqtt", or "http+mqtt" when a job used both; "none" when
// every call was stopped before anything was sent. Nothing recorded at all (no router involved) reads
// as "http", the default path.
func (t *Trace) Via() string {
	if t == nil {
		return "http"
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.http && t.mqtt:
		return "http+mqtt"
	case t.mqtt:
		return "mqtt"
	case t.http:
		return "http"
	case t.refused:
		return "none"
	}
	return "http"
}

func noteTrace(ctx context.Context, p Path) {
	if t, _ := ctx.Value(keyTrace).(*Trace); t != nil {
		t.note(p)
	}
}

// ---- classification

type cmdClass struct {
	write  bool // changes the device (everything but a read): follows Paths.Writes
	job    bool // starts a firmware upgrade or a restore
	redact bool // the reply may echo a credential
}

func classifyCommand(cmnd string) cmdClass {
	v, err := cmdpolicy.Classify(cmnd)
	c := cmdClass{write: err != nil || v.Class != cmdpolicy.ClassRead, redact: v.Redact || err != nil}
	c.job = startsJob(cmnd)
	return c
}

// startsJob reports an Upgrade or WebGetConfig with an argument, alone or in a Backlog. The bare
// Upgrade is the probe the engine sends and changes nothing. OtaUrl is a setting and only
// prepares an upgrade.
func startsJob(cmnd string) bool {
	for _, part := range strings.Split(cmnd, ";") {
		part = strings.TrimSpace(part)
		for len(part) >= len("backlog") && strings.EqualFold(part[:len("backlog")], "backlog") {
			part = strings.TrimLeft(strings.TrimSpace(part[len("backlog"):]), "0123456789 ")
		}
		name, args, _ := strings.Cut(part, " ")
		base := strings.ToLower(strings.TrimRight(name, "0123456789"))
		if (base == "upgrade" || base == "webgetconfig") && strings.TrimSpace(args) != "" {
			return true
		}
	}
	return false
}

// ---- Command

// Command sends one command, over the transport the facts allow.
func (r *Router) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	id := t.DeviceID()
	if id == 0 {
		return r.http.Command(ctx, t, cmnd) // scans and address checks verify HTTP itself
	}
	cls := classifyCommand(cmnd)
	if pin, ok := PathFrom(ctx); ok {
		if pin == HTTP {
			return r.httpCommand(ctx, t, cmnd)
		}
		return r.pinnedMQTT(ctx, id, cmnd, cls)
	}
	if r.mqttDoer() == nil || r.o.Facts.HTTPState(id).State != HTTPFailing {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	// HTTP is known to fail: ask the facts which way works.
	d, f, p, err := r.decide(ctx, id)
	if err != nil {
		return r.httpFirst(ctx, t, cmnd, cls) // no judgement possible: the established path
	}
	if cls.job {
		return r.httpFirst(ctx, t, cmnd, cls)
	}
	if r.pathFor(p, cls) == MQTT {
		return r.sendMQTT(ctx, d, cmnd, cls)
	}
	raw, err := r.httpCommand(ctx, t, cmnd)
	return raw, r.explain(err, f, p, cls)
}

// httpFirst sends over HTTP and fails over to MQTT only when the error proves that nothing was
// sent (D4): a refused connection, an address outside the device networks, a 401 or a device that
// refuses the HTTP API. A timeout, an unparsable answer or a connection dropped after the request
// went out prove nothing; the command may have run and is never sent a second time.
func (r *Router) httpFirst(ctx context.Context, t tasmota.Target, cmnd string, cls cmdClass) (json.RawMessage, error) {
	kind, _, had := r.o.Memory.HTTPFailure(t.DeviceID())
	raw, err := r.httpCommand(ctx, t, cmnd)
	if err == nil || !tasmota.NotSent(err) || cls.job || r.mqttDoer() == nil {
		return raw, err
	}
	// SetOption128 0 makes the handler send nothing, but a write that got that answer on its first try is
	// not proven unsent (the same silence can be a command that ran): only reads fail over, or a device
	// the memory already knew to refuse the API.
	if errors.Is(err, tasmota.ErrHTTPAPIRefused) && cls.write && (!had || kind != HTTPFailAPIRefused) {
		return nil, err
	}
	d, f, p, derr := r.decide(ctx, t.DeviceID())
	if derr != nil {
		return nil, err
	}
	if r.pathFor(p, cls) == MQTT {
		r.o.Log.Info("transport: HTTP did not send, using MQTT", "device", int64(d.ID))
		return r.sendMQTT(ctx, d, cmnd, cls)
	}
	return nil, r.explain(err, f, p, cls)
}

// pathFor picks reads or writes.
func (r *Router) pathFor(p Paths, cls cmdClass) Path {
	if cls.write {
		return p.Writes
	}
	return p.Commands
}

// explain adds the reason MQTT was not used to an HTTP error of a device that is on our broker.
func (r *Router) explain(err error, f Facts, p Paths, cls cmdClass) error {
	if err == nil || !f.MQTT.Ours || f.MQTT.Topic == "" {
		return err
	}
	act := ActCommands
	if cls.write {
		act = ActWrites
	}
	if why := p.Reason(act); why != "" && r.pathFor(p, cls) == None {
		r.o.Log.Debug("transport: not using MQTT", "topic", f.MQTT.Topic, "action", string(act), "why", why)
		return fmt.Errorf("%w (%s)", err, why)
	}
	return err
}

// decide reads the device once and derives facts and paths from that one snapshot.
func (r *Router) decide(ctx context.Context, id model.DeviceID) (model.Device, Facts, Paths, error) {
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return d, Facts{}, Paths{}, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return d, f, Paths{}, err
	}
	return d, f, Choose(f), nil
}

func (r *Router) httpCommand(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	raw, err := r.http.Command(ctx, t, cmnd)
	if tasmota.NotSent(err) {
		noteTrace(ctx, None)
	} else {
		noteTrace(ctx, HTTP)
	}
	return raw, err
}

// pinnedMQTT is a call of a job that chose MQTT: no failover, no HTTP, and the same checks as a
// routed call. Writes need the strict conditions.
func (r *Router) pinnedMQTT(ctx context.Context, id model.DeviceID, cmnd string, cls cmdClass) (json.RawMessage, error) {
	if r.mqttDoer() == nil {
		return nil, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return nil, err
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return nil, err
	}
	if why := MQTTBlocker(f, cls.write); why != "" {
		return nil, fmt.Errorf("%w: not usable over MQTT: %s", tasmota.ErrUnreachable, why)
	}
	return r.sendMQTT(ctx, d, cmnd, cls)
}

// sendMQTT publishes the command for the device snapshot d. The device is read once more right
// before the send: a topic or broker change since the snapshot (an edit, a refresh) must not
// send to the old topic.
func (r *Router) sendMQTT(ctx context.Context, d model.Device, cmnd string, cls cmdClass) (json.RawMessage, error) {
	m := r.mqttDoer()
	if m == nil {
		return nil, fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	if cls.write {
		if name := cmdpolicy.CutOffName(cmnd); name != "" {
			if !HasCutOffConsent(ctx) {
				noteTrace(ctx, None)
				return nil, fmt.Errorf("%w: this device is reached over MQTT only; changing %s can cut TasmoFleet off from it. Use the console, which asks to confirm", ErrCutOff, name)
			}
		}
	}
	now, err := r.o.Devices.GetDevice(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if now.Topic != d.Topic || now.GroupTopic != d.GroupTopic || now.FullTopic != d.FullTopic || now.MQTT != d.MQTT {
		return nil, fmt.Errorf("%w: the device's MQTT settings changed while the command was prepared; nothing was sent", tasmota.ErrUnreachable)
	}
	var raw json.RawMessage
	if loose, _ := ctx.Value(keyLoose).(bool); loose {
		raw, err = m.DoLoose(ctx, d, cmnd)
	} else {
		raw, err = m.Do(ctx, d, cmnd)
	}
	if err == nil || !notPublished(err) {
		noteTrace(ctx, MQTT)
	}
	if err != nil {
		return nil, err
	}
	if cls.redact {
		raw = cmdpolicy.ScrubReply(raw) // the reply of a secret-setting command echoes the secret
	}
	return raw, nil
}

// notPublished: the channel refused the command or could not publish it.
func notPublished(err error) bool {
	return !errors.Is(err, tasmota.ErrTimeout) && (errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, mqttcmd.ErrRefused))
}

// ---- the rest of tasmota.Client

// DownloadConfig is HTTP only until the MQTT backup lands (FileDownload, #257).
func (r *Router) DownloadConfig(ctx context.Context, t tasmota.Target) ([]byte, error) {
	if pin, ok := PathFrom(ctx); ok && pin == MQTT {
		return nil, fmt.Errorf("%w: a backup over MQTT is not available yet", tasmota.ErrNotImplemented)
	}
	return r.http.DownloadConfig(ctx, t)
}

// RootPage is HTTP only: over MQTT the page does not exist. The identity of a device that only
// MQTT reaches comes from INFO1 and Status 2 (mqttidentity), not from here.
func (r *Router) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	if pin, ok := PathFrom(ctx); ok && pin == MQTT {
		return tasmota.RootInfo{}, fmt.Errorf("%w: the main page of a device cannot be read over MQTT yet", tasmota.ErrNotImplemented)
	}
	return r.http.RootPage(ctx, t)
}

// UploadFirmware passes straight to HTTP: an upload never fails over (D4).
func (r *Router) UploadFirmware(ctx context.Context, t tasmota.Target, filename string, size int64, body io.Reader) error {
	u, ok := r.o.HTTP.(tasmota.Uploader)
	if !ok {
		return fmt.Errorf("%w: the HTTP client cannot upload", tasmota.ErrUploadUnsupported)
	}
	return u.UploadFirmware(ctx, t, filename, size, body)
}

// ReadUpLayout passes straight to HTTP.
func (r *Router) ReadUpLayout(ctx context.Context, t tasmota.Target) (tasmota.UpLayout, error) {
	s, ok := r.o.HTTP.(tasmota.BootSwitcher)
	if !ok {
		return "", fmt.Errorf("%w: the HTTP client cannot read the layout", tasmota.ErrNotImplemented)
	}
	return s.ReadUpLayout(ctx, t)
}

// SwitchBoot passes straight to HTTP.
func (r *Router) SwitchBoot(ctx context.Context, t tasmota.Target, to tasmota.BootTarget) (tasmota.BootReply, error) {
	s, ok := r.o.HTTP.(tasmota.BootSwitcher)
	if !ok {
		return "", fmt.Errorf("%w: the HTTP client cannot switch the boot partition", tasmota.ErrNotImplemented)
	}
	return s.SwitchBoot(ctx, t, to)
}
