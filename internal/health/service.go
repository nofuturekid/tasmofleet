package health

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Defaults, see spec "Health model".
const (
	DefaultStaleAfter   = 660 * time.Second // 2 x TelePeriod (300 s) + 60 s
	DefaultOfflineAfter = 3
	DefaultPollInterval = 5 * time.Minute
	DefaultPollParallel = 4

	maxBackoff    = 30 * time.Minute
	jitterFrac    = 0.2
	tickEvery     = 5 * time.Second
	reloadEvery   = 30 * time.Second
	pollTimeout   = 30 * time.Second // backstop; the client has its own per-request timeout
	persistBuffer = 256

	// snapshotRetry spaces repeated snapshot fetches of one device that keeps
	// answering without usable STATE data (minimal firmware).
	snapshotRetry = time.Minute
)

// Store is the persistence the tracker needs.
type Store interface {
	store.DeviceStore
	store.HealthStore
}

// Deps are the collaborators of a Service. Bus may be nil: without a broker
// every device is health-checked by HTTP polling alone, and nothing is
// reported as degraded because MQTT is missing.
type Deps struct {
	Store  Store
	Client tasmota.Client
	Bus    mqttbus.Bus    // optional
	Clock  clock.Clock    // default clock.Real
	Hub    *events.Hub    // optional; receives TopicHealth events
	Rand   func() float64 // optional, [0,1); jitter source for tests

	// OnMQTTSeen is called once per device (and run) when the first message
	// on our broker matches the device's topic. Inventory uses it to set
	// Device.MQTT to MQTTOurs (spec 6.4). It runs off the bus goroutine.
	OnMQTTSeen func(id model.DeviceID, topic string)
}

var _ Tracker = (*Service)(nil)

// Service is the Tracker implementation.
type Service struct {
	d       Deps
	o       Options
	started time.Time
	staleNS atomic.Int64 // StaleAfter in nanoseconds; SetIntervals changes it
	pollNS  atomic.Int64 // PollInterval in nanoseconds

	mu         sync.Mutex
	devs       map[model.DeviceID]*devState
	byTopic    map[string]model.DeviceID
	lastReload time.Time
	lastVer    int64 // DataVersion seen by the last reload

	sem  chan struct{}
	work chan func()
	wg   sync.WaitGroup
}

type devState struct {
	id    model.DeviceID
	topic string
	cap   model.MQTTCapability

	mqttSeen   bool // a message on our broker matched this device
	lwt        string
	lastMQTT   time.Time
	lastHTTP   time.Time
	fails      int
	authFailed bool
	minimal    bool

	nextPoll time.Time
	polling  bool

	hasSnap   bool      // uptime/RSSI/power came in via STATE or Status 11
	snapAfter time.Time // earliest next snapshot fetch
	h         model.Health

	sigDBm int                      // Wifi.Signal from STATE; valid when hasSig
	hasSig bool                     //
	energy map[string]EnergyChannel // channel ("" = single meter) -> last tele/SENSOR ENERGY
	power  *powerRing               // watts per minute for the last two hours; nil until the first reading
}

// New builds a Service. Zero Options fields select the defaults.
func New(d Deps, o Options) *Service {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Rand == nil {
		d.Rand = rand.Float64
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = DefaultStaleAfter
	}
	if o.OfflineAfter <= 0 {
		o.OfflineAfter = DefaultOfflineAfter
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.PollParallel <= 0 {
		o.PollParallel = DefaultPollParallel
	}
	svc := &Service{
		d: d, o: o,
		started: d.Clock.Now(),
		devs:    make(map[model.DeviceID]*devState),
		byTopic: make(map[string]model.DeviceID),
		sem:     make(chan struct{}, o.PollParallel),
		work:    make(chan func(), persistBuffer),
	}
	svc.staleNS.Store(int64(o.StaleAfter))
	svc.pollNS.Store(int64(o.PollInterval))
	return svc
}

// Get returns the health of one device; unknown devices are "unknown".
func (s *Service) Get(id model.DeviceID) model.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds, ok := s.devs[id]; ok {
		return cloneHealth(ds.h)
	}
	return model.Health{DeviceID: id, State: model.HealthUnknown}
}

