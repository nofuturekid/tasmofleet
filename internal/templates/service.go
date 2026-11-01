package templates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// Collaborators of the Manager, small so the tests can fake them.
type (
	// Inventory lists the devices.
	Inventory interface {
		ListDevices(ctx context.Context) ([]model.Device, error)
		GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	}
	// Backups gives the stored backups; the newest snapshot is the fallback reading.
	Backups interface {
		List(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
		Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error)
	}
	// KeyReader reads settings (settings.Manager.ReadKeys); only PowerOnState is asked.
	KeyReader interface {
		ReadKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error)
	}
	// Submitter starts the transaction (ota.Engine).
	Submitter interface {
		SubmitTemplate(ctx context.Context, actor string, start ota.TemplateStart, reqs []ota.TemplateRequest) (model.BatchID, []ota.Refusal, error)
	}
)

// Options configure the Manager.
type Options struct {
	Devices  Inventory
	Client   Commander
	Backups  Backups
	Settings KeyReader // optional: without it the restart line has no PowerOnState
	Engine   Submitter // nil: Apply returns ErrNoEngine
	// Health returns the health of a device (offline devices are not read and carry "offline since").
	Health func(model.DeviceID) model.Health
	Now    func() time.Time
	Log    *slog.Logger
}

const (
	// liveMaxAge is how long a live reading counts as live (E5 applies it to display and reference choice
	// only; Preview and the job read again).
	liveMaxAge   = time.Hour
	liveGap      = time.Minute // refresh requests closer together than this are answered from the cache
	liveParallel = 4
	liveTimeout  = 30 * time.Second
)

// Manager implements Service.
type Manager struct {
	o     Options
	cache *cache

	mu      sync.Mutex // single flight of refresh
	lastRun time.Time

	snapMu sync.Mutex
	snaps  map[model.BackupID]snapRead
}

type snapRead struct {
	r  reading
	ok bool
}

var _ Service = (*Manager)(nil)

// New returns the service.
func New(o Options) *Manager {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Manager{o: o, cache: newCache(liveMaxAge), snaps: map[model.BackupID]snapRead{}}
}

// view builds the DeviceView of d from a reading and where it came from.
func (m *Manager) view(d model.Device, r *reading, src Source) DeviceView {
	v := DeviceView{ID: d.ID, Name: d.Name, Family: d.Identity.Family, Hardware: d.Identity.Hardware, Source: src}
	v.Reading = policy.TemplateDevice{ID: d.ID, Family: d.Identity.Family, Variant: d.Identity.Variant, Hardware: d.Identity.Hardware,
		Wired: d.Net.Wired, Live: src == SourceLive}
	if r != nil {
		mod := r.Module
		v.Reading.Module, v.Reading.Template, v.ReadAt = &mod, r.Template, r.At
	}
	if m.o.Health != nil {
		if h := m.o.Health(d.ID); h.State == model.HealthOffline {
			v.OfflineSince = h.Since
		}
	}
	if v.Reading.State() == policy.TemplateUnknown {
		v.NotJudgedReason = notJudgedReason(v, r)
	}
	return v
}

func notJudgedReason(v DeviceView, r *reading) string {
	var why string
	switch {
	case v.Family == model.ChipUnknown:
		why = "The chip family is not known yet."
	case r == nil:
		why = "Not read yet, and no backup holds a template."
	case r.TplErr != "":
		why = "The template could not be read: " + r.TplErr + "."
	default:
		why = "The template was not read."
	}
	if !v.OfflineSince.IsZero() {
		why = "The device is offline. " + why
	}
	return why
}

