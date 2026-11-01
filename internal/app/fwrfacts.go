package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Status 2 facts for devices identified before 0.22.0 (#417). The StatusFWR facts (core, SDK, CPU
// frequency, build date, CR) came with 0.22.0 as a new column without a backfill. Status 2 is read
// automatically only at adoption, by the MQTT background ask for devices no probe has identified, on
// an OTA preflight and by "Refresh identity", so a device probed earlier says "Most of Status 2 is
// not read yet" for good. This reader fills those gaps: devices with identity_at set and no facts,
// one Status 2 per tick on the whole process (so at most one per second on the bus, like the
// background identity ask), one per device per ten minutes, only while the device is online. The
// router sends it over HTTP while HTTP works and over the MQTT command channel when it does not.
// It is a read (never Status 0) and it writes through store.UpdateFWRFacts, which touches the facts
// column only: the identity fields stay as the probe left them.
const (
	fwrFactsTick    = 2 * time.Second
	fwrFactsTimeout = 20 * time.Second
	fwrFactsEvery   = mqttIDEvery // between two reads of one device, failed or not
	fwrFactsIdle    = 30 * time.Second
)

type fwrFactsStore interface {
	UpdateFWRFacts(ctx context.Context, id model.DeviceID, g store.ProbeGuard, f model.FWRFacts) error
}

type fwrFactsReader struct {
	list      func(context.Context) ([]model.Device, error)
	client    tasmota.Client // the router
	store     fwrFactsStore
	health    func(model.DeviceID) model.Health
	now       func() time.Time
	log       *slog.Logger
	tried     map[model.DeviceID]time.Time
	idleUntil time.Time
	announce  func(model.DeviceID)
}

func newFWRFactsReader(list func(context.Context) ([]model.Device, error), health func(model.DeviceID) model.Health, client tasmota.Client,
	st fwrFactsStore, announce func(model.DeviceID), log *slog.Logger) *fwrFactsReader {
	return &fwrFactsReader{list: list, health: health, client: client, store: st, announce: announce, now: time.Now, log: log,
		tried: map[model.DeviceID]time.Time{}}
}

func (r *fwrFactsReader) run(ctx context.Context) {
	t := time.NewTicker(fwrFactsTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// wantsFWRFacts: an HTTP probe identified the device, the facts are empty, and the firmware can
// answer Status 2 (minimal and safeboot images cannot).
func wantsFWRFacts(d model.Device) bool {
	i := d.Identity
	return !i.DetectedAt.IsZero() && !i.FWR.Read() && i.Variant != "minimal" && i.Variant != "safeboot"
}

// tick reads one device: the one that has waited longest (never tried first) among those that are
// online and not tried within fwrFactsEvery.
func (r *fwrFactsReader) tick(ctx context.Context) {
	now := r.now()
	if now.Before(r.idleUntil) {
		return
	}
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("fwr facts reader: list devices", "err", err)
		return
	}
	inList := make(map[model.DeviceID]bool, len(devs))
	wanting := false
	var pick *model.Device
	for i := range devs {
		d := &devs[i]
		inList[d.ID] = true
		if !wantsFWRFacts(*d) {
			delete(r.tried, d.ID)
			continue
		}
		wanting = true
		if r.health(d.ID).State != model.HealthOnline {
			continue // nothing could answer
		}
		last, was := r.tried[d.ID]
		if was && now.Sub(last) < fwrFactsEvery {
			continue
		}
		if pick == nil || last.Before(r.tried[pick.ID]) {
			pick = d
		}
	}
	for id := range r.tried {
		if !inList[id] {
			delete(r.tried, id) // deleted from the inventory
		}
	}
	if pick == nil {
		if !wanting {
			r.idleUntil = now.Add(fwrFactsIdle)
		}
		return
	}
	r.tried[pick.ID] = now
	if r.read(ctx, *pick) {
		delete(r.tried, pick.ID)
	}
}

// read puts Status 2 to d and stores its facts; it reports whether that worked.
func (r *fwrFactsReader) read(ctx context.Context, d model.Device) bool {
	ctx, cancel := context.WithTimeout(ctx, fwrFactsTimeout)
	defer cancel()
	if d.Address == "" {
		ctx = transport.WithPath(ctx, transport.MQTT) // added from the broker: there is no HTTP address to try
	}
	raw, err := r.client.Command(ctx, tasmota.TargetFor(d), "Status 2")
	if err != nil {
		r.log.Debug("fwr facts reader: Status 2 not read", "device", d.ID, "err", err)
		return false
	}
	ident, err := fwselect.Detect(fwselect.Inputs{Status2: raw})
	if err != nil || ident.Variant == "minimal" || ident.Variant == "safeboot" || !ident.FWR.Read() {
		r.log.Debug("fwr facts reader: Status 2 reply not usable", "device", d.ID, "err", err)
		return false
	}
	g := store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}
	if err := r.store.UpdateFWRFacts(ctx, d.ID, g, ident.FWR); err != nil {
		if !errors.Is(err, store.ErrConflict) { // a probe or an edit got there first: that is fine
			r.log.Warn("fwr facts reader: could not store the facts", "device", d.ID, "err", err)
		}
		return false
	}
	r.log.Info("fwr facts reader: Status 2 facts read", "device", d.ID)
	if r.announce != nil {
		r.announce(d.ID)
	}
	return true
}
