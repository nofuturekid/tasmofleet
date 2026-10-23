package app

import (
	"context"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

const (
	metricsEvery   = 30 * time.Second
	retentionEvery = 24 * time.Hour
	healthKeep     = 90 * 24 * time.Hour
	backupGap      = 2 * time.Second
)

// startBackground starts the loops that are not request-driven: metrics
// gauges, scheduled backups and retention pruning.
func (a *runtime) startBackground() {
	a.goBG("metrics", func() error { a.loop(metricsEvery, a.updateMetrics); return nil })
	a.goBG("retention", func() error { a.loop(retentionEvery, a.prune); return nil })
	a.setupSysConfigLive() // backup schedule and scheduled scan follow the system settings page
}

// loop runs f now and then every interval until the background context ends.
func (a *runtime) loop(every time.Duration, f func(context.Context)) {
	f(a.bgContext)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-a.bgContext.Done():
			return
		case <-t.C:
			f(a.bgContext)
		}
	}
}

func (a *runtime) updateMetrics(ctx context.Context) {
	std := a.svc.Std
	std.Devices.Reset()
	counts := map[model.HealthState]int{}
	devs, err := a.svc.Inventory.List(ctx)
	if err != nil {
		a.log.Warn("metrics: list devices", "err", err)
		return
	}
	all := a.tracker.All()
	for _, d := range devs {
		st := model.HealthUnknown
		if h, ok := all[d.ID]; ok {
			st = h.State
		}
		counts[st]++
	}
	for _, st := range []model.HealthState{model.HealthOnline, model.HealthStale, model.HealthOffline, model.HealthAuthFailed, model.HealthUnknown} {
		std.Devices.With(string(st)).Set(float64(counts[st]))
	}
	connected := 0.0
	if a.bus != nil && a.bus.Connected() {
		connected = 1
	}
	std.MQTTConnected.With().Set(connected)

	// Per-device gauges come from the health cache; a nil slice clears them.
	var samples []metrics.DeviceSample
	if a.svc.Sys.Snapshot().MetricsDeviceLabels {
		samples = deviceSamples(devs, all, a.tracker.Telemetry())
	}
	std.SetDevices(samples)

	std.BackupLastOK.Reset()
	oldest := int64(0) // without device labels: the oldest "latest backup" of the fleet
	for _, d := range devs {
		bs, err := a.backups.List(ctx, d.ID)
		if err != nil || len(bs) == 0 {
			continue
		}
		ts := bs[0].TakenAt.Unix()
		if a.svc.Sys.Snapshot().MetricsDeviceLabels {
			std.BackupLastOK.With(d.Name).Set(float64(ts))
		} else if oldest == 0 || ts < oldest {
			oldest = ts
		}
	}
	if !a.svc.Sys.Snapshot().MetricsDeviceLabels && oldest != 0 {
		std.BackupLastOK.With("fleet").Set(float64(oldest))
	}
	if a.cache != nil {
		std.FirmwareCache.With().Set(float64(dirSize(filepath.Join(a.cfg.DataDir, "firmware"))))
	}
	if jobs, err := a.svc.Store.ActiveJobs(ctx); err == nil {
		std.OTAJobsActive.With().Set(float64(len(jobs)))
	}
}

// deviceSamples joins the inventory with the health cache. Pure: no I/O.
func deviceSamples(devs []model.Device, all map[model.DeviceID]model.Health, tel map[model.DeviceID]health.Telemetry) []metrics.DeviceSample {
	out := make([]metrics.DeviceSample, 0, len(devs))
	for _, d := range devs {
		h, ok := all[d.ID]
		smp := metrics.DeviceSample{ID: int64(d.ID), Name: d.Name, Unknown: !ok || h.State == model.HealthUnknown}
		if smp.Unknown {
			out = append(out, smp)
			continue
		}
		smp.Online = h.State == model.HealthOnline
		smp.LastSeen, smp.UptimeSec, smp.RSSI = h.LastSeen, h.UptimeSec, h.RSSI
		smp.Relays = map[string]bool{}
		for k, v := range h.Power {
			n := strings.TrimPrefix(k, "POWER")
			if n == "" {
				if _, both := h.Power["POWER1"]; both {
					continue // POWERn wins over the bare POWER of the same relay
				}
				n = "1"
			}
			if i, err := strconv.Atoi(n); err != nil || i < 1 || i > metrics.MaxIndex {
				continue // only POWER, POWER1..POWER8 become series
			}
			smp.Relays[n] = strings.EqualFold(v, "ON")
		}
		t := tel[d.ID]
		if t.HasSignal {
			sig := t.SignalDBm
			smp.SignalDBm = &sig
		}
		if len(t.Energy) > 0 {
			smp.Energy = make(map[string]metrics.EnergySample, len(t.Energy))
			for ch, e := range t.Energy {
				smp.Energy[ch] = metrics.EnergySample{PowerW: e.Power, TodayKWh: e.Today, YesterdayKWh: e.Yesterday, TotalKWh: e.Total}
			}
		}
		out = append(out, smp)
	}
	return out
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error { //nolint:errcheck // best-effort gauge; unreadable entries count as zero
		if err == nil && !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func (a *runtime) prune(ctx context.Context) {
	now := time.Now()
	if n, err := a.svc.Store.PruneAudit(ctx, now.Add(-a.svc.Sys.Snapshot().AuditKeep)); err != nil {
		a.log.Warn("prune audit", "err", err)
	} else if n > 0 {
		a.log.Info("pruned audit events", "n", n)
	}
	if _, err := a.svc.Store.PruneBackupAttempts(ctx, now.Add(-a.svc.Sys.Snapshot().AuditKeep)); err != nil {
		a.log.Warn("prune failed backup attempts", "err", err)
	}
	// Transactions and their logs follow the audit retention (one setting for "how long the
	// history is kept"); running ones are never pruned (store.PruneJobs).
	if n, err := a.svc.Store.PruneJobs(ctx, now.Add(-a.svc.Sys.Snapshot().AuditKeep)); err != nil {
		a.log.Warn("prune transactions", "err", err)
	} else if n > 0 {
		a.log.Info("pruned finished update jobs", "n", n)
	}
	if _, err := a.svc.Store.PruneHealthEvents(ctx, now.Add(-healthKeep)); err != nil {
		a.log.Warn("prune health events", "err", err)
	}
	if _, err := a.svc.Store.DeleteExpiredSessions(ctx, now); err != nil {
		a.log.Warn("prune sessions", "err", err)
	}
}

// backupAll backs up every device one after the other (BACKUP_INTERVAL).
func (a *runtime) backupAll(ctx context.Context) {
	devs, err := a.svc.Inventory.List(ctx)
	if err != nil {
		a.log.Warn("scheduled backup: list devices", "err", err)
		return
	}
	for _, d := range devs {
		if ctx.Err() != nil {
			return
		}
		if _, err := a.backups.Backup(backup.WithRun(ctx, backup.Run{Trigger: model.TriggerScheduled}), "system", d.ID); err != nil {
			a.log.Warn("scheduled backup failed", "device", d.Name, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backupGap):
		}
	}
}