// All returns a snapshot of every tracked device.
func (s *Service) All() map[model.DeviceID]model.Health {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[model.DeviceID]model.Health, len(s.devs))
	for id, ds := range s.devs {
		out[id] = cloneHealth(ds.h)
	}
	return out
}

// Minimal reports whether the last successful poll found minimal firmware
// (Status 11 unknown, Upgrade probe answered). The UI shows it as "minimal".
func (s *Service) Minimal(id model.DeviceID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.devs[id]
	return ok && ds.minimal
}

func cloneHealth(h model.Health) model.Health {
	if h.Power != nil {
		p := make(map[string]string, len(h.Power))
		for k, v := range h.Power {
			p[k] = v
		}
		h.Power = p
	}
	return h
}

// Run consumes bus messages and polls until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	// Load the inventory first: Subscribe replays the retained LWTs at once
	// and they can only be matched to devices by topic.
	s.reload(ctx, s.d.Clock.Now())
	if s.d.Bus != nil {
		unsub := s.d.Bus.Subscribe([]mqttbus.Kind{
			mqttbus.KindLWT, mqttbus.KindState, mqttbus.KindSensor, mqttbus.KindResult,
			mqttbus.KindPower, mqttbus.KindStatus, mqttbus.KindInfo, mqttbus.KindUpgrade,
		}, s.onMessage)
		defer unsub()
	}
	var devCh <-chan events.Event
	if s.d.Hub != nil {
		ch, cancel := s.d.Hub.Subscribe(events.TopicDevice)
		defer cancel()
		devCh = ch
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		for {
			select {
			case f := <-s.work:
				f()
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		s.wg.Wait()
		<-workerDone
	}()

	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-s.d.Clock.After(tickEvery):
		case <-devCh: // inventory changed: pick the change up now
			s.mu.Lock()
			s.lastReload = time.Time{}
			s.mu.Unlock()
		}
	}
}

