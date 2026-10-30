package app

import (
	"context"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// httpFreshFor is how long a successful HTTP call counts as "HTTP works" (D2).
const httpFreshFor = 15 * time.Minute

// HTTPMemory is the router's memory of HTTP calls that failed (WP-A2 implements it): why and since
// when the last calls to a device failed, ok=false when they did not or it was cleared by a success.
type HTTPMemory interface {
	HTTPFailure(id model.DeviceID) (kind transport.HTTPFailKind, since time.Time, ok bool)
}

type factsDevices interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	ListDevices(ctx context.Context) ([]model.Device, error)
}

type factsHealth interface {
	Get(id model.DeviceID) model.Health
}

// TransportFacts gathers the transport.Facts of a device from the inventory, health, the router's
// HTTP-failure memory, the bus and the live settings. It only reads.
type TransportFacts struct {
	Devices factsDevices
	Health  factsHealth
	Memory  HTTPMemory // nil: no failure is ever known
	// BusConnected, FullTopic (MQTT_FULLTOPIC) and FirmwareBaseURL (FIRMWARE_BASE_URL) are read per
	// call: the settings page changes them live.
	BusConnected    func() bool
	FullTopic       func() string
	FirmwareBaseURL func() string
	// BrokerPassword is TasmoFleet's MQTT_PASSWORD; devices in broker mode send it.
	BrokerPassword func() string
	// CommandPrefix is the command prefix of the bus (mqttbus Options.Prefixes[0]); "" reads as cmnd.
	CommandPrefix func() string
	Now           func() time.Time
}

var (
	_ transport.FactsSource = (*TransportFacts)(nil)
	_ transport.DeviceFacts = (*TransportFacts)(nil) // what the router uses
)

// UpdateRequest is what a person asked for when an update is planned.
type UpdateRequest struct {
	Delivery        model.Delivery
	AllowMQTTUpload bool
	ImageSize       int64 // the image the update would send, for the upload fit
	UpLayout        fwselect.UpLayoutHint
	// HTTPPush and HTTPPushNote are the OTA engine's decision for a device HTTP reaches (see
	// transport.Request).
	HTTPPush     transport.Path
	HTTPPushNote string
}

// Facts implements transport.FactsSource; the Request part stays empty.
func (s *TransportFacts) Facts(ctx context.Context, id model.DeviceID) (transport.Facts, error) {
	d, err := s.Devices.GetDevice(ctx, id)
	if err != nil {
		return transport.Facts{}, fmt.Errorf("transport facts: %w", err)
	}
	return s.FactsFor(ctx, d)
}

// HTTPState is the cheap part of the facts (the router's memory and the health service, no
// store): the router asks it before every call to decide whether HTTP is worth trying.
func (s *TransportFacts) HTTPState(id model.DeviceID) transport.HTTPState {
	return s.httpState(id, s.Health.Get(id), s.Now())
}

// FactsFor is Facts for a device snapshot the caller holds: the router decides and sends from
// the same snapshot.
func (s *TransportFacts) FactsFor(ctx context.Context, d model.Device) (transport.Facts, error) {
	all, err := s.Devices.ListDevices(ctx)
	if err != nil {
		return transport.Facts{}, fmt.Errorf("transport facts: %w", err)
	}
	return s.factsFor(ctx, d, all)
}

// factsFor is FactsFor with the inventory the caller already listed.
func (s *TransportFacts) factsFor(_ context.Context, d model.Device, all []model.Device) (transport.Facts, error) {
	id := d.ID
	h := s.Health.Get(id)
	now := s.Now()

	probe := lastProbe(d)
	return transport.Facts{
		Now:  now,
		HTTP: s.httpState(id, h, now),
		MQTT: transport.MQTTFacts{
			BusConnected: s.BusConnected(), Ours: d.MQTT == model.MQTTOurs,
			Topic: d.Topic, TopicExclusive: topicExclusive(d, all, s.commandPrefix()),
			OthersGroupUnread: othersGroupUnread(d, all, func(id model.DeviceID) bool { return s.Health.Get(id).LWT == "Offline" }),
			FullTopic:         d.FullTopic, OurFullTopic: s.FullTopic(),
			GroupTopic: d.GroupTopic, GroupTopicReadAt: d.GroupTopicReadAt, LastProbeAt: probe,
			LWTOnline: h.LWT == "Online",
		},
		Password:        s.password(d),
		FWReach:         d.FWReach,
		FirmwareBaseURL: s.FirmwareBaseURL() != "",
		Identity:        d.Identity,
	}, nil
}

