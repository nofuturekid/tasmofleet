package app

import (
	"context"
	"errors"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// scanConcurrency and scanTimeout are the spec limits of the HTTP autoscan.
const (
	scanConcurrency = 16
	scanTimeout     = 2 * time.Second
)

// setupDiscovery starts MQTT discovery (with a broker) and prepares the
// admin-started HTTP scan. The scan has its own device client: the shared one
// allows 8 requests in flight and 10 s per request, the scan needs 16 and 2 s.
func (a *runtime) setupDiscovery() error {
	scanClient := tasmota.NewHTTPClient(tasmota.Options{
		Networks: a.svc.Networks, Timeout: scanTimeout, MaxConcurrent: scanConcurrency, Logger: a.log,
	})
	var asker discovery.StatusAsker
	if a.mqttCmd != nil {
		asker = a.mqttCmd // not a nil *Channel in an interface
	}
	m, err := discovery.New(discovery.Options{
		Asker: asker, Inventory: a.svc.Inventory, Store: a.svc.Store, Audit: a.svc.Audit, Client: scanClient,
		Verifier: macVerifier{client: a.svc.HTTP}, NetworkSet: a.svc.Networks,
		Concurrency: scanConcurrency, ProbeTimeout: scanTimeout, Logger: a.log,
		OnDropped:  func(kind string) { a.svc.Std.DiscoveryDropped.With(kind).Inc() },
		OnAccepted: a.identifyLater,
	})
	if err != nil {
		return err
	}
	a.discovery = m
	a.discoveryFollowsSetting()
	a.goBG("discovery", func() error { return m.Run(a.bgContext) })
	return nil
}

// identifyTimeout bounds the first probe of an accepted device.
const identifyTimeout = 30 * time.Second

// identifyLater probes a device created from a proposal, which carries no identity yet, and
// stores firmware, module and MQTT facts. It returns at once; the prober announces the result
// to open pages. A failure is not fatal: the device keeps an empty identity until a manual
// "Refresh identity".
func (a *runtime) identifyLater(id model.DeviceID) {
	a.bgWG.Go(func() {
		ctx, cancel := context.WithTimeout(a.bgContext, identifyTimeout)
		defer cancel()
		// Automatic (actor ""): it binds no MAC (discovery D8).
		if err := a.svc.Prober.Refresh(ctx, id, ""); err != nil && a.bgContext.Err() == nil && !errors.As(err, new(*TopicKeptError)) {
			a.log.Warn("discovery: first probe of the accepted device failed", "device", id, "err", err)
		}
	})
}
