package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const jobCols = `id, batch_id, device_id, state, stage, plan, policy, skip_backup, attempt, target_release, target_variant, target_custom,
	target_artifact, minimal_artifact, identity_before, original_ota_url, uptime_before, seen_offline, cancel_requested,
	auto_continue_seen, skip_power_restore, power_on, power_read_at, power_note, restore_relays, power_cutoff, deadline_at, last_error, created_by, created_at, updated_at, kind, commands, created_kind, delivery, batch_parallel, reply_timeout_ms`

func scanJob(r scanner) (model.Job, error) {
	var j model.Job
	var state, stage, plan, policy, ident, kind, delivery string
	var skip, seen, cancel, autoCont, skipPower, restoreRel int
	var power uint32
	var deadline, created, updated, powerAt, cutoff, replyMS int64
	err := r.Scan(&j.ID, &j.BatchID, &j.DeviceID, &state, &stage, &plan, &policy, &skip, &j.Attempt, &j.TargetRelease,
		&j.TargetVariant, &j.TargetCustom, &j.TargetArtifact, &j.MinimalArtifact, &ident, &j.OriginalOtaURL, &j.UptimeBefore, &seen, &cancel,
		&autoCont, &skipPower, &power, &powerAt, &j.PowerNote, &restoreRel, &cutoff, &deadline, &j.LastError, &j.CreatedBy, &created, &updated, &kind, &j.Commands, &j.CreatedKind, &delivery, &j.BatchParallel, &replyMS)
	if err != nil {
		return model.Job{}, err
	}
	if err := json.Unmarshal([]byte(ident), &j.IdentityBefore); err != nil {
		return model.Job{}, fmt.Errorf("store: job %d identity: %w", j.ID, err)
	}
	j.PowerOn, j.PowerReadAt = model.RelaySet(power), fromMS(powerAt)
	j.RestoreRelays, j.PowerCutoff = restoreRel != 0, fromMS(cutoff)
	j.AutoContinueSeen, j.SkipPowerRestore = autoCont != 0, skipPower != 0
	j.Delivery = model.Delivery(delivery)
	j.ReplyTimeout = time.Duration(replyMS) * time.Millisecond
	if kind != storedUpdate {
		j.Kind = model.JobKind(kind)
	}
	j.State, j.Stage, j.Plan, j.Policy = model.JobState(state), model.Stage(stage), model.Plan(plan), model.UpdatePolicy(policy)
	j.SkipBackup, j.SeenOffline, j.CancelRequested = skip != 0, seen != 0, cancel != 0
	j.DeadlineAt, j.CreatedAt, j.UpdatedAt = fromMS(deadline), fromMS(created), fromMS(updated)
	return j, nil
}

// CreateBatch inserts the batch and all its jobs in one transaction. If any
// device already has an active job, nothing is created and ErrConflict is
// returned; an unknown device yields ErrNotFound. Jobs get the state the
// caller set (normally JobQueued).
func (s *SQLite) CreateBatch(ctx context.Context, createdBy string, jobs []model.Job) (model.BatchID, []model.JobID, error) {
	return s.createBatch(ctx, createdBy, 0, jobs)
}

// CreateRetryBatch creates a batch that retries retryOf.
func (s *SQLite) CreateRetryBatch(ctx context.Context, createdBy string, retryOf model.BatchID, jobs []model.Job) (model.BatchID, []model.JobID, error) {
	if retryOf == 0 {
		return 0, nil, errors.New("store: a retry needs the batch it retries")
	}
	if _, err := s.GetBatch(ctx, retryOf); err != nil {
		return 0, nil, err
	}
	return s.createBatch(ctx, createdBy, retryOf, jobs)
}

