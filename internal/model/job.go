package model

import (
	"strconv"
	"strings"
	"time"
)

// Batch is one transaction: the jobs created by one user action. A retry of
// failed devices is a new Batch with RetryOf set to the one it retries.
type Batch struct {
	ID        BatchID
	CreatedBy string
	CreatedAt time.Time
	RetryOf   BatchID // 0 = not a retry
}

// JobID is the OTA job primary key.
type JobID int64

// BatchID groups the jobs created by one user action ("update these 12 devices").
type BatchID int64

// JobState is a state of the OTA state machine (package ota). The full
// transition table is in docs/spec, section "OTA state machine".
type JobState string

// OTA job states. Terminal states: Succeeded, Failed, Cancelled.
const (
	JobQueued        JobState = "queued"         // waiting for a free slot
	JobPreflight     JobState = "preflight"      // identity detection, guard, plan
	JobBackup        JobState = "backup"         // pre-OTA config backup (skippable per batch)
	JobSetOtaURL     JobState = "set_ota_url"    // OtaUrl <url> issued
	JobUpgradeIssued JobState = "upgrade_issued" // Upgrade 1 issued, waiting for the device to drop off
	JobAwaitReboot   JobState = "await_reboot"   // device gone, waiting for it to come back
	// JobSafebootSwitch: a push job on an ESP32 with the safeboot layout has sent the one restart
	// into safeboot and waits for the device to answer as safeboot (#30). Never re-sent, also not after a restart.
	JobSafebootSwitch JobState = "safeboot_switch"
	// JobSafebootRecover: the job ended with the device in (or possibly in) safeboot; the one
	// switch back to the app has been sent and the job waits for the old firmware to answer.
	JobSafebootRecover JobState = "safeboot_recover"
	JobVerify          JobState = "verify"          // device back, checking version/variant/family
	JobRestoreOtaURL   JobState = "restore_ota_url" // putting the original OtaUrl back
	JobRestorePower    JobState = "restore_power"   // switching the relays that were ON before the update back on
	JobApplying        JobState = "applying"        // policy job: commands are being sent and saved (JobVerify then reads them back)
	JobSucceeded       JobState = "succeeded"
	JobFailed          JobState = "failed"
	JobCancelled       JobState = "cancelled"
)

// Terminal reports whether no further transition can leave the state.
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// Stage says which image the current step is flashing. ESP8266 devices
// without enough free flash go through StageMinimal first.
type Stage string

// Stages.
const (
	StageFinal   Stage = "final"
	StageMinimal Stage = "minimal"
)

// Plan is the per-family update policy decided in preflight.
type Plan string

// Plans.
const (
	PlanDirect Plan = "direct" // ESP32, or ESP8266 with enough free flash
	PlanStaged Plan = "staged" // ESP8266: minimal first, then the target image
)

// UpdatePolicy is chosen per batch and decides what happens when the
// device already runs the target or a newer version.
type UpdatePolicy string

// Update policies.
const (
	PolicyOnlyNewer      UpdatePolicy = "only-newer"      // default: skip if current >= target
	PolicyReinstall      UpdatePolicy = "reinstall"       // also flash when current == target
	PolicyAllowDowngrade UpdatePolicy = "allow-downgrade" // also flash when current > target
)

// JobKind says what a job does. The zero value is a firmware update (the OTA
// state machine); the store keeps it as "update".
type JobKind string

// KindPolicy is "Make compliant": apply the stored commands, save once, read back.
const KindPolicy JobKind = "policy"

// KindCommand is "Send command": one free-form command, sent once, with the
// device's reply as the result. Commands holds the single canonical line.
const KindCommand JobKind = "command"

// KindBackup is "Back up": take a backup of the device (dump, snapshot, decoded
// JSON). Commands holds only the marker BackupMarker; the work is the backup
// service's, not a device command.
const KindBackup JobKind = "backup"

// KindTemplate is "Apply GPIO template" (#327): back up, re-read Module and Template, send the one
// Template command, wait for the restart and read it back. Commands holds that command line.
const KindTemplate JobKind = "template"

