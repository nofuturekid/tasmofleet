// Package store is the only package that talks SQL. It opens the SQLite
// database (pure-Go driver modernc.org/sqlite), applies the embedded
// migrations in migrations/ and implements the repository interfaces below.
//
// Consumers depend on the narrow interfaces (DeviceStore, JobStore, ...),
// never on *SQLite, so they can be tested with fakes. Tests of this package
// use a temporary on-disk database (t.TempDir()), never a shared file.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Errors returned by every repository.
var (
	ErrNotFound       = errors.New("store: not found")
	ErrConflict       = errors.New("store: conflict") // unique constraint, e.g. second active job for a device
	ErrNotImplemented = errors.New("store: not implemented")
)

// DeviceStore persists the inventory.
type DeviceStore interface {
	ListDevices(ctx context.Context) ([]model.Device, error)
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	GetDeviceByMAC(ctx context.Context, mac string) (model.Device, error)
	GetDeviceByTopic(ctx context.Context, topic string) (model.Device, error)
	CreateDevice(ctx context.Context, d model.Device) (model.DeviceID, error)
	UpdateDevice(ctx context.Context, d model.Device) error
	UpdateIdentity(ctx context.Context, id model.DeviceID, ident model.Identity) error
	DeleteDevice(ctx context.Context, id model.DeviceID) error
	// SetLastSeen persists the coarse health summary (written on state change only).
	SetLastSeen(ctx context.Context, id model.DeviceID, state model.HealthState, at time.Time) error
}

// HealthEvent is one persisted health state transition.
type HealthEvent struct {
	DeviceID model.DeviceID
	At       time.Time
	From     model.HealthState
	To       model.HealthState
	Source   model.HealthSource
	Detail   string
}

// HealthStore persists health transitions for the device history view.
type HealthStore interface {
	AppendHealthEvent(ctx context.Context, e HealthEvent) error
	ListHealthEvents(ctx context.Context, id model.DeviceID, limit int) ([]HealthEvent, error)
	PruneHealthEvents(ctx context.Context, before time.Time) (int64, error)
}

// JobStore persists OTA jobs. Every state change is written before the
// engine executes the actions of that transition (write-ahead), so a restart
// never repeats a non-idempotent step.
type JobStore interface {
	CreateBatch(ctx context.Context, createdBy string, jobs []model.Job) (model.BatchID, []model.JobID, error)
	GetJob(ctx context.Context, id model.JobID) (model.Job, error)
	ListJobs(ctx context.Context, f JobFilter) ([]model.Job, error)
	// ActiveJobs returns all jobs in a non-terminal state, oldest first.
	ActiveJobs(ctx context.Context) ([]model.Job, error)
	// SaveTransition atomically updates the job row and appends the event.
	// It fails with ErrConflict if the stored state is not ev.From
	// (optimistic concurrency).
	SaveTransition(ctx context.Context, j model.Job, ev model.JobEvent) error
	JobEvents(ctx context.Context, id model.JobID) ([]model.JobEvent, error)
}

// JobPruner is the retention of finished transactions (see SQLite.PruneJobs): they and their
// logs go once older than the cut-off; running work and a transaction with an unfinished job
// are never touched.
type JobPruner interface {
	PruneJobs(ctx context.Context, before time.Time) (int64, error)
}

// BatchStore is the batch (transaction) level of the job store: reading a
// batch and creating a retry of one. *SQLite implements it next to JobStore.
type BatchStore interface {
	GetBatch(ctx context.Context, id model.BatchID) (model.Batch, error)
	// CreateRetryBatch is CreateBatch for a batch that retries another one;
	// ErrNotFound if retryOf does not exist.
	CreateRetryBatch(ctx context.Context, createdBy string, retryOf model.BatchID, jobs []model.Job) (model.BatchID, []model.JobID, error)
	// BatchRetries lists the batches that retry id, oldest first.
	BatchRetries(ctx context.Context, id model.BatchID) ([]model.Batch, error)
}

// JobFilter narrows ListJobs. Zero values mean "any".
type JobFilter struct {
	DeviceID model.DeviceID
	BatchID  model.BatchID
	States   []model.JobState
	Limit    int
}

// ArtifactStore persists the firmware cache index (files live on disk).
type ArtifactStore interface {
	PutArtifact(ctx context.Context, a model.Artifact) (model.ArtifactID, error)
	GetArtifact(ctx context.Context, id model.ArtifactID) (model.Artifact, error)
	FindArtifact(ctx context.Context, release, filename string) (model.Artifact, error)
	ListArtifacts(ctx context.Context) ([]model.Artifact, error)
	DeleteArtifact(ctx context.Context, id model.ArtifactID) error
}

// CustomMeta is the provenance of an image an admin uploaded (artifact source
// "custom"); the image itself is an ordinary artifact.
type CustomMeta struct {
	ArtifactID   model.ArtifactID
	Label        string
	OriginalName string // display only, never used as a path
	UploadedBy   string
	UploadedAt   time.Time
}