// ForUpdate is Facts plus the request of an update: the upload fit is computed here, from the
// image size, so Choose stays pure.
func (s *TransportFacts) ForUpdate(ctx context.Context, id model.DeviceID, r UpdateRequest) (transport.Facts, error) {
	f, err := s.Facts(ctx, id)
	if err != nil {
		return f, err
	}
	ok, need, room, why := fwselect.MQTTUploadFits(f.Identity, r.UpLayout, r.ImageSize)
	f.Request = transport.Request{
		Delivery: r.Delivery, AllowMQTTUpload: r.AllowMQTTUpload,
		MQTTUploadFit: transport.MQTTFit{OK: ok, NeedKB: need, RoomKB: room, Reason: why},
		HTTPPush:      r.HTTPPush, HTTPPushNote: r.HTTPPushNote,
	}
	return f, nil
}

// httpState: a remembered failure wins unless a successful HTTP call came after it began; else a
// success within 15 minutes is ok; else unknown (which Choose treats as "try").
func (s *TransportFacts) httpState(id model.DeviceID, h model.Health, now time.Time) transport.HTTPState {
	if s.Memory != nil {
		if kind, since, ok := s.Memory.HTTPFailure(id); ok && !h.SeenHTTP.After(since) {
			return transport.HTTPState{State: transport.HTTPFailing, Kind: kind, Since: since}
		}
	}
	if !h.SeenHTTP.IsZero() && now.Sub(h.SeenHTTP) < httpFreshFor {
		return transport.HTTPState{State: transport.HTTPOK}
	}
	return transport.HTTPState{}
}

// password: a device in broker mode sends MQTT_PASSWORD, so without one it has no usable password.
func (s *TransportFacts) password(d model.Device) transport.Password {
	p := transport.Password{Mode: d.MQTTPasswordMode, BadAt: d.MQTTPasswordBadAt}
	if d.MQTTPasswordMode == model.MQTTPasswordBroker && s.BrokerPassword() == "" {
		p.Unusable = "TasmoFleet has no broker password configured"
	}
	return p
}

func (s *TransportFacts) commandPrefix() string {
	if s.CommandPrefix != nil {
		if p := s.CommandPrefix(); p != "" {
			return p
		}
	}
	return "cmnd"
}

// topicExclusive reports whether no device in the inventory listens on d's command stem: not as its
// Topic, not as its GroupTopic (fresh or stale, d's own included). Stems are compared the way
// Tasmota subscribes (stem + "/#"): equal or one a level prefix of the other. d's own FullTopic must
// expand (contain %topic%, no other placeholder). Another device whose stem cannot be known (a
// FullTopic without %topic%, or with %hostname%, %id%, ...) makes the answer false: unknown is not
// exclusive. A device with neither Topic nor FullTopic is HTTP only and listens on nothing.
// Not seen: GroupTopic2-4 (only GroupTopic is stored) and devices outside the inventory.
func topicExclusive(d model.Device, all []model.Device, prefix string) bool {
	mine, ok := transport.CommandTopic(d.FullTopic, d.Topic, prefix)
	if d.Topic == "" || !ok {
		return false
	}
	for _, o := range all {
		if o.Topic == "" && o.FullTopic == "" && o.GroupTopic == "" {
			continue
		}
		if o.ID != d.ID {
			if o.Topic == d.Topic {
				return false
			}
			x, ok := transport.CommandTopic(o.FullTopic, o.Topic, prefix)
			if !ok || transport.StemsOverlap(x, mine) {
				return false
			}
		}
		if o.GroupTopic != "" {
			if o.GroupTopic == d.Topic {
				return false
			}
			x, ok := transport.CommandTopic(o.FullTopic, o.GroupTopic, prefix)
			if !ok || transport.StemsOverlap(x, mine) {
				return false
			}
		}
	}
	return true
}

// lastProbe is the time of the device's last Status 1 read or identity detection.
func lastProbe(d model.Device) time.Time {
	probe := d.OTAURLReadAt // the Status 1 stamp
	if d.Identity.DetectedAt.After(probe) {
		probe = d.Identity.DetectedAt
	}
	return probe
}

// groupTopicStale says the device's GroupTopic is not known: never read, empty, or older than the
// device's last probe. The one rule for the device's own GroupTopic (transport.groupTopicKnown), for
// the other devices' (othersGroupUnread) and for the background reader.
func groupTopicStale(d model.Device) bool {
	return d.GroupTopic == "" || d.GroupTopicReadAt.IsZero() || d.GroupTopicReadAt.Before(lastProbe(d))
}

// othersGroupUnread counts the other devices with an MQTT topic whose GroupTopic is not known. A device
// whose last will is Offline (the broker confirmed it disconnected, not merely "unknown") is left out:
// our commands are never published retained, so it cannot receive one and cannot be in a group that a
// message now would reach; one dead device must not block MQTT writes for the whole fleet. Its topic and
// stale GroupTopic still count for TopicExclusive, which stays fail-closed.
func othersGroupUnread(d model.Device, all []model.Device, offline func(model.DeviceID) bool) int {
	n := 0
	for _, o := range all {
		if o.ID != d.ID && o.Topic != "" && groupTopicStale(o) && !offline(o.ID) {
			n++
		}
	}
	return n
}