// snapshotReading is the newest backup snapshot of the device as a reading, or nil.
func (m *Manager) snapshotReading(ctx context.Context, id model.DeviceID) *reading {
	list, err := m.o.Backups.List(ctx, id)
	if err != nil {
		return nil
	}
	var newest *model.Backup
	for i := range list {
		if list[i].Kind == model.BackupSnapshot && (newest == nil || list[i].TakenAt.After(newest.TakenAt)) {
			newest = &list[i]
		}
	}
	if newest == nil {
		return nil
	}
	m.snapMu.Lock()
	s, done := m.snaps[newest.ID]
	m.snapMu.Unlock()
	if !done {
		data, _, err := m.o.Backups.Open(ctx, newest.ID)
		if err != nil {
			return nil // unreadable: unknown, and tried again next time
		}
		s.r, s.ok = fromSnapshot(data, newest.TakenAt)
		m.snapMu.Lock()
		m.snaps[newest.ID] = s
		m.snapMu.Unlock()
	}
	if !s.ok {
		return nil
	}
	return &s.r
}

// bestReading is the live reading from the cache, else the newest snapshot.
func (m *Manager) bestReading(ctx context.Context, d model.Device) DeviceView {
	now := m.o.Now()
	if r, ok := m.cache.get(d.ID, now); ok {
		return m.view(d, &r, SourceLive)
	}
	if r := m.snapshotReading(ctx, d.ID); r != nil {
		return m.view(d, r, SourceBackup)
	}
	return m.view(d, nil, SourceNone)
}

// refresh reads the devices that are not offline, a few at a time. It reports whether the rate limit
// stopped it.
func (m *Manager) refresh(ctx context.Context, devs []model.Device) bool {
	if !m.mu.TryLock() { // single flight: wait for the run in progress, then use what it read
		m.mu.Lock()
		defer m.mu.Unlock()
		return true
	}
	defer m.mu.Unlock()
	if now := m.o.Now(); !m.lastRun.IsZero() && now.Sub(m.lastRun) < liveGap {
		return true
	}
	m.lastRun = m.o.Now()
	sem := make(chan struct{}, liveParallel)
	var wg sync.WaitGroup
	for _, d := range devs {
		if m.o.Health != nil && m.o.Health(d.ID).State == model.HealthOffline {
			continue
		}
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			m.readInto(ctx, d)
		}()
	}
	wg.Wait()
	return false
}

// readInto reads one device live and stores the result; a failed read leaves the cache as it was.
func (m *Manager) readInto(ctx context.Context, d model.Device) (reading, error) {
	rctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	r, err := readLive(rctx, m.o.Client, d, m.o.Now())
	if err != nil {
		m.o.Log.Debug("template read failed", "device", int64(d.ID), "err", err)
		return reading{}, err
	}
	m.cache.put(d.ID, r)
	return r, nil
}

// Overview implements Service.
func (m *Manager) Overview(ctx context.Context, refs Refs, refresh bool) (Overview, error) {
	devs, err := m.o.Devices.ListDevices(ctx)
	if err != nil {
		return Overview{}, err
	}
	var out Overview
	if refresh {
		out.Throttled = m.refresh(ctx, devs)
	}
	views := make(map[model.DeviceID]DeviceView, len(devs))
	tds := make([]policy.TemplateDevice, 0, len(devs))
	for _, d := range devs {
		v := m.bestReading(ctx, d)
		views[d.ID] = v
		tds = append(tds, v.Reading)
		if v.Source == SourceLive && v.ReadAt.After(out.ReadAt) {
			out.ReadAt = v.ReadAt
		}
	}
	groups, notJudged := policy.GroupTemplates(tds)
	for _, g := range groups {
		gv := GroupView{Key: g.Key}
		var ref *policy.Reference
		if !g.Key.BuiltIn {
			if id, ok := refs[g.Key]; ok {
				if r, err := policy.ChosenReference(g, id); err == nil {
					ref = &r
				}
			} else if r, ok := policy.MostCommon(g); ok {
				ref = &r
			}
			gv.Reference, gv.NeedsReference = ref, ref == nil
		}
		for _, td := range g.Devices {
			v := views[td.ID]
			if ref != nil {
				p := policy.PlanApply(*ref, v.Reading, policy.PlanOptions{StrictMQTT: true})
				v.Plan = &p
			}
			gv.Devices = append(gv.Devices, v)
		}
		if g.Key.BuiltIn {
			out.BuiltIn = append(out.BuiltIn, gv)
		} else {
			out.Groups = append(out.Groups, gv)
		}
	}
	for _, td := range notJudged {
		out.NotJudged = append(out.NotJudged, views[td.ID])
	}
	return out, nil
}

