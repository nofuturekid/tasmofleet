package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Reading every device's GroupTopic (#79, review #279). A message to a device's topic reaches every
// device that listens on it, so tasmofleet writes or transfers over MQTT only when it knows that no
// other device has the topic as its GroupTopic. One device with an unread GroupTopic therefore blocks
// MQTT writes for every MQTT-routed device (transport.MQTTFacts.OthersGroupUnread), and the reader
// closes that gap without a person doing anything: shortly after start and after a device is adopted,
// one Status 1 per tick, through the router. The router sends it over HTTP while HTTP works and over
// the MQTT command channel when it does not (a read is allowed there with an unknown GroupTopic).
// Status 1 is individual and read-only, never Status 0; one read per tick on the whole process. A
// device that gave no usable answer is asked again after groupTopicBackoff[n]: soon at first (right
// after start it may not be known as ours on the broker yet), then rarely.
const (
	groupTopicTick    = 2 * time.Second
	groupTopicTimeout = 20 * time.Second
)

var groupTopicBackoff = [...]time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 15 * time.Minute}

// groupTopicIdle is how long the reader does not even list the inventory after a tick that found no
// device in need of a read.
const groupTopicIdle = 30 * time.Second

// groupTry is the record of the reads that gave nothing.
type groupTry struct {
	at time.Time
	n  int  // failed reads so far
	up bool // the device was online when it was tried
}

func (g groupTry) wait() time.Duration {
	return groupTopicBackoff[max(0, min(g.n-1, len(groupTopicBackoff)-1))]
}

type groupTopicReader struct {
	list   func(context.Context) ([]model.Device, error)
	client tasmota.Client // the router
	store  interface {
		UpdateGroupTopicFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, groupTopic string) error
	}
	health    func(model.DeviceID) model.Health
	now       func() time.Time
	log       *slog.Logger
	tried     map[model.DeviceID]groupTry
	idleUntil time.Time
}

func newGroupTopicReader(list func(context.Context) ([]model.Device, error), health func(model.DeviceID) model.Health, client tasmota.Client, st interface {
	UpdateGroupTopicFromMQTT(ctx context.Context, id model.DeviceID, g store.ProbeGuard, topic, groupTopic string) error
}, log *slog.Logger) *groupTopicReader {
	return &groupTopicReader{list: list, health: health, client: client, store: st, now: time.Now, log: log, tried: map[model.DeviceID]groupTry{}}
}

func (r *groupTopicReader) run(ctx context.Context) {
	t := time.NewTicker(groupTopicTick)
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

// wantsGroupTopic: the device has an MQTT topic and its GroupTopic is not known (groupTopicStale).
func wantsGroupTopic(d model.Device) bool { return d.Topic != "" && groupTopicStale(d) }

// tick reads one device: a never tried one before a retry that is due, else the one that has waited
// longest. Offline devices are skipped (nothing could answer), and a device that was tried while it was
// not online is read again at once when it comes online.
func (r *groupTopicReader) tick(ctx context.Context) {
	now := r.now()
	if now.Before(r.idleUntil) {
		return
	}
	devs, err := r.list(ctx)
	if err != nil {
		r.log.Warn("group topic reader: list devices", "err", err)
		return
	}
	inList := make(map[model.DeviceID]bool, len(devs))
	wanting := false
	var pick *model.Device
	for i := range devs {
		d := &devs[i]
		inList[d.ID] = true
		if !wantsGroupTopic(*d) {
			delete(r.tried, d.ID)
			continue
		}
		wanting = true
		h := r.health(d.ID)
		if h.State == model.HealthOffline {
			continue
		}
		up := h.State == model.HealthOnline
		last, was := r.tried[d.ID]
		if was && up && !last.up {
			delete(r.tried, d.ID) // it came online since the failed attempt: read it now
			was, last = false, groupTry{}
		}
		if was && now.Sub(last.at) < last.wait() {
			continue
		}
		if pick == nil || last.at.Before(r.tried[pick.ID].at) {
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
			r.idleUntil = now.Add(groupTopicIdle)
		}
		return
	}
	t := r.tried[pick.ID]
	r.tried[pick.ID] = groupTry{at: now, n: t.n + 1, up: r.health(pick.ID).State == model.HealthOnline}
	if r.read(ctx, *pick) {
		delete(r.tried, pick.ID)
	}
}

// read puts Status 1 to d and stores its GroupTopic; it reports whether that worked.
func (r *groupTopicReader) read(ctx context.Context, d model.Device) bool {
	ctx, cancel := context.WithTimeout(ctx, groupTopicTimeout)
	defer cancel()
	if d.Address == "" {
		ctx = transport.WithPath(ctx, transport.MQTT) // added from the broker: there is no HTTP address to try
	}
	raw, err := r.client.Command(ctx, tasmota.TargetFor(d), "Status 1")
	if err != nil {
		r.log.Debug("group topic reader: Status 1 not read", "device", d.ID, "err", err)
		return false
	}
	gt, ok := parseGroupTopic(raw)
	if !ok {
		r.log.Debug("group topic reader: Status 1 has no usable GroupTopic", "device", d.ID)
		return false
	}
	g := store.ProbeGuard{Address: d.Address, Port: d.Port, MAC: d.MAC}
	if err := r.store.UpdateGroupTopicFromMQTT(ctx, d.ID, g, d.Topic, gt); err != nil && !errors.Is(err, store.ErrConflict) {
		r.log.Warn("group topic reader: could not store the GroupTopic", "device", d.ID, "err", err)
		return false
	}
	return true
}