func (s *SQLite) createBatch(ctx context.Context, createdBy string, retryOf model.BatchID, jobs []model.Job) (model.BatchID, []model.JobID, error) {
	if len(jobs) == 0 {
		return 0, nil, errors.New("store: empty batch")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	now := toMS(s.now())
	res, err := tx.ExecContext(ctx, "INSERT INTO ota_batches (created_by, created_at, retry_of) VALUES (?,?,?)", createdBy, now, retryOf)
	if err != nil {
		return 0, nil, mapErr(err)
	}
	bid, err := res.LastInsertId()
	if err != nil {
		return 0, nil, err
	}
	ids := make([]model.JobID, 0, len(jobs))
	for _, j := range jobs {
		ident, err := json.Marshal(j.IdentityBefore)
		if err != nil {
			return 0, nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, skip_backup, attempt,
			target_release, target_variant, target_custom, target_artifact, minimal_artifact, identity_before, original_ota_url, uptime_before,
			seen_offline, cancel_requested, auto_continue_seen, skip_power_restore, power_on, power_read_at, power_note, restore_relays, power_cutoff, deadline_at, last_error, created_by, created_at, updated_at, kind, commands, created_kind, delivery, batch_parallel, reply_timeout_ms)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			bid, j.DeviceID, string(j.State), string(j.Stage), string(j.Plan), string(j.Policy), b2i(j.SkipBackup), j.Attempt,
			j.TargetRelease, j.TargetVariant, j.TargetCustom, j.TargetArtifact, j.MinimalArtifact, string(ident), j.OriginalOtaURL, j.UptimeBefore,
			b2i(j.SeenOffline), b2i(j.CancelRequested), b2i(j.AutoContinueSeen), b2i(j.SkipPowerRestore), uint32(j.PowerOn), toMS(j.PowerReadAt), j.PowerNote, b2i(j.RestoreRelays), toMS(j.PowerCutoff),
			toMS(j.DeadlineAt), j.LastError, createdBy, now, now, kindOf(j), j.Commands, j.CreatedKind, string(j.Delivery), j.BatchParallel, j.ReplyTimeout.Milliseconds())
		if err != nil {
			return 0, nil, fmt.Errorf("device %d: %w", j.DeviceID, mapErr(err))
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, nil, err
		}
		ids = append(ids, model.JobID(id))
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	return model.BatchID(bid), ids, nil
}

func (s *SQLite) GetJob(ctx context.Context, id model.JobID) (model.Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM ota_jobs WHERE id=?", id))
	return j, notFound(err)
}

// ListJobs returns matching jobs newest first (highest id first); Limit 0
// means no limit.
func (s *SQLite) ListJobs(ctx context.Context, f JobFilter) ([]model.Job, error) {
	var where []string
	var args []any
	if f.DeviceID != 0 {
		where = append(where, "device_id=?")
		args = append(args, f.DeviceID)
	}
	if f.BatchID != 0 {
		where = append(where, "batch_id=?")
		args = append(args, f.BatchID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",")+")")
		for _, st := range f.States {
			args = append(args, string(st))
		}
	}
	return s.queryJobs(ctx, where, "id DESC", f.Limit, args)
}

// ActiveJobs returns all non-terminal jobs, oldest first.
func (s *SQLite) ActiveJobs(ctx context.Context) ([]model.Job, error) {
	return s.queryJobs(ctx, []string{"state NOT IN ('succeeded','failed','cancelled')"}, "id ASC", 0, nil)
}

func (s *SQLite) queryJobs(ctx context.Context, where []string, order string, limit int, args []any) ([]model.Job, error) {
	q := "SELECT " + jobCols + " FROM ota_jobs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY " + order //nolint:gosec // order is a constant chosen by the callers above; values are bound
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SaveTransition writes j and appends ev in one transaction, but only if the
// stored state equals ev.From; otherwise it returns ErrConflict and changes
// nothing. Immutable fields (batch, device, creator, creation time) are not
// updated.
func (s *SQLite) SaveTransition(ctx context.Context, j model.Job, ev model.JobEvent) error {
	if ev.JobID != j.ID {
		return fmt.Errorf("store: event is for job %d, job is %d", ev.JobID, j.ID)
	}
	ident, err := json.Marshal(j.IdentityBefore)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	now := s.now()
	res, err := tx.ExecContext(ctx, `UPDATE ota_jobs SET state=?, stage=?, plan=?, policy=?, skip_backup=?, attempt=?, target_release=?,
		target_variant=?, target_artifact=?, minimal_artifact=?, identity_before=?, original_ota_url=?, uptime_before=?, seen_offline=?,
		cancel_requested=?, auto_continue_seen=?, skip_power_restore=?, power_on=?, power_read_at=?, power_note=?, restore_relays=?, power_cutoff=?, deadline_at=?, last_error=?, updated_at=? WHERE id=? AND state=?`,
		string(j.State), string(j.Stage), string(j.Plan), string(j.Policy), b2i(j.SkipBackup), j.Attempt, j.TargetRelease,
		j.TargetVariant, j.TargetArtifact, j.MinimalArtifact, string(ident), j.OriginalOtaURL, j.UptimeBefore, b2i(j.SeenOffline),
		b2i(j.CancelRequested), b2i(j.AutoContinueSeen), b2i(j.SkipPowerRestore), uint32(j.PowerOn), toMS(j.PowerReadAt), j.PowerNote, b2i(j.RestoreRelays), toMS(j.PowerCutoff), toMS(j.DeadlineAt), j.LastError, toMS(now), j.ID, string(ev.From))
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var cur string
		err := tx.QueryRowContext(ctx, "SELECT state FROM ota_jobs WHERE id=?", j.ID).Scan(&cur)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: job %d is in state %q, transition expects %q", ErrConflict, j.ID, cur, ev.From)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ota_job_events (job_id, at, from_state, to_state, event, detail) VALUES (?,?,?,?,?,?)`,
		j.ID, toMS(s.orNow(ev.At)), string(ev.From), string(ev.To), ev.Event, ev.Detail); err != nil {
		return mapErr(err)
	}
	return tx.Commit()
}