// tick reloads the inventory when due, re-evaluates time-based states and
// starts the polls that are due. It never blocks on a poll.
func (s *Service) tick(ctx context.Context) {
	now := s.d.Clock.Now()
	s.mu.Lock()
	reload := s.lastReload.IsZero() || now.Sub(s.lastReload) >= reloadEvery
	lastVer := s.lastVer
	s.mu.Unlock()
	// Another process (CLI import) can change the inventory without a hub
	// event: a cheap version check per tick picks that up; the full reload
	// every reloadEvery stays as the backstop.
	if !reload {
		if v, ok := s.dataVersion(ctx); ok && v != lastVer {
			reload = true
		}
	}
	if reload {
		s.reload(ctx, now)
	}

	var trs []transition
	var due []model.DeviceID
	snapBudget := s.o.PollParallel // per tick: no burst of snapshot fetches
	s.mu.Lock()
	for id, ds := range s.devs {
		if tr, ok := s.evaluate(ds, now, ds.h.Source, "no recent observation"); ok {
			trs = append(trs, tr)
		}
		if ds.polling {
			continue
		}
		if s.pollEligible(ds, now) && !now.Before(ds.nextPoll) {
			ds.polling = true
			due = append(due, id)
		} else if snapBudget > 0 && s.snapshotDue(ds, now) {
			ds.polling = true
			snapBudget--
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	s.persist(ctx, trs)
	for _, id := range due {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.poll(ctx, id)
		}()
	}
}

// dataVersion asks the store for its change marker; stores without one
// (fakes, other backends) report ok=false and rely on the periodic reload.
func (s *Service) dataVersion(ctx context.Context) (int64, bool) {
	dv, ok := s.d.Store.(interface {
		DataVersion(context.Context) (int64, error)
	})
	if !ok {
		return 0, false
	}
	v, err := dv.DataVersion(ctx)
	return v, err == nil
}

func (s *Service) reload(ctx context.Context, now time.Time) {
	// Read the version before the list: a commit in between triggers one more
	// reload instead of being missed.
	ver, _ := s.dataVersion(ctx)
	list, err := s.d.Store.ListDevices(ctx)
	if err != nil {
		slog.Warn("health: list devices failed", "err", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastReload = now
	s.lastVer = ver
	seen := make(map[model.DeviceID]struct{}, len(list))
	s.byTopic = make(map[string]model.DeviceID, len(list))
	for _, dev := range list {
		seen[dev.ID] = struct{}{}
		ds, ok := s.devs[dev.ID]
		if !ok {
			ds = &devState{id: dev.ID, h: model.Health{DeviceID: dev.ID, State: model.HealthUnknown, Since: now}}
			s.devs[dev.ID] = ds
		}
		ds.topic, ds.cap = dev.Topic, dev.MQTT
		if dev.Topic != "" {
			s.byTopic[dev.Topic] = dev.ID
		}
	}
	for id := range s.devs {
		if _, ok := seen[id]; !ok {
			delete(s.devs, id)
		}
	}
}

// busEnabled reports whether MQTT is in use right now. A bus that can be turned
// off while the server runs (mqttbus.Switch) says so; the others always are.
// Callers hold s.mu, so Enabled implementations must never block on a lock
// that is held while handlers run (Switch uses an atomic for this reason).
func (s *Service) busEnabled() bool {
	if s.d.Bus == nil {
		return false
	}
	if e, ok := s.d.Bus.(interface{ Enabled() bool }); ok {
		return e.Enabled()
	}
	return true
}

// mqttMode: health uses MQTT only for devices that talk to our broker.
func (s *Service) mqttMode(ds *devState) bool {
	return s.busEnabled() && ds.topic != "" && (ds.cap == model.MQTTOurs || ds.mqttSeen)
}

func (s *Service) lastObs(ds *devState) time.Time {
	if ds.lastHTTP.After(ds.lastMQTT) {
		return ds.lastHTTP
	}
	return ds.lastMQTT
}

func (s *Service) fresh(ds *devState, now time.Time) bool {
	obs := s.lastObs(ds)
	return !obs.IsZero() && now.Sub(obs) < s.staleAfter()
}

// pollEligible: HTTP-only devices are always polled; MQTT devices only
// while their telemetry is silent (and the LWT does not already say offline).
func (s *Service) pollEligible(ds *devState, now time.Time) bool {
	if !s.mqttMode(ds) {
		return true
	}
	if ds.lwt == "Offline" || s.fresh(ds, now) {
		return false
	}
	// Silent, or never heard since start and the grace period is over.
	return !s.lastObs(ds).IsZero() || now.Sub(s.started) >= s.staleAfter()
}

// snapshotDue: an MQTT device that is alive (a retained LWT or any message
// counts) but never delivered a STATE snapshot gets one Status 11 now instead
// of waiting up to a TelePeriod for its next tele/STATE. HTTP-only devices
// are covered by the normal poll, which starts at once.
func (s *Service) snapshotDue(ds *devState, now time.Time) bool {
	return s.mqttMode(ds) && !ds.hasSnap && !ds.minimal && !ds.authFailed &&
		ds.lwt != "Offline" && !s.lastObs(ds).IsZero() && !now.Before(ds.snapAfter)
}

func (s *Service) compute(ds *devState, now time.Time) model.HealthState {
	switch {
	case ds.authFailed:
		return model.HealthAuthFailed
	case s.mqttMode(ds):
		switch {
		case ds.lwt == "Offline":
			return model.HealthOffline
		case s.fresh(ds, now):
			return model.HealthOnline
		case ds.fails >= s.o.OfflineAfter:
			return model.HealthOffline
		case s.lastObs(ds).IsZero() && now.Sub(s.started) < s.staleAfter():
			return model.HealthUnknown
		default:
			return model.HealthStale
		}
	default:
		switch {
		case ds.fails >= s.o.OfflineAfter:
			return model.HealthOffline
		case ds.lastHTTP.IsZero():
			return model.HealthUnknown
		default:
			return model.HealthOnline
		}
	}
}

type transition struct {
	id       model.DeviceID
	at       time.Time
	from, to model.HealthState
	source   model.HealthSource
	detail   string
	lastSeen time.Time
}

// evaluate recomputes the state (caller holds s.mu), updates the exported
// Health, publishes it and reports a state change for persistence.
func (s *Service) evaluate(ds *devState, now time.Time, src model.HealthSource, detail string) (transition, bool) {
	ds.h.LastSeen = s.lastObs(ds)
	ds.h.SeenMQTT, ds.h.SeenHTTP = ds.lastMQTT, ds.lastHTTP
	ds.h.LWT = ds.lwt
	if src != "" {
		ds.h.Source = src
	}
	st := s.compute(ds, now)
	if st == ds.h.State {
		return transition{}, false
	}
	tr := transition{id: ds.id, at: now, from: ds.h.State, to: st, source: ds.h.Source, detail: detail, lastSeen: ds.h.LastSeen}
	ds.h.State, ds.h.Since = st, now
	s.publish(ds)
	return tr, true
}

func (s *Service) publish(ds *devState) {
	if s.d.Hub != nil {
		s.d.Hub.Publish(events.Event{Topic: events.TopicHealth, At: s.d.Clock.Now(), Payload: cloneHealth(ds.h)})
	}
}

// persist writes transitions (only changes are stored, spec 10). A device's first observation
// since the start (from unknown) is its baseline, not a change: the summary below is written,
// but no history event, so a restart does not log "Unknown -> Online" for every device.
func (s *Service) persist(ctx context.Context, trs []transition) {
	for _, tr := range trs {
		if tr.from != "" && tr.from != model.HealthUnknown {
			if err := s.d.Store.AppendHealthEvent(ctx, store.HealthEvent{
				DeviceID: tr.id, At: tr.at, From: tr.from, To: tr.to, Source: tr.source, Detail: tr.detail,
			}); err != nil {
				slog.Warn("health: persisting transition failed", "device", tr.id, "err", err)
			}
		}
		seen := tr.lastSeen
		if seen.IsZero() {
			seen = tr.at
		}
		if err := s.d.Store.SetLastSeen(ctx, tr.id, tr.to, seen); err != nil {
			slog.Warn("health: persisting last seen failed", "device", tr.id, "err", err)
		}
	}
}

// ---- MQTT ----

var powerKey = regexp.MustCompile(`^POWER[0-9]*$`)

type stateDoc struct {
	UptimeSec *int64 `json:"UptimeSec"`
	Wifi      *struct {
		RSSI   *int `json:"RSSI"`
		Signal *int `json:"Signal"` // dBm
	} `json:"Wifi"`
}

// onMessage runs on the bus goroutine: memory updates only, no I/O.
func (s *Service) onMessage(m mqttbus.Message) {
	if m.Retained && m.Kind != mqttbus.KindLWT {
		return // a retained telemetry message says nothing about now
	}
	now := s.d.Clock.Now()
	s.mu.Lock()
	id, ok := s.byTopic[m.Topic]
	ds := s.devs[id]
	if !ok || ds == nil {
		s.mu.Unlock()
		return
	}
	firstSeen := !ds.mqttSeen
	ds.mqttSeen = true
	ds.lastMQTT = now
	ds.fails = 0 // the device is demonstrably alive
	ds.authFailed = false

	switch m.Kind {
	case mqttbus.KindLWT:
		ds.lwt = strings.TrimSpace(string(m.Payload))
		switch {
		case strings.EqualFold(ds.lwt, "online"):
			ds.lwt = "Online"
		case strings.EqualFold(ds.lwt, "offline"):
			ds.lwt = "Offline"
		}
	case mqttbus.KindState, mqttbus.KindResult:
		s.applyJSON(ds, m.Payload, m.Kind == mqttbus.KindState)
	case mqttbus.KindPower:
		if ds.h.Power == nil {
			ds.h.Power = map[string]string{}
		}
		ds.h.Power[m.Suffix] = strings.ToUpper(strings.TrimSpace(string(m.Payload)))
	case mqttbus.KindSensor:
		if w, ok := totalWatts(applyEnergy(ds, m.Payload)); ok {
			if ds.power == nil {
				ds.power = &powerRing{}
			}
			ds.power.add(now, w)
		}
	case mqttbus.KindStatus, mqttbus.KindDiscovery, mqttbus.KindUpgrade, mqttbus.KindInfo:
		// A sign of life only; nothing to merge.
	}
	tr, changed := s.evaluate(ds, now, model.SourceMQTT, "mqtt "+string(m.Kind))
	if !changed {
		s.publish(ds)
	}
	topic := ds.topic
	if firstSeen {
		ds.cap = model.MQTTOurs
	}
	s.mu.Unlock()

	if changed {
		s.enqueue(func() { s.persist(context.Background(), []transition{tr}) })
	}
	if firstSeen && s.d.OnMQTTSeen != nil {
		s.enqueue(func() { s.d.OnMQTTSeen(id, topic) })
	}
}

func (s *Service) enqueue(f func()) {
	select {
	case s.work <- f:
	default:
		slog.Warn("health: persistence queue full, dropping one update")
	}
}

// applyJSON merges POWERn, Wifi.RSSI and UptimeSec from a STATE/RESULT
// payload. Unparsable payloads still count as a sign of life.
func (s *Service) applyJSON(ds *devState, payload []byte, telemetry bool) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return
	}
	for k, v := range raw {
		if !powerKey.MatchString(k) {
			continue
		}
		var val string
		if json.Unmarshal(v, &val) == nil {
			if ds.h.Power == nil {
				ds.h.Power = map[string]string{}
			}
			ds.h.Power[k] = val
		}
	}
	if !telemetry {
		return
	}
	var doc stateDoc
	if json.Unmarshal(payload, &doc) != nil {
		return
	}
	if doc.UptimeSec != nil {
		ds.h.UptimeSec = *doc.UptimeSec
		ds.hasSnap = true
	}
	if doc.Wifi != nil && doc.Wifi.RSSI != nil {
		ds.h.RSSI = *doc.Wifi.RSSI
	}
	if doc.Wifi != nil && doc.Wifi.Signal != nil {
		ds.sigDBm, ds.hasSig = *doc.Wifi.Signal, true
	}
}

// ---- HTTP poll ----

func (s *Service) poll(ctx context.Context, id model.DeviceID) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.finishPoll(id, time.Time{})
		return
	}
	dev, err := s.d.Store.GetDevice(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			slog.Warn("health: loading device failed", "device", id, "err", err)
		}
		s.finishPoll(id, s.d.Clock.Now().Add(s.pollInterval())) // a vanished device is dropped by the next reload
		return
	}
	pctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	res, err := s.probe(pctx, dev)
	if ctx.Err() != nil {
		s.finishPoll(id, time.Time{})
		return
	}
	now := s.d.Clock.Now()

	s.mu.Lock()
	ds := s.devs[id]
	if ds == nil {
		s.mu.Unlock()
		return
	}
	ds.polling = false
	detail := "http poll ok"
	switch {
	case err == nil:
		ds.fails, ds.authFailed = 0, false
		ds.lastHTTP = now
		ds.minimal = res.minimal
		if !res.minimal {
			s.applyJSON(ds, res.sts, true)
		}
		ds.nextPoll = now
		ds.snapAfter = now.Add(snapshotRetry)
		if !s.mqttMode(ds) {
			ds.nextPoll = now.Add(s.jitter(s.pollInterval()))
		}
	case errors.Is(err, tasmota.ErrAuth):
		ds.fails++
		ds.authFailed = true
		ds.nextPoll = now.Add(s.failDelay(ds.fails))
		ds.snapAfter = ds.nextPoll
		detail = "http poll: credentials rejected"
	default:
		ds.fails++
		ds.authFailed = false
		ds.nextPoll = now.Add(s.failDelay(ds.fails))
		ds.snapAfter = ds.nextPoll
		detail = "http poll failed: " + errKind(err)
	}
	tr, changed := s.evaluate(ds, now, model.SourceHTTP, detail)
	if !changed {
		s.publish(ds)
	}
	s.mu.Unlock()
	if changed {
		s.persist(ctx, []transition{tr})
	}
}