// BackupMarker is stored in Commands of a backup job (a job without work is not
// created, and a retry keeps the field as it is).
const BackupMarker = "backup"

// Target is what the job puts on the device, for lists: the release and
// variant of an update, "Make compliant" for a policy job, "Send command"
// for a command job (the web layer adds the redacted command).
func (j Job) Target() string {
	if j.IsPolicy() {
		return "Make compliant"
	}
	if j.IsCommand() {
		return "Send command"
	}
	if j.IsBackup() {
		return "Back up"
	}
	if j.IsTemplate() {
		return "GPIO template"
	}
	t := strings.TrimSpace(j.TargetRelease + " " + j.TargetVariant)
	if j.TargetBuild != "" {
		t += " (" + j.TargetBuild + ")"
	}
	return t
}

// IsPolicy reports whether the job makes a device policy-compliant.
func (j Job) IsPolicy() bool { return j.Kind == KindPolicy }

// IsCommand reports whether the job sends one free-form command.
func (j Job) IsCommand() bool { return j.Kind == KindCommand }

// IsBackup reports whether the job takes a backup of the device.
func (j Job) IsBackup() bool { return j.Kind == KindBackup }

// IsTemplate reports whether the job applies a GPIO template.
func (j Job) IsTemplate() bool { return j.Kind == KindTemplate }

// IsUpdate reports whether the job is a firmware update (the OTA state machine).
func (j Job) IsUpdate() bool {
	return !j.IsPolicy() && !j.IsCommand() && !j.IsBackup() && !j.IsTemplate()
}

// Delivery is how a job gets the firmware onto the device.
type Delivery string

// Deliveries. The zero value is the default.
const (
	DeliveryPull Delivery = ""     // the device downloads the image (OtaUrl, Upgrade 1)
	DeliveryPush Delivery = "push" // tasmofleet uploads the image to the device's /u2
)

// Transport is the way a job talks to its device.
type Transport string

// Transports. Empty reads as HTTP.
const (
	TransportHTTP Transport = ""
	TransportMQTT Transport = "mqtt"
)

