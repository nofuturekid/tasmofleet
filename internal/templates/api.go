// Package templates is the service behind the "GPIO templates" tab of the Policy page (#327): it
// reads the Module and Template of the fleet, groups the devices, plans what applying a reference
// template would do, and starts the apply transaction. The pure decisions live in internal/policy
// (template.go); this package owns the reads, the preview digest and the hand-off to the OTA engine.
//
// The types in this file are the contract between the service and the pages (WP-C).
package templates

import (
	"context"
	"errors"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
)

// Source says where a device's Module and Template readings come from.
type Source string

// Sources. Only live readings are ever applied (E5); a backup reading is shown with its date.
const (
	SourceLive   Source = "live"
	SourceBackup Source = "backup"
	SourceNone   Source = "" // nothing read yet and no backup
)

// DeviceView is one device of the tab.
type DeviceView struct {
	ID       model.DeviceID
	Name     string
	Family   model.ChipFamily
	Hardware string // model.Identity.Hardware, e.g. "ESP8285N08"; shown, never a group key
	// Reading is what the planner knows: Module, Template and whether they are live.
	Reading policy.TemplateDevice
	Source  Source
	// ReadAt is when a live reading was taken, or the date of the backup it comes from.
	ReadAt time.Time
	// NotJudgedReason says in a sentence why the device is in the "Not judged" fold (no reading and no
	// backup, unknown chip family, template not read, offline); "" for a judged device.
	NotJudgedReason string
	// OfflineSince is set when the device is offline (the "Not judged" fold says since when); zero otherwise.
	OfflineSince time.Time
	// PowerOnState is the device's PowerOnState setting as last known (the raw value, "" when unknown);
	// PowerOnStateText is the sentence for the restart line ("after the restart relays go off").
	PowerOnState     string
	PowerOnStateText string
	// Plan is the verdict for applying the group's reference to this device; nil when the group has no
	// reference, the group is a built-in module group, or the device is not judged.
	Plan *policy.ApplyPlan
}

// GroupView is one group of devices that share a template NAME and chip family (or a built-in module).
type GroupView struct {
	Key     policy.GroupKey
	Devices []DeviceView
	// Reference is nil when the group needs one (a tie, fewer than two live devices) or is a built-in group.
	Reference *policy.Reference
	// NeedsReference: the page says "Choose a reference" and preselects nothing.
	NeedsReference bool
}

// Refs holds the reference device chosen per group in this request (not stored, decision Q3). A group
// that is absent uses the most common layout.
type Refs map[policy.GroupKey]model.DeviceID

// Overview is the content of the tab.
type Overview struct {
	// Groups are the template groups, sorted; a group of one device is listed too (the page folds it).
	Groups []GroupView
	// BuiltIn are the groups of devices on a built-in module; listed only (Q2).
	BuiltIn []GroupView
	// NotJudged are devices without a usable reading or with an unknown chip family.
	NotJudged []DeviceView
	// ReadAt is when the newest live reading of the fleet was taken (shown as "Read live <time>"); zero when none.
	ReadAt time.Time
	// Throttled: a live refresh was asked for but the rate limit stopped it; cached readings are shown.
	Throttled bool
}

// Request names what to apply: the reference of one group and the devices that get it.
type Request struct {
	Group     policy.GroupKey
	Reference model.DeviceID
	Devices   []model.DeviceID
	// Chosen: the person picked the reference (ref in the URL); false when it is the preselected most
	// common one. Only the audit uses it.
	Chosen bool
}

// PreviewRow is one device of a preview.
type PreviewRow struct {
	// Device carries the name, the current template (Device.Reading.Template) and PowerOnStateText.
	Device DeviceView
	// Plan holds the diff, the exact command, the restart flag, and the refusal with its reason.
	Plan policy.ApplyPlan
	// Warning is a caution that does not block, for example the template NAME growing past the room
	// settings have (crOverflow, T15) or a flash write; "" when there is none.
	Warning string
}

// Excluded is a device of the request that is not sent anything, with the reason.
type Excluded struct {
	Device DeviceView
	// Why is "matches the reference", "is the reference", or the refusal's sentence.
	Why string
	// Refusal is policy.RefuseNone for the first two.
	Refusal policy.Refusal
}

// Preview is what applying a Request would do, judged from fresh live readings.
type Preview struct {
	Group     policy.GroupKey
	Reference DeviceView
	// ReferenceTemplate is the template the devices get (the reference's reading).
	ReferenceTemplate policy.GPIOTemplate
	// Rows are the devices that are sent the template, sorted by device id.
	Rows []PreviewRow
	// Excluded are the devices of the request that get nothing: the reference, matching devices, refusals.
	Excluded []Excluded
	// Applicable is len(Rows).
	Applicable int
	// Digest covers the reference's layout, every row's device id, current layout and command text.
	// Apply rebuilds the preview and refuses with ErrStaleDigest when it differs.
	Digest string
	// BackupLine: "A backup is taken first; nothing is sent if it fails."
	BackupLine string
	// RestartLine: the restart warning, with the PowerOnState behaviour when known.
	RestartLine string
}

// Refusal names a device the transaction did not accept and why.
type Refusal struct {
	DeviceID model.DeviceID
	Reason   string
}

// Errors of Apply.
var (
	// ErrStaleDigest: the devices or the reference changed since the preview; show a fresh preview (409).
	ErrStaleDigest = errors.New("templates: the preview is out of date")
	// ErrNothingToApply: no device of the request can be sent the template.
	ErrNothingToApply = errors.New("templates: no device to apply the template to")
	// ErrBadReference: the reference is unknown, not in the group, or could not be read live.
	ErrBadReference = errors.New("templates: the reference device cannot be used")
	// ErrNoEngine: this server runs no transactions (the OTA engine is off).
	ErrNoEngine = errors.New("templates: this server cannot run transactions")
)

// Service is what the pages use. Reading methods are for operators, Apply for admins (the caller checks).
type Service interface {
	// Overview groups the fleet. refresh asks for new live reads (rate limited like the policy live check);
	// without it only cached readings and backups are used, and no device is contacted.
	Overview(ctx context.Context, refs Refs, refresh bool) (Overview, error)
	// Preview re-reads the devices of the request and plans the apply. It never changes a device.
	Preview(ctx context.Context, req Request) (Preview, error)
	// Apply rebuilds the preview, compares its digest, and starts one transaction (Parallel 1, backup first).
	// Devices the engine refuses (for example an active job) come back as refusals while the others start.
	Apply(ctx context.Context, actor string, req Request, digest string) (model.BatchID, []Refusal, error)
}