// CustomFirmwareStore is implemented by *SQLite next to ArtifactStore.
// Deleting the artifact deletes its CustomMeta.
type CustomFirmwareStore interface {
	PutCustomMeta(ctx context.Context, m CustomMeta) error
	ListCustomMeta(ctx context.Context) ([]CustomMeta, error) // newest first
	// FindCustomArtifact finds a file of an upload by its key (Artifact.Custom),
	// release and file name; FindArtifact only ever returns upstream files.
	FindCustomArtifact(ctx context.Context, custom, release, filename string) (model.Artifact, error)
}

// BackupStore persists the backup index (files live on disk).
type BackupStore interface {
	PutBackup(ctx context.Context, b model.Backup) (model.BackupID, error)
	GetBackup(ctx context.Context, id model.BackupID) (model.Backup, error)
	ListBackups(ctx context.Context, device model.DeviceID) ([]model.Backup, error) // newest first
	DeleteBackup(ctx context.Context, id model.BackupID) error
	// SetBackupsKept pins (or unpins) rows in one transaction.
	SetBackupsKept(ctx context.Context, ids []model.BackupID, kept bool) error
	// ListOrphanBackups lists the kept backups of deleted devices, newest first.
	ListOrphanBackups(ctx context.Context) ([]model.Backup, error)
	// Failed backups (nothing stored), pruned with the audit retention.
	AddBackupAttempt(ctx context.Context, a model.BackupAttempt) (int64, error)
	ListBackupAttempts(ctx context.Context, device model.DeviceID, limit int) ([]model.BackupAttempt, error) // newest first
	PruneBackupAttempts(ctx context.Context, before time.Time) (int64, error)
}

// AuditStore is append-only. There is deliberately no update or delete
// except retention pruning.
type AuditStore interface {
	AppendAudit(ctx context.Context, e model.AuditEvent) error
	ListAudit(ctx context.Context, f AuditFilter) ([]model.AuditEvent, error)
	PruneAudit(ctx context.Context, before time.Time) (int64, error)
}

// AuditFacetStore lists what the audit filters can offer (design v2, N14).
type AuditFacetStore interface {
	// AuditFacets returns the distinct actors and actions of the rows at or
	// after since (zero = all rows), each sorted.
	AuditFacets(ctx context.Context, since time.Time) (actors, actions []string, err error)
}

// AuditFilter narrows ListAudit. Zero values mean "any".
type AuditFilter struct {
	Actor  string
	Actors []string // any of these (a display name can match several subjects)
	Target string
	// ActionPrefix keeps actions that start with it, e.g. "device." for the
	// family "device.*". It is compared literally (no wildcards).
	ActionPrefix string
	// Search keeps rows whose actor, action or target contains it (case
	// insensitive), whose target is one of SearchTargets (the device targets
	// whose name matched) or whose actor is one of SearchActors (the subjects
	// whose display name matched), both resolved by the caller.
	Search        string
	SearchTargets []string
	SearchActors  []string
	Since         time.Time
	Limit         int
}

// Session is a server-side web session. The cookie carries only a random
// ID; the store keeps its SHA-256, never the raw value.
type Session struct {
	IDHash    string
	Principal model.Principal
	CSRFToken string
	IDToken   string // OIDC id_token for RP-initiated logout (id_token_hint); empty otherwise
	// RefreshToken is the OIDC refresh token in plaintext in memory. The
	// store seals it with secretbox on write and opens it on read, so the
	// database never holds it in the clear.
	RefreshToken string
	// RecheckedAt is the time of the last OIDC group re-check (zero for
	// sessions of other methods).
	RecheckedAt time.Time
	// PasswordGen is the LocalUser.PasswordGen the login verified (local
	// sessions); the session is valid only while it is current.
	PasswordGen int64
	CreatedAt   time.Time
	LastSeen    time.Time
	ExpiresAt   time.Time
}

// SessionStore persists web sessions.
type SessionStore interface {
	CreateSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, idHash string) (Session, error)
	TouchSession(ctx context.Context, idHash string, at time.Time) error
	// UpdateSessionRecheck replaces the principal, ID token, refresh token
	// and RecheckedAt of the session s.IDHash after an OIDC re-check.
	UpdateSessionRecheck(ctx context.Context, s Session) error
	DeleteSession(ctx context.Context, idHash string) error
	// DeleteUserSessions removes every session whose principal has this
	// subject ("local:<name>" for local accounts) and returns the count.
	DeleteUserSessions(ctx context.Context, subject string) (int64, error)
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}

// LocalUser is a local account. Role is RoleOperator or RoleAdmin.
type LocalUser struct {
	Username           string
	PasswordHash       string // PHC-style string, see package auth
	UpdatedAt          time.Time
	Role               model.Role
	Disabled           bool
	MustChangePassword bool
	// PasswordGen starts at 1 and is incremented by every password change and
	// by disabling the account. Sessions and device cookies are bound to it.
	PasswordGen int64
	// Profile fields (design v2, N4); both may be empty.
	DisplayName string
	Email       string
	// OTPExpiresAt is set while the password is a one-time password an admin
	// generated: it is refused for sign-in from then on. Zero otherwise.
	OTPExpiresAt time.Time
	// CreatedAt is zero and CreatedBy empty for accounts created before they
	// were recorded. CreatedBy is the creator's subject, "cli" or "first-start".
	CreatedAt time.Time
	CreatedBy string
	// InitialSetup marks the admin the first start created while it still
	// has its first-start password (the setup link is valid only then). Only
	// AddLocalUser sets it; every password_gen increment clears it.
	InitialSetup bool
}