// fresh reads a device live now and returns its view; the reading is cached.
func (m *Manager) fresh(ctx context.Context, d model.Device) (DeviceView, error) {
	r, err := m.readInto(ctx, d)
	if err != nil {
		return m.view(d, nil, SourceNone), err
	}
	return m.view(d, &r, SourceLive), nil
}

// Preview implements Service.
func (m *Manager) Preview(ctx context.Context, req Request) (Preview, error) {
	if req.Group.BuiltIn {
		return Preview{}, errors.New("templates: a built-in module group has no template to apply")
	}
	refDev, err := m.o.Devices.GetDevice(ctx, req.Reference)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: %w", ErrBadReference, err)
	}
	refView, err := m.fresh(ctx, refDev)
	if err != nil {
		return Preview{}, fmt.Errorf("%w: it could not be read: %w", ErrBadReference, err)
	}
	rt := refView.Reading
	if rt.State() != policy.OnTemplate || rt.Family != req.Group.Family || rt.Template.Name != req.Group.Name {
		return Preview{}, fmt.Errorf("%w: it is not on the user template %q of %s", ErrBadReference, req.Group.Name, req.Group.Family)
	}
	ref := policy.Reference{Device: rt, How: policy.RefChosen}
	if !req.Chosen {
		ref.How = policy.RefMostCommon
	}
	pv := Preview{Group: req.Group, Reference: refView, ReferenceTemplate: *rt.Template,
		BackupLine:  "A backup is taken first; nothing is sent if it fails.",
		RestartLine: "Each device restarts once. Relays can switch while the pins change; after the restart they follow their PowerOnState."}
	ids := slices.Clone(req.Devices)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		d, err := m.o.Devices.GetDevice(ctx, id)
		if err != nil { // deleted, or never ours: shown, so the count the person sees stays explained
			pv.Excluded = append(pv.Excluded, Excluded{Device: DeviceView{ID: id, Name: "#" + strconv.FormatInt(int64(id), 10)}, Why: "device not found (deleted?)", Refusal: policy.RefuseNotLive})
			continue
		}
		var v DeviceView
		if id == req.Reference {
			v = refView
		} else if v, err = m.fresh(ctx, d); err != nil {
			pv.Excluded = append(pv.Excluded, Excluded{Device: v, Why: "could not be read: " + err.Error(), Refusal: policy.RefuseNotLive})
			continue
		}
		p := policy.PlanApply(ref, v.Reading, policy.PlanOptions{StrictMQTT: true})
		v.Plan = &p
		switch {
		case id == req.Reference:
			pv.Excluded = append(pv.Excluded, Excluded{Device: v, Why: "is the reference"})
		case p.Matches:
			pv.Excluded = append(pv.Excluded, Excluded{Device: v, Why: "matches the reference"})
		case p.Refusal != policy.RefuseNone:
			pv.Excluded = append(pv.Excluded, Excluded{Device: v, Why: p.Reason, Refusal: p.Refusal})
		default:
			m.addPowerOnState(ctx, &v)
			pv.Rows = append(pv.Rows, PreviewRow{Device: v, Plan: p})
		}
	}
	pv.Applicable = len(pv.Rows)
	pv.Digest = digest(req, pv)
	return pv, nil
}

// powerOnStateText words the PowerOnState setting for the restart line.
func powerOnStateText(v string) string {
	switch strings.TrimSpace(v) {
	case "0":
		return "After the restart the relays are off (PowerOnState 0)."
	case "1":
		return "After the restart the relays are on (PowerOnState 1)."
	case "2":
		return "After the restart the relays toggle from their saved state (PowerOnState 2)."
	case "3":
		return "After the restart the relays return to their last saved state (PowerOnState 3)."
	case "":
		return ""
	}
	return "After the restart the relays follow PowerOnState " + strings.TrimSpace(v) + "."
}