// Job is one OTA job for one device. Every field needed to resume after a
// process restart is persisted; nothing lives only in memory.
type Job struct {
	ID       JobID
	BatchID  BatchID
	DeviceID DeviceID

	// Kind is empty (an update) unless the job makes a device policy-compliant; such a
	// job ignores every field below that is about firmware and carries its
	// work in Commands.
	Kind JobKind
	// Commands is a policy job's work: one device command per line, in the
	// order they are sent. Fixed at creation, so a retry sends the same.
	Commands string
	// CreatedKind is the sign-in method of CreatedBy ("local", "oidc", "header"):
	// together with the subject it names the creator of a command job.
	CreatedKind string

	State   JobState
	Stage   Stage
	Plan    Plan
	Attempt int // Upgrade attempts in the current stage (max 2)

	Policy     UpdatePolicy
	SkipBackup bool // set only by explicit user choice
	// SkipPowerRestore turns the power restore after the update off. The
	// default (false) is to switch back on the relays that were ON before.
	SkipPowerRestore bool
	// RestoreRelays also restores plain relays. Off by default: on this fleet
	// relays keep their state across an update, so a relay that is OFF after
	// the boot was most likely switched off by someone, and switching it back
	// ON would override them. Only light channels are restored by default.
	RestoreRelays bool

	// Delivery is how the image reaches the device: empty is the default pull
	// (the device downloads it from tasmofleet's firmware server after OtaUrl and
	// Upgrade 1), DeliveryPush is tasmofleet uploading it to the device's /u2.
	// Fixed at creation.
	Delivery Delivery
	// ViaSafeboot: a push job on an ESP32 whose image does not fit its running partition
	// (single-OTA layout): the device is restarted into safeboot and the image is uploaded
	// there (#30). Only with DeliveryPush; fixed at creation.
	ViaSafeboot bool
	// RecoveryStep is how far the one recovery of a push into safeboot got: 0 none, 1 entered
	// (probing, nothing sent), 2 the switch back (/u4?u4=ota) was sent. Persisted before the
	// request goes out, so a second recovery or a second switch back is impossible, also after a
	// restart of tasmofleet.
	RecoveryStep int

	// Transport is how the job reaches its device, recorded at preflight from the path choice:
	// empty (every job made before MQTT transport) is HTTP.
	Transport Transport
	// MQTTUpload: the transaction allowed the experimental firmware upload over MQTT. Decided in
	// the update preview, fixed at creation, kept so a resume goes the same way.
	MQTTUpload bool

	// BatchParallel caps how many jobs of this job's transaction run at the
	// same time (bulk "One device at a time", "Devices at a time"); 0 leaves
	// it to the fleet-wide limit. ReplyTimeout is how long a command job waits
	// for the device's answer (bulk "Wait up to 5 s"); 0 is the default. Both
	// are fixed at creation and copied by a retry.
	BatchParallel int
	ReplyTimeout  time.Duration

	TargetRelease   string
	TargetCustom    string // key of the uploaded image (Artifact.Custom) this job installs; empty = the upstream release
	TargetVariant   string // chosen variant; differs from the detected one only on explicit user choice
	TargetBuild     string // explicit chip build (ESP32-S2: "s2" or "s2cdc"), chosen per update in the preview; empty = none (#88)
	TargetArtifact  ArtifactID
	MinimalArtifact ArtifactID // ESP8266: the release's tasmota-minimal; flashed only if Plan == PlanStaged, recorded for direct plans so the cache keeps it

	IdentityBefore Identity // detected in preflight
	OriginalOtaURL string   // restored at the end
	// PowerOn is the set of relays that were ON in preflight (empty when
	// the state could not be read, e.g. the device already ran minimal).
	// Only these are ever switched, and only after the final image is verified.
	PowerOn RelaySet
	// PowerReadAt is when PowerOn was last read from the device (preflight,
	// refreshed by the read right before the first Upgrade). The restore
	// refuses to act on a state that is older than the configured maximum.
	PowerReadAt time.Time
	// PowerCutoff is when the device first went offline for the update. An
	// MQTT "switched off" report counts only if it was received between
	// PowerReadAt and PowerCutoff; later ones are the device's boot
	// announcements.
	PowerCutoff time.Time
	// PowerNote collects why relays are not restored (switched off by
	// someone, PulseTime/Interlock/shutter relays, state too old); it is
	// added to LastError when the job ends.
	PowerNote    string
	UptimeBefore int64 // UptimeSec right before Upgrade; reboot proof is a smaller value later
	SeenOffline  bool  // device dropped off after Upgrade
	// CancelRequested: the user cancelled while the device was flashing; the
	// job ends as cancelled after the current stage instead of continuing.
	CancelRequested bool
	// AutoContinueSeen: verify already saw the device back on minimal/safeboot
	// in the final stage once (Tasmota's own ota_loader continuation); the
	// second time it counts as "still old".
	AutoContinueSeen bool

	DeadlineAt time.Time // when the current state times out
	LastError  string
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// MaxRelays is the number of relays a RelaySet can hold.
const MaxRelays = 32

// RelaySet is a set of relay numbers (1 = POWER/POWER1, 2 = POWER2, ...).
// It is a plain integer so that Job stays comparable.
type RelaySet uint32

// Add returns the set with relay n (1..MaxRelays) added; other n are ignored.
func (s RelaySet) Add(n int) RelaySet {
	if n < 1 || n > MaxRelays {
		return s
	}
	return s | 1<<(n-1)
}

// Has reports whether relay n is in the set.
func (s RelaySet) Has(n int) bool {
	return n >= 1 && n <= MaxRelays && s&(1<<(n-1)) != 0
}

// String lists the relays like "1, 3".
func (s RelaySet) String() string {
	var parts []string
	for _, n := range s.List() {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

// List returns the relays in ascending order.
func (s RelaySet) List() []int {
	var out []int
	for n := 1; n <= MaxRelays; n++ {
		if s.Has(n) {
			out = append(out, n)
		}
	}
	return out
}

// JobEvent is one persisted transition, for the job history view and audit.
type JobEvent struct {
	JobID  JobID
	At     time.Time
	From   JobState
	To     JobState
	Event  string
	Detail string
}