func (s *Service) finishPoll(id model.DeviceID, next time.Time) {
	s.mu.Lock()
	if ds := s.devs[id]; ds != nil {
		ds.polling = false
		ds.nextPoll = next
	}
	s.mu.Unlock()
}

type probeResult struct {
	minimal bool
	sts     json.RawMessage
}

// probe sends "Status 11" and, if the device answers Unknown (minimal
// firmware), the bare "Upgrade" probe. Never "Status 0".
func (s *Service) probe(ctx context.Context, dev model.Device) (probeResult, error) {
	t := tasmota.TargetFor(dev)
	raw, err := s.d.Client.Command(ctx, t, "Status 11")
	if err == nil {
		var env struct {
			StatusSTS json.RawMessage `json:"StatusSTS"`
		}
		if json.Unmarshal(raw, &env) != nil || env.StatusSTS == nil {
			return probeResult{}, tasmota.ErrBadResponse
		}
		return probeResult{sts: env.StatusSTS}, nil
	}
	if !errors.Is(err, tasmota.ErrUnknownCommand) {
		return probeResult{}, err
	}
	if _, err := s.d.Client.Command(ctx, t, "Upgrade"); err != nil {
		return probeResult{}, err
	}
	return probeResult{minimal: true}, nil
}

func errKind(err error) string {
	switch {
	case errors.Is(err, tasmota.ErrTimeout):
		return "timeout"
	case errors.Is(err, tasmota.ErrUnreachable):
		return "unreachable"
	case errors.Is(err, tasmota.ErrBadResponse):
		return "bad response"
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return "address not allowed"
	default:
		return "error" // no err.Error(): it may carry device-controlled text
	}
}

