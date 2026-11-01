package store

import (
	"context"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// RestoreID is the restore job primary key.
type RestoreID int64

// Restore job states. Done and Failed are terminal.
const (
	RestorePending   = "pending"
	RestoreBackup    = "backup"
	RestoreFiles     = "files"
	RestoreSending   = "sending"
	RestoreWaiting   = "waiting"
	RestoreVerifying = "verifying"
	RestoreDone      = "done"
	RestoreFailed    = "failed"
)

// Restore job kinds.
const (
	KindRestore = "restore" // the backup goes back onto the device it came from
	KindReplace = "replace" // the backup goes onto new hardware that then takes over the entry
)

// RestoreJob is one restore of a backup onto a device, or (Kind replace) onto
// new hardware that is about to take over the inventory entry.
type RestoreJob struct {
	ID          RestoreID
	DeviceID    model.DeviceID
	BackupID    model.BackupID
	PreBackupID model.BackupID
	State       string
	Detail      string
	Verified    bool
	Changes     int
	Actor       string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// Kind is KindRestore (also when empty) or KindReplace. For a replace,
	// NewMAC, NewAddress and NewPort name the replacement hardware.
	Kind       string
	NewMAC     string
	NewAddress string
	NewPort    int
	// Transport is how the restore reached the device, recorded when it starts: empty (every job
	// made before MQTT transport) is HTTP, "mqtt" otherwise.
	Transport model.Transport
}

// Terminal reports whether the job is finished.
func (j RestoreJob) Terminal() bool { return j.State == RestoreDone || j.State == RestoreFailed }

// RestoreStore persists restore jobs.
type RestoreStore interface {
	// CreateRestore inserts a pending job. A second unfinished job for the
	// device is ErrConflict.
	CreateRestore(ctx context.Context, j RestoreJob) (RestoreID, error)
	GetRestore(ctx context.Context, id RestoreID) (RestoreJob, error)
	// UpdateRestore replaces state, detail, verification result and the
	// pre-restore backup of the job.
	UpdateRestore(ctx context.Context, j RestoreJob) error
	// ListRestores returns the newest jobs of a device first; limit <= 0 means all.
	ListRestores(ctx context.Context, device model.DeviceID, limit int) ([]RestoreJob, error)
	// FailActiveRestores marks every unfinished job failed with reason (start-up
	// recovery: the process that ran them is gone) and returns the count.
	FailActiveRestores(ctx context.Context, reason string) (int64, error)
}

const restoreCols = "id, device_id, backup_id, pre_backup_id, state, detail, verified, changes, actor, created_at, updated_at, kind, new_mac, new_address, new_port, transport"

func scanRestore(r scanner) (RestoreJob, error) {
	var j RestoreJob
	var verified int
	var created, updated int64
	var transport string
	err := r.Scan(&j.ID, &j.DeviceID, &j.BackupID, &j.PreBackupID, &j.State, &j.Detail, &verified, &j.Changes, &j.Actor, &created, &updated, &j.Kind, &j.NewMAC, &j.NewAddress, &j.NewPort, &transport)
	j.Transport = model.Transport(transport)
	j.Verified, j.CreatedAt, j.UpdatedAt = verified != 0, fromMS(created), fromMS(updated)
	return j, err
}

func (s *SQLite) CreateRestore(ctx context.Context, j RestoreJob) (RestoreID, error) {
	now := toMS(s.now())
	if j.Kind == "" {
		j.Kind = KindRestore
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO restore_jobs (device_id, backup_id, pre_backup_id, state, detail, verified, changes, actor,
		created_at, updated_at, kind, new_mac, new_address, new_port, transport) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		j.DeviceID, j.BackupID, j.PreBackupID, RestorePending, j.Detail, b2i(j.Verified), j.Changes, j.Actor, now, now,
		j.Kind, strings.ToLower(j.NewMAC), j.NewAddress, j.NewPort, string(j.Transport))
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	return RestoreID(id), err
}

func (s *SQLite) GetRestore(ctx context.Context, id RestoreID) (RestoreJob, error) {
	j, err := scanRestore(s.db.QueryRowContext(ctx, "SELECT "+restoreCols+" FROM restore_jobs WHERE id=?", id))
	return j, notFound(err)
}

func (s *SQLite) UpdateRestore(ctx context.Context, j RestoreJob) error {
	res, err := s.db.ExecContext(ctx, `UPDATE restore_jobs SET pre_backup_id=?, state=?, detail=?, verified=?, changes=?, transport=?, updated_at=? WHERE id=?`,
		j.PreBackupID, j.State, j.Detail, b2i(j.Verified), j.Changes, string(j.Transport), toMS(s.now()), j.ID)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // SQLite always reports it
		return ErrNotFound
	}
	return nil
}

func (s *SQLite) ListRestores(ctx context.Context, device model.DeviceID, limit int) ([]RestoreJob, error) {
	q, args := "SELECT "+restoreCols+" FROM restore_jobs WHERE device_id=? ORDER BY created_at DESC, id DESC", []any{device}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []RestoreJob
	for rows.Next() {
		j, err := scanRestore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *SQLite) FailActiveRestores(ctx context.Context, reason string) (int64, error) {
	return rowsDeleted(s.db.ExecContext(ctx, `UPDATE restore_jobs SET state=?, detail=?, updated_at=? WHERE state NOT IN ('done','failed')`,
		RestoreFailed, reason, toMS(s.now())))
}
