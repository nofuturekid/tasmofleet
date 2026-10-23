package store

import (
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// batchOf creates a batch with one job per device and moves every job to state, stamped at.
func batchOf(t *testing.T, s *SQLite, state model.JobState, at time.Time, devs ...model.DeviceID) (model.BatchID, []model.JobID) {
	t.Helper()
	var jobs []model.Job
	for _, d := range devs {
		jobs = append(jobs, job(d))
	}
	b, ids, err := s.CreateBatch(ctx, "u", jobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, "UPDATE ota_jobs SET state=?, updated_at=? WHERE id=?", string(state), toMS(at), id); err != nil {
			t.Fatal(err)
		}
	}
	return b, ids
}

func jobCount(t *testing.T, s *SQLite) (jobs, batches, events int) {
	t.Helper()
	for q, dst := range map[string]*int{"SELECT COUNT(*) FROM ota_jobs": &jobs, "SELECT COUNT(*) FROM ota_batches": &batches, "SELECT COUNT(*) FROM ota_job_events": &events} {
		if err := s.db.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	return
}

// Why: transactions and their per-device logs were never deleted, so the tables grew for ever
// while audit events and health events were pruned. Finished transactions older than the cut-off
// go with their logs; recent ones stay.
func TestPruneJobsRemovesOldFinishedTransactionsWithTheirLogs(t *testing.T) {
	s, _, _ := newStore(t)
	a, b := mustDevice(t, s, "a", "10.0.0.1"), mustDevice(t, s, "b", "10.0.0.2")
	cut := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	_, oldIDs := batchOf(t, s, model.JobSucceeded, cut.Add(-24*time.Hour), a, b)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO ota_job_events (job_id, at, from_state, to_state, event) VALUES (?,?,?,?,?)`, oldIDs[0], toMS(cut), "queued", "succeeded", "done"); err != nil {
		t.Fatal(err)
	}
	newBatch, _ := batchOf(t, s, model.JobFailed, cut.Add(24*time.Hour), a)
	n, err := s.PruneJobs(ctx, cut)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d jobs, want the 2 old ones", n)
	}
	jobs, batches, events := jobCount(t, s)
	if jobs != 1 || batches != 1 || events != 0 {
		t.Fatalf("left %d jobs, %d batches, %d events; want the recent transaction only and no orphaned log", jobs, batches, events)
	}
	if got, err := s.ListJobs(ctx, JobFilter{BatchID: newBatch}); err != nil || len(got) != 1 {
		t.Fatalf("the recent transaction is gone: %v %v", got, err)
	}
}

// Why: a running (or queued) transaction must survive any retention, however old it looks;
// and a transaction is never cut in half: one unfinished job keeps the whole batch.
func TestPruneJobsNeverDeletesRunningWorkOrHalfATransaction(t *testing.T) {
	s, _, _ := newStore(t)
	a, b := mustDevice(t, s, "a", "10.0.0.1"), mustDevice(t, s, "b", "10.0.0.2")
	long := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	_, ids := batchOf(t, s, model.JobSucceeded, long, a, b)
	if _, err := s.db.ExecContext(ctx, "UPDATE ota_jobs SET state=? WHERE id=?", string(model.JobAwaitReboot), ids[1]); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneJobs(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v) although one job of the batch still runs", n, err)
	}
	if jobs, _, _ := jobCount(t, s); jobs != 2 {
		t.Fatalf("%d jobs left, want both", jobs)
	}
}

// Why: a retry points at the transaction it retries. Pruning the original while the retry is
// kept would leave the retry's "retry of #N" dangling, so the original stays until the retry goes.
func TestPruneJobsKeepsATransactionWhoseRetryIsStillKept(t *testing.T) {
	s, _, _ := newStore(t)
	a := mustDevice(t, s, "a", "10.0.0.1")
	cut := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	orig, _ := batchOf(t, s, model.JobFailed, cut.Add(-48*time.Hour), a)
	retry, _, err := s.CreateRetryBatch(ctx, "u", orig, []model.Job{job(a)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE ota_jobs SET state=?, updated_at=? WHERE batch_id=?", string(model.JobSucceeded), toMS(cut.Add(time.Hour)), retry); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneJobs(ctx, cut); err != nil || n != 0 {
		t.Fatalf("pruned %d (%v): the original is still referenced by the kept retry", n, err)
	}
	if n, err := s.PruneJobs(ctx, cut.Add(48*time.Hour)); err != nil || n != 2 {
		t.Fatalf("once both are old, both go: pruned %d (%v)", n, err)
	}
}
