package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// Why: transactions were never pruned. The daily retention follows the audit retention setting
// (AuditKeep, one knob for how long the history is kept): a finished transaction older than it
// goes, a running one stays however old it is.
func TestRetentionPrunesFinishedTransactionsByTheAuditSetting(t *testing.T) {
	cfg, err := config.Load([]string{"TASMOFLEET_DATA_DIR=/nonexistent-tasmofleet-test", "TASMOFLEET_CONFIG_FILE=/dev/null", "TASMOFLEET_AUTH_MODE=header", "TASMOFLEET_TRUSTED_PROXIES=172.31.12.20/32", "TASMOFLEET_OPERATOR_GROUPS=ops"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.AuditKeep = time.Millisecond // everything finished before "now" is past the retention
	sys, err := sysconfig.New(context.Background(), sysconfig.Options{Base: cfg, Store: &memSettings{rows: map[string]store.SystemSetting{}}, Audit: nopAudit{}})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	box, err := secretbox.LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() }) // temp DB
	ctx := context.Background()
	done, err := st.CreateDevice(ctx, model.Device{Name: "done", Address: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	busy, err := st.CreateDevice(ctx, model.Device{Name: "busy", Address: "192.0.2.2"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(dev model.DeviceID, to model.JobState) {
		j := model.Job{DeviceID: dev, State: model.JobQueued, Stage: model.StageFinal, Policy: model.PolicyOnlyNewer, TargetRelease: "16.0.0", TargetVariant: "tasmota"}
		_, ids, err := st.CreateBatch(ctx, "u", []model.Job{j})
		if err != nil {
			t.Fatal(err)
		}
		j.ID, j.State = ids[0], to
		if err := st.SaveTransition(ctx, j, model.JobEvent{JobID: ids[0], From: model.JobQueued, To: to, Event: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	mk(done, model.JobSucceeded)
	mk(busy, model.JobAwaitReboot)
	time.Sleep(20 * time.Millisecond)

	a := &runtime{svc: &Services{Sys: sys, Store: st}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.prune(ctx)

	left, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].DeviceID != busy {
		t.Fatalf("after the retention: %+v; want only the running job", left)
	}
}