// addPowerOnState reads PowerOnState of a device that will restart; a failed read leaves it unknown.
func (m *Manager) addPowerOnState(ctx context.Context, v *DeviceView) {
	if m.o.Settings == nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	got, err := m.o.Settings.ReadKeys(rctx, v.ID, []string{"PowerOnState"})
	if err != nil {
		return
	}
	v.PowerOnState = got["PowerOnState"]
	v.PowerOnStateText = powerOnStateText(v.PowerOnState)
}

// digest covers the group, the reference and its layout, and for every device the layout it has now and
// what happens to it (the command, or why not). A device that changed, or another plan, gives another digest.
func digest(req Request, pv Preview) string {
	h := sha256.New()
	w := func(parts ...string) { h.Write([]byte(strings.Join(parts, "\x00") + "\n")) }
	id := func(i model.DeviceID) string { return strconv.FormatInt(int64(i), 10) }
	w("group", string(req.Group.Family), req.Group.Name, strconv.FormatBool(req.Group.BuiltIn))
	w("ref", id(req.Reference), pv.ReferenceTemplate.Name, pv.ReferenceTemplate.Fingerprint())
	for _, r := range pv.Rows {
		w("row", id(r.Device.ID), r.Device.Reading.Template.Fingerprint(), strings.Join(r.Plan.Commands, "\x01"))
	}
	ex := slices.Clone(pv.Excluded)
	sort.Slice(ex, func(i, j int) bool { return ex[i].Device.ID < ex[j].Device.ID })
	for _, e := range ex {
		fp := ""
		if t := e.Device.Reading.Template; t != nil {
			fp = t.Fingerprint()
		}
		w("excluded", id(e.Device.ID), string(e.Refusal), fp)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Apply implements Service.
func (m *Manager) Apply(ctx context.Context, actor string, req Request, want string) (model.BatchID, []Refusal, error) {
	if m.o.Engine == nil {
		return 0, nil, ErrNoEngine
	}
	pv, err := m.Preview(ctx, req)
	if err != nil {
		return 0, nil, err
	}
	if pv.Digest != want {
		return 0, nil, ErrStaleDigest
	}
	if pv.Applicable == 0 {
		return 0, nil, ErrNothingToApply
	}
	reqs := make([]ota.TemplateRequest, 0, len(pv.Rows))
	for _, r := range pv.Rows {
		reqs = append(reqs, ota.TemplateRequest{DeviceID: r.Device.ID, Command: r.Plan.Commands[0],
			Have: r.Device.Reading.Template.Fingerprint(), Want: pv.ReferenceTemplate.Fingerprint()})
	}
	how := string(policy.RefMostCommon)
	if req.Chosen {
		how = string(policy.RefChosen)
	}
	start := ota.TemplateStart{Group: string(req.Group.Family) + " " + req.Group.Name, Reference: req.Reference, How: how}
	id, skipped, err := m.o.Engine.SubmitTemplate(ctx, actor, start, reqs)
	var ref []Refusal
	for _, s := range skipped {
		ref = append(ref, Refusal{DeviceID: s.DeviceID, Reason: s.Reason})
	}
	if errors.Is(err, ota.ErrNothingToFix) {
		return 0, ref, ErrNothingToApply
	}
	return id, ref, err
}

// NewApplier returns the device side of the template jobs; its readings also refresh the cache of the tab.
func (m *Manager) NewApplier() *Applier {
	return &Applier{Client: m.o.Client, Settle: 6 * time.Second, Now: m.o.Now,
		OnRead: func(id model.DeviceID, r reading) { m.cache.put(id, r) }}
}

// SetEngine gives the Manager the engine that runs the transactions; it exists only after the Manager
// (the engine needs the Manager's applier). Call it once, before the server starts.
func (m *Manager) SetEngine(e Submitter) { m.o.Engine = e }
