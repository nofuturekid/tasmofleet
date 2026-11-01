package model

import "time"

// BackupID is the backup primary key.
type BackupID int64

// BackupKind distinguishes the binary Tasmota config dump from the JSON
// status snapshot.
type BackupKind string

// Backup kinds.
const (
	BackupConfigDump BackupKind = "dmp"      // /dl download, restorable through the Tasmota web UI
	BackupSnapshot   BackupKind = "snapshot" // JSON of Status 1..11, read-only reference
	BackupDecoded    BackupKind = "decoded"  // decode-config JSON of a dmp (secrets inside, see spec)
	BackupFiles      BackupKind = "files"    // tar of the device's flash files plus a manifest (secret: holds tlskey)
)

// BackupTrigger says what started a backup run (the list shows it as
// "Manual", "Scheduled", "Automatic, before update to 14.3.0", ...). Empty is
// "not recorded" (backups taken before v0.11).
type BackupTrigger string

// Backup triggers.
const (
	TriggerManual    BackupTrigger = "manual"    // the Back up button
	TriggerScheduled BackupTrigger = "scheduled" // BACKUP_INTERVAL
	TriggerUpdate    BackupTrigger = "update"    // before an update; TriggerRef is the target release
	TriggerRestore   BackupTrigger = "restore"   // before (and after) a restore
	TriggerReplace   BackupTrigger = "replace"   // "Replace device"
	TriggerBulk      BackupTrigger = "bulk"      // "Back up" on several devices
)

// Backup is one stored backup file under <data>/backups.
type Backup struct {
	ID BackupID
	// DeviceID is 0 for a backup whose device was deleted while its backups
	// were kept; DeviceName and DeviceMAC then still say whose it was.
	DeviceID   DeviceID
	DeviceName string
	DeviceMAC  string
	Kind       BackupKind
	TakenAt    time.Time
	Version    string // firmware version at the time
	Path       string // relative to the data dir
	Size       int64
	SHA256     string
	// DecoderVersion is set for BackupDecoded; Note carries flags such as
	// "decoder outdated" (dmp kept, decoding skipped).
	DecoderVersion string
	Note           string
	// Kept backups are never pruned and do not count against the retention.
	// It is set on every file of a run alike.
	Kept       bool
	Trigger    BackupTrigger
	TriggerRef string // TriggerUpdate: the target release
}

// BackupAttempt is a backup that failed: nothing was stored. Reason is a
// human text without credentials.
type BackupAttempt struct {
	ID         int64
	DeviceID   DeviceID
	At         time.Time
	Trigger    BackupTrigger
	TriggerRef string
	Version    string // firmware version known at the time
	Reason     string
}

// AuditEvent is one append-only audit log row. Every state-changing action
// (web, CLI, engine) writes one.
type AuditEvent struct {
	ID         int64
	At         time.Time
	Actor      string // principal subject, "cli", or "system"
	Action     string // dotted verb, e.g. "device.create", "ota.start", "command.send"
	Target     string // e.g. "device:12", "transaction:3"
	Detail     string // JSON object, no secrets
	Result     string // "ok" or an error string
	RemoteAddr string
}

// Role is an authorisation level. Higher roles include lower ones.
type Role int

// Roles.
const (
	RoleNone     Role = iota
	RoleOperator      // read everything, OTA, backups, allowlisted commands, power toggles
	RoleAdmin         // plus inventory edits, settings, imports, user management
)

// Principal is the authenticated user of a request.
type Principal struct {
	Subject string // OIDC sub, proxy header user, or "local:<name>"
	Name    string // display name
	Email   string
	Groups  []string
	Role    Role
	Method  string // "oidc", "header", "local"
}