// ErrLastAdmin is returned when a change would leave no enabled admin.
var ErrLastAdmin = errors.New("store: this would leave no enabled admin")

// Actor is the signed-in admin on whose authority a mutation runs. The store
// applies the mutation only if, in the same SQL statement, that account still
// exists, is enabled, is an admin and is still at PasswordGen, so a role
// change, disable or password change of the actor that lands between the
// request check and the write stops the write (ErrStaleActor). The zero Actor
// means a trusted caller (the host CLI).
type Actor struct {
	Username    string
	PasswordGen int64
}

// ErrStaleActor is returned when the acting account no longer has the
// authority it had when the request was checked.
var ErrStaleActor = errors.New("store: the acting account changed during the request")

// LocalUserStore persists local accounts. Every mutation is one atomic SQL
// statement, so concurrent writers (web admin, CLI, second process) cannot
// create an account twice or remove the last enabled admin together.
type LocalUserStore interface {
	GetLocalUser(ctx context.Context, username string) (LocalUser, error)
	// ListLocalUsers returns all accounts ordered by username.
	ListLocalUsers(ctx context.Context) ([]LocalUser, error)
	// HasLocalUsers reports whether any local account exists (the
	// /login/local page is disabled when none does).
	HasLocalUsers(ctx context.Context) (bool, error)
	// AddLocalUser inserts u (PasswordGen 1); an existing name is ErrConflict.
	// Role must be operator or admin.
	AddLocalUser(ctx context.Context, u LocalUser, by Actor) error
	// SetLocalPassword replaces the hash, sets UpdatedAt and
	// MustChangePassword, clears OTPExpiresAt and increments PasswordGen.
	SetLocalPassword(ctx context.Context, username, hash string, at time.Time, mustChange bool, by Actor) error
	// SetLocalOneTimePassword is SetLocalPassword for a generated one-time
	// password: MustChangePassword is set and OTPExpiresAt is expires.
	SetLocalOneTimePassword(ctx context.Context, username, hash string, at, expires time.Time, by Actor) error
	// SetLocalProfile sets display name and e-mail on the authority of an admin.
	SetLocalProfile(ctx context.Context, username, displayName, email string, by Actor) error
	// SetOwnProfile is SetLocalProfile for the account's owner: it applies only
	// while the account is enabled and at expectGen, else ErrStaleActor.
	SetOwnProfile(ctx context.Context, username string, expectGen int64, displayName, email string) error
	// ChangeOwnPassword is SetLocalPassword for the account's owner: it applies
	// only while the account is enabled and still at expectGen (the generation
	// the owner's session was validated against), else ErrStaleActor. It
	// clears MustChangePassword and OTPExpiresAt.
	ChangeOwnPassword(ctx context.Context, username string, expectGen int64, hash string, at time.Time) error
	// SetLocalRole changes the role; demoting the last enabled admin is ErrLastAdmin.
	SetLocalRole(ctx context.Context, username string, role model.Role, by Actor) error
	// SetLocalDisabled disables (incrementing PasswordGen) or enables an
	// account; disabling the last enabled admin is ErrLastAdmin.
	SetLocalDisabled(ctx context.Context, username string, disabled bool, by Actor) error
	// DeleteLocalUser removes the account; the last enabled admin is ErrLastAdmin.
	DeleteLocalUser(ctx context.Context, username string, by Actor) error
}

// DeviceCookieStore records issued login device cookies so that a single
// cookie can be revoked (docs/security/login-rate-limiting.md). Only the
// SHA-256 of the cookie's random id is passed in and stored.
type DeviceCookieStore interface {
	// PutDeviceCookie records a cookie. More than MaxDeviceCookiesPerUser
	// per user evicts the oldest.
	PutDeviceCookie(ctx context.Context, idHash, username string, issued time.Time, passwordGen int64) error
	// HasDeviceCookie is true only while the user's PasswordGen still equals passwordGen.
	HasDeviceCookie(ctx context.Context, idHash, username string, passwordGen int64) (bool, error)
	DeleteDeviceCookie(ctx context.Context, idHash string) error
	DeleteUserDeviceCookies(ctx context.Context, username string) (int64, error)
	DeleteDeviceCookiesBefore(ctx context.Context, issued time.Time) (int64, error)
}

// Store bundles every repository; *SQLite implements it.
type Store interface {
	DeviceStore
	HealthStore
	JobStore
	JobPruner
	ArtifactStore
	CustomFirmwareStore
	BackupStore
	AuditStore
	SessionStore
	LocalUserStore
	DeviceCookieStore
	DiscoveryStore
	RestoreStore
	HardwareStore
	SystemSettingsStore
	PrefsStore
	Ping(ctx context.Context) error
	Close() error
}
