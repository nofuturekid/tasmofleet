package app

import (
	"context"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// mqttPrefixes are the topic prefixes of the bus (cmnd, stat, tele). The bus is built from them and
// the transport facts expand the devices' command topics with the first one, so both use this one value.
var mqttPrefixes = [3]string{"cmnd", "stat", "tele"}

// setupTransport gives the router its facts and the MQTT command channel. Until here the router
// is the HTTP client it wraps (also in the CLI, which never calls this).
func (a *runtime) setupTransport() {
	facts := &TransportFacts{
		Devices: a.svc.Store, Health: a.tracker, Memory: a.svc.Router.Memory(),
		BusConnected:    a.bus.Connected,
		FullTopic:       func() string { return a.svc.Sys.Snapshot().MQTTFullTopic },
		FirmwareBaseURL: func() string { return a.svc.Sys.Snapshot().FWBaseURL },
		BrokerPassword:  func() string { return a.svc.Sys.Snapshot().MQTTPassword }, // live: a change on the settings page counts at once
		CommandPrefix:   func() string { return mqttPrefixes[0] },
		Now:             time.Now,
	}
	a.facts = facts
	a.svc.Router.SetFacts(facts)
	a.svc.Router.SetMQTT(a.mqttCmd)
	gr := newGroupTopicReader(a.svc.Inventory.List, a.tracker.Get, a.svc.Client, a.svc.Store, a.log)
	a.goBG("group-topic", func() error { gr.run(a.bgContext); return nil })
}

var _ transport.DeviceFacts = (*TransportFacts)(nil)

// onlyOverMQTT reports whether commands to any of the devices travel over MQTT because HTTP fails: a
// change of its MQTT identity can then cut it off for good (cmdpolicy.WithMQTTOnly). The inventory is
// listed once for all of them.
func (a *runtime) onlyOverMQTT(ctx context.Context, ids ...model.DeviceID) bool {
	all, err := a.facts.Devices.ListDevices(ctx)
	if err != nil {
		return false
	}
	want := make(map[model.DeviceID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	for _, d := range all {
		if !want[d.ID] {
			continue
		}
		f, err := a.facts.factsFor(ctx, d, all)
		if err == nil && transport.Choose(f).Commands == transport.MQTT {
			return true
		}
	}
	return false
}
