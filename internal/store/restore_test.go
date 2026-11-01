package store

import (
	"context"
	"errors"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Why: two restores racing on one device would overwrite each other's
// config; the database, not the service, is what makes "one at a time" hold.
func TestRestoreAllowsOneUnfinishedJobPerDevice(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	dev, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	id, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 3, Actor: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 4, Actor: "bob"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active job: %v", err)
	}
	j, _ := s.GetRestore(ctx, id)
	j.State, j.Detail, j.Verified = RestoreDone, "ok", true
	if err := s.UpdateRestore(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 4, Actor: "bob"}); err != nil {
		t.Fatalf("after a terminal job a new one must be allowed: %v", err)
	}
	list, _ := s.ListRestores(ctx, dev, 0)
	if len(list) != 2 || list[0].BackupID != 4 || !list[1].Verified || list[1].State != RestoreDone {
		t.Errorf("list: %+v", list)
	}
}

// Why: a crash mid-restore must not block the device forever; start-up
// recovery fails the orphaned job, and finished jobs stay untouched.
func TestFailActiveRestores(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	d1, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	d2, _ := s.CreateDevice(ctx, model.Device{Name: "b", Address: "10.0.0.2"})
	a, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d1, BackupID: 1, Actor: "x"})
	b, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d2, BackupID: 2, Actor: "x"})
	jb, _ := s.GetRestore(ctx, b)
	jb.State = RestoreDone
	if err := s.UpdateRestore(ctx, jb); err != nil {
		t.Fatal(err)
	}
	n, err := s.FailActiveRestores(ctx, "interrupted by restart", nil)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	ja, _ := s.GetRestore(ctx, a)
	if ja.State != RestoreFailed || ja.Detail != "interrupted by restart" {
		t.Errorf("%+v", ja)
	}
	jb, _ = s.GetRestore(ctx, b)
	if jb.State != RestoreDone {
		t.Errorf("finished job changed: %+v", jb)
	}
	if _, err := s.GetRestore(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing job: %v", err)
	}
}

// Why (#324): a restore interrupted while it writes files (state "files") is an unfinished job
// like any other: it still blocks a second restore of the device (partial uploads must not be
// raced), and after a restart of TasmoFleet start-up recovery fails it instead of leaving it
// stuck. The state column is free text, so this proves no schema list excludes the new state.
func TestRestoreInFilesStateIsActiveAndRecoveredAfterARestart(t *testing.T) {
	s, _, _ := newStore(t)
	ctx := context.Background()
	d, _ := s.CreateDevice(ctx, model.Device{Name: "a", Address: "10.0.0.1"})
	id, _ := s.CreateRestore(ctx, RestoreJob{DeviceID: d, BackupID: 1, Actor: "x"})
	j, _ := s.GetRestore(ctx, id)
	j.State, j.Detail = RestoreFiles, "writing lib/x.be (2 of 3)"
	if err := s.UpdateRestore(ctx, j); err != nil {
		t.Fatal(err)
	}
	if j.Terminal() {
		t.Error("a job in the files state must not count as finished")
	}
	if _, err := s.CreateRestore(ctx, RestoreJob{DeviceID: d, BackupID: 2, Actor: "x"}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second restore while files are written: %v, want ErrConflict", err)
	}
	if n, err := s.FailActiveRestores(ctx, "interrupted by restart", nil); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got, _ := s.GetRestore(ctx, id); got.State != RestoreFailed {
		t.Errorf("state after recovery: %q", got.State)
	}
}
