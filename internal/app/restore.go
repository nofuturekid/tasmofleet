package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// setupRestore builds the restore service. A restore over HTTP needs the firmware listener
// (devices fetch the dump from it) and so FIRMWARE_BASE_URL; one over MQTT (FileUpload) does not.
// Which one a device gets is decided per restore from the transport facts. Restores that were
// running when the process stopped are failed: their one-time tokens died with it.
func (a *runtime) setupRestore() error {
	if a.backups == nil {
		return nil
	}
	n, err := a.svc.Store.FailActiveRestores(context.Background(), "interrupted by a restart of tasmofleet", restore.InterruptedDetails())
	if err != nil {
		return fmt.Errorf("restore recovery: %w", err)
	}
	if n > 0 {
		a.log.Warn("restores interrupted by the restart were marked failed", "count", n)
	}
	m, err := restore.New(restore.Options{
		Devices: a.svc.Store, Backups: a.backups, Files: a.backups, Store: a.svc.Store, Client: a.svc.Client,
		BaseURL: a.cfg.FWBaseURL, Uploader: a.svc.Router, PathFor: a.restorePath, BrokerPassword: func() string { return a.svc.Sys.Snapshot().MQTTPassword }, Audit: a.svc.Audit, Log: a.log, Context: a.bgContext,
		Locks: a.deviceLocks(), Jobs: a.svc.Store,
		Hardware: a.svc.Store, Describe: a.svc.Prober.Describe,
		OnServed: func(id model.DeviceID) {
			a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
				return l.NoteYes(ctx, id, FWReachSourceRestoreToken)
			})
		},
	})
	if err != nil {
		return err
	}
	a.restore = m
	a.goBG("restore", func() error { <-a.bgContext.Done(); m.Wait(); return nil })
	return nil
}

// restorePath is transport.Choose's verdict for a restore of d: HTTP (WebGetConfig), MQTT (FileUpload)
// or None, with the sentence why. Without facts (nothing to judge) the established path, HTTP.
func (a *runtime) restorePath(ctx context.Context, d model.Device) (transport.Path, string) {
	if a.facts == nil {
		return transport.HTTP, ""
	}
	f, err := a.facts.FactsFor(ctx, d)
	if err != nil {
		return transport.None, "the transport facts of the device could not be read: " + err.Error()
	}
	p := transport.Choose(f)
	return p.Restore, p.Reason(transport.ActRestore)
}

// restoreHandler is mounted at /restore/ on the firmware listener (nil when
// restore is off).
func (a *runtime) restoreHandler() http.Handler {
	if a.restore == nil {
		return nil
	}
	return a.restore.Handler()
}

// deviceLocks is the per-device operation lock set. Restore uses it now; OTA,
// settings and console must take the same locks (see docs/security/restore.md).
func (a *runtime) deviceLocks() *devlock.Locks {
	if a.devLocks == nil {
		a.devLocks = devlock.New()
	}
	return a.devLocks
}