// JobEvents returns the job's events oldest first.
func (s *SQLite) JobEvents(ctx context.Context, id model.JobID) ([]model.JobEvent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT job_id, at, from_state, to_state, event, detail FROM ota_job_events WHERE job_id=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.JobEvent
	for rows.Next() {
		var e model.JobEvent
		var at int64
		var from, to string
		if err := rows.Scan(&e.JobID, &at, &from, &to, &e.Event, &e.Detail); err != nil {
			return nil, err
		}
		e.At, e.From, e.To = fromMS(at), model.JobState(from), model.JobState(to)
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetBatch returns one batch.
func (s *SQLite) GetBatch(ctx context.Context, id model.BatchID) (model.Batch, error) {
	var b model.Batch
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT id, created_by, created_at, retry_of FROM ota_batches WHERE id=?", id).
		Scan(&b.ID, &b.CreatedBy, &created, &b.RetryOf)
	b.CreatedAt = fromMS(created)
	return b, notFound(err)
}

// BatchRetries lists the batches that retry id, oldest first.
func (s *SQLite) BatchRetries(ctx context.Context, id model.BatchID) ([]model.Batch, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, created_by, created_at, retry_of FROM ota_batches WHERE retry_of=? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.Batch
	for rows.Next() {
		var b model.Batch
		var created int64
		if err := rows.Scan(&b.ID, &b.CreatedBy, &created, &b.RetryOf); err != nil {
			return nil, err
		}
		b.CreatedAt = fromMS(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

var _ BatchStore = (*SQLite)(nil)

const storedUpdate = "update" // the kind column's default

// kindOf stores the zero Kind as an update.
func kindOf(j model.Job) string {
	if j.Kind == "" {
		return storedUpdate
	}
	return string(j.Kind)
}

// PruneJobs deletes finished transactions (batches) whose newest job was last updated before
// the cut-off, with their jobs and job logs, and returns the number of jobs deleted. It never
// touches running work and never cuts a transaction in half: one job that is not finished keeps
// the whole batch. A batch that a kept batch retries (ota_batches.retry_of) is kept too, so
// "retry of #N" never dangles.
func (s *SQLite) PruneJobs(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.retry_of,
		NOT EXISTS (SELECT 1 FROM ota_jobs j WHERE j.batch_id=b.id AND j.state NOT IN ('succeeded','failed','cancelled'))
		AND COALESCE((SELECT MAX(j.updated_at) FROM ota_jobs j WHERE j.batch_id=b.id), ?) < ?
		FROM ota_batches b`, toMS(before), toMS(before))
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	retryOf := map[int64]int64{}
	prune := map[int64]bool{}
	for rows.Next() {
		var id, of int64
		var old bool
		if err := rows.Scan(&id, &of, &old); err != nil {
			return 0, err
		}
		retryOf[id], prune[id] = of, old
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for changed := true; changed; { // a kept retry keeps what it retries, along the whole chain
		changed = false
		for id, of := range retryOf {
			if !prune[id] && of != 0 && prune[of] {
				prune[of], changed = false, true
			}
		}
	}
	var jobs int64
	for id, p := range prune {
		if !p {
			continue
		}
		n, err := rowsDeleted(tx.ExecContext(ctx, "DELETE FROM ota_jobs WHERE batch_id=?", id)) // the job log goes with its job (cascade)
		if err != nil {
			return 0, err
		}
		jobs += n
		if _, err := tx.ExecContext(ctx, "DELETE FROM ota_batches WHERE id=?", id); err != nil {
			return 0, err
		}
	}
	return jobs, tx.Commit()
}