// backoff is the poll delay after fails consecutive failures: the normal
// interval until the device counts as offline, then doubling up to 30 min.
func (s *Service) backoff(fails int) time.Duration {
	d := s.pollInterval()
	for n := s.o.OfflineAfter; n <= fails && d < maxBackoff; n++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

// jitter spreads d by +-20 % so that polls do not synchronise.
func (s *Service) jitter(d time.Duration) time.Duration {
	f := 1 + jitterFrac*(2*s.d.Rand()-1)
	return time.Duration(float64(d) * f)
}

// failDelay is the jittered delay after a failed poll, never beyond the
// 30 minute cap (or the poll interval when that is longer).
func (s *Service) failDelay(fails int) time.Duration {
	limit := maxBackoff
	if s.pollInterval() > limit {
		limit = s.pollInterval()
	}
	return min(s.jitter(s.backoff(fails)), limit)
}

func (s *Service) staleAfter() time.Duration { return time.Duration(s.staleNS.Load()) }

// StaleAfter is the silence after which an MQTT device counts as stale (the
// UI names it in the tooltip of the Stale badge).
func (s *Service) StaleAfter() time.Duration   { return s.staleAfter() }
func (s *Service) pollInterval() time.Duration { return time.Duration(s.pollNS.Load()) }

// SetIntervals changes the stale threshold and the HTTP poll interval while
// the service runs (the settings page). Non-positive values are ignored.
func (s *Service) SetIntervals(staleAfter, pollInterval time.Duration) {
	if staleAfter > 0 {
		s.staleNS.Store(int64(staleAfter))
	}
	if pollInterval > 0 {
		s.pollNS.Store(int64(pollInterval))
	}
}
