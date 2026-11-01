package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// RestoreService is the part of restore.Service the UI uses. Optional: the
// restore pages exist only when it is set (they need the firmware listener).
type RestoreService interface {
	StartWith(ctx context.Context, actor string, backup model.BackupID, o restore.StartOptions) (restore.Job, error)
	Assess(ctx context.Context, backup model.BackupID) (restore.Assessment, error)
	Get(ctx context.Context, id store.RestoreID) (restore.Job, error)
	List(ctx context.Context, device model.DeviceID, limit int) ([]restore.Job, error)
	// FilesProgress is the per-file progress of a job the service ran in this process (#324).
	FilesProgress(id store.RestoreID) (restore.FilesProgress, bool)
}

func (h *handler) registerRestore(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	route("GET /backups/{id}/restore", adm, h.restoreConfirm)
	route("POST /backups/{id}/restore", adm, h.restoreStart)
	route("GET /restores/{id}", adm, h.restoreJob)
	route("GET /restores/{id}/status", adm, h.restoreStatus)
}

type confirmPage struct {
	Backup      model.Backup
	Device      model.Device
	Rows        []diffRow
	HasPreview  bool
	WifiChange  bool
	Secrets     bool // the preview holds changed secrets: they come back
	Error       string
	Refused     bool   // the live checks refuse this restore: no form
	NeedMajor   bool   // the backup is from another firmware major version
	NeedFamily  bool   // the chip family of the backup could not be checked
	NeedLockout bool   // a restore over MQTT that changes the device's MQTT settings
	MACUnknown  bool   // the inventory has no MAC: the one the device reports is recorded
	LiveMAC     string // that MAC, shown and confirmed
	LiveAddr    string
	LiveChip    string
	Detail      string // why the risks above apply
	ViaMQTT     bool   // the restore goes over MQTT (FileUpload)
	PathNote    string // how it reaches the device, as the transport choice words it
	Files       restore.FilesPlan
}

// confirmData fills the page shared by the GET and the refusals. The live
// checks run here, so the one confirmation names every risk up front and a
// restore that would be refused never shows a form.
func (h *handler) confirmData(r *http.Request, b model.Backup, d model.Device) confirmPage {
	p := confirmPage{Backup: b, Device: d}
	p.Rows, p.WifiChange, p.HasPreview = h.preview(r, b)
	for _, row := range p.Rows {
		p.Secrets = p.Secrets || row.Secret
	}
	a, err := h.d.Restore.Assess(r.Context(), b.ID)
	if err != nil {
		p.Refused, p.Error = true, assessText(err)
		if !isRefusal(err) {
			h.log.WarnContext(r.Context(), "restore: assess", "err", err)
		}
		return p
	}
	p.NeedMajor, p.NeedFamily, p.NeedLockout, p.MACUnknown, p.Detail = a.Major, a.Family, a.Lockout, a.MACUnknown, a.Detail
	p.LiveMAC, p.LiveAddr, p.LiveChip = a.LiveMAC, a.Address, a.Chip
	p.ViaMQTT, p.PathNote = a.ViaMQTT, a.PathNote
	p.Files = a.Files
	return p
}

func isRefusal(err error) bool {
	for _, t := range []error{restore.ErrIdentity, restore.ErrFamilyMismatch, restore.ErrVersionUnknown, restore.ErrNewerBackup,
		restore.ErrNotDump, restore.ErrNoAddress, restore.ErrBusy, restore.ErrOTAActive, restore.ErrNoPath, restore.ErrNotConfigured} {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}

// assessText words a failed assessment: the fixed refusal sentence, or for a
// device that did not answer a plain "try again" (device errors never reach
// the page).
func assessText(err error) string {
	switch {
	case errors.Is(err, restore.ErrBusy), errors.Is(err, restore.ErrOTAActive):
		return "Another operation on this device is running. Try again when it has finished."
	case isRefusal(err):
		return refusalText(err)
	default:
		return "The device could not be checked right now (it did not answer as expected). Nothing was changed. Check that it is online and try again."
	}
}

// restoreTarget loads the dump backup of the URL and its device.
func (h *handler) restoreTarget(w http.ResponseWriter, r *http.Request) (model.Backup, model.Device, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return model.Backup{}, model.Device{}, false
	}
	_, b, err := h.d.Backups.Open(r.Context(), model.BackupID(n))
	if errors.Is(err, backup.ErrFileMissing) && b.Kind == model.BackupConfigDump {
		h.backupFileMissing(w, r, b)
		return b, model.Device{}, false
	}
	if err != nil || b.Kind != model.BackupConfigDump {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.log.ErrorContext(r.Context(), "restore: open backup", "err", err)
		}
		h.notFound(w, r)
		return b, model.Device{}, false
	}
	d, err := h.d.Inventory.Get(r.Context(), b.DeviceID)
	if err != nil {
		h.notFound(w, r)
		return b, d, false
	}
	return b, d, true
}

// preview compares the backup to restore with the newest other decoded
// backup of the device: what the restore would roughly change.
func (h *handler) preview(r *http.Request, b model.Backup) ([]diffRow, bool, bool) {
	list, err := h.d.Backups.List(r.Context(), b.DeviceID)
	if err != nil {
		return nil, false, false
	}
	var target, current *model.Backup
	for i := range list {
		if list[i].Kind != model.BackupDecoded {
			continue
		}
		if list[i].TakenAt.Equal(b.TakenAt) {
			target = &list[i]
		} else if current == nil {
			current = &list[i]
		}
	}
	if target == nil || current == nil {
		return nil, false, false
	}
	dt, _, err1 := h.d.Backups.Open(r.Context(), target.ID)
	dc, _, err2 := h.d.Backups.Open(r.Context(), current.ID)
	if err1 != nil || err2 != nil {
		return nil, false, false
	}
	changes, err := backup.Diff(dc, dt)
	if err != nil {
		return nil, false, false
	}
	changes = backup.FilterVolatile(changes)
	wifi := false
	for _, c := range changes {
		if strings.HasPrefix(c.Path, "sta_") || c.Path == "hostname" || strings.HasPrefix(c.Path, "ip_address") {
			wifi = true
		}
	}
	return toRows(changes), wifi, true
}

func (h *handler) restoreConfirm(w http.ResponseWriter, r *http.Request) {
	b, d, ok := h.restoreTarget(w, r)
	if !ok {
		return
	}
	h.renderConfirm(w, r, http.StatusOK, h.confirmData(r, b, d))
}

func (h *handler) restoreStart(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	b, d, ok := h.restoreTarget(w, r)
	if !ok {
		return
	}
	if r.PostFormValue("confirm") != "1" {
		p := h.confirmData(r, b, d)
		p.Error = needConfirm
		h.renderConfirm(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	opt := restore.StartOptions{AllowMajorChange: r.PostFormValue("major_ok") == "1", AllowUnknownFamily: r.PostFormValue("family_ok") == "1", AllowMQTTLockout: r.PostFormValue("lockout_ok") == "1",
		ExpectMAC: r.PostFormValue("expect_mac")}
	job, err := h.d.Restore.StartWith(r.Context(), actor(r), b.ID, opt)
	var need *restore.OverrideError
	switch {
	case errors.As(err, &need):
		p := h.confirmData(r, b, d)
		p.NeedMajor, p.NeedFamily, p.NeedLockout, p.Detail = need.Major, need.Family, need.Lockout, need.Detail
		p.MACUnknown = p.MACUnknown || need.MAC != ""
		p.Error = "Something changed since the page was shown: confirm the risk named below."
		h.renderConfirm(w, r, http.StatusUnprocessableEntity, p)
	case errors.Is(err, restore.ErrBusy), errors.Is(err, restore.ErrOTAActive):
		h.message(w, r, http.StatusConflict, h.view(r, "Restore not started", "devices", "Another operation on this device is running. Try again when it has finished."))
	case errors.Is(err, restore.ErrIdentity), errors.Is(err, restore.ErrFamilyMismatch), errors.Is(err, restore.ErrVersionUnknown),
		errors.Is(err, restore.ErrNewerBackup), errors.Is(err, restore.ErrNotDump), errors.Is(err, restore.ErrNoAddress):
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Restore not started", "devices", refusalText(err)))
	case err != nil:
		h.fail(w, r, err)
	default:
		redirect(w, r, "/restores/"+strconv.FormatInt(int64(job.ID), 10))
	}
}

// refusalText is the fixed sentence for a refusal; wrapped device errors
// never reach the page.
func refusalText(err error) string {
	switch {
	case errors.Is(err, restore.ErrIdentity):
		return "The device at this address is not the device in the inventory (or its MAC is not stored). Nothing was changed. Probe the device and try again."
	case errors.Is(err, restore.ErrFamilyMismatch):
		return "The backup comes from another chip family than this device. It cannot be restored."
	case errors.Is(err, restore.ErrVersionUnknown):
		return "The firmware version of the device or of the backup is unknown. Probe the device and try again."
	case errors.Is(err, restore.ErrNewerBackup):
		return "The backup comes from a newer firmware than the device runs. Update the device first, or restore an older backup."
	case errors.Is(err, restore.ErrNotDump):
		return "Only a config dump can be restored."
	case errors.Is(err, restore.ErrNoPath):
		return "TasmoFleet has no way to restore this device right now: " + strings.TrimPrefix(err.Error(), restore.ErrNoPath.Error()+": ")
	case errors.Is(err, restore.ErrNotConfigured):
		return "A restore needs the firmware base URL (FIRMWARE_BASE_URL) or a device on the MQTT broker with its MQTT password set."
	default:
		return "The device address does not resolve to exactly one IP address."
	}
}

type jobPage struct {
	Job       restore.Job
	Device    model.Device
	Base      string
	PreBackup model.BackupID
	Terminal  bool
}

func (h *handler) jobData(r *http.Request) (jobPage, bool, error) {
	n, perr := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if perr != nil || n <= 0 {
		return jobPage{}, false, nil //nolint:nilerr // a malformed ID is "not found", not a server error
	}
	j, err := h.d.Restore.Get(r.Context(), store.RestoreID(n))
	if errors.Is(err, store.ErrNotFound) {
		return jobPage{}, false, nil
	}
	if err != nil {
		return jobPage{}, false, err
	}
	d, _ := h.d.Inventory.Get(r.Context(), j.DeviceID) // a deleted device shows an empty name
	return jobPage{Job: j, Device: d, Base: h.view(r, "", "", nil).Base, PreBackup: j.PreBackupID, Terminal: j.Terminal()}, true, nil
}

func (h *handler) restoreJob(w http.ResponseWriter, r *http.Request) {
	p, ok, err := h.jobData(r)
	switch {
	case err != nil:
		h.fail(w, r, err)
	case !ok:
		h.notFound(w, r)
	default:
		target, pre, preFiles := h.jobBackups(r, p.Job)
		jv := restoreJobOf(p.Base, p, target, pre, preFiles, h.filesProgress(p.Job.ID))
		v := h.view(r, jv.Title, "devices", nil)
		v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: p.Device.Name, Path: fmt.Sprintf("/devices/%d", p.Device.ID)}, {Label: lastCrumb(p.Job)}}
		h.show(w, r, http.StatusOK, "restore_job", v, restoreJobPage(jv))
	}
}

// lastCrumb names the job page in the trail.
func lastCrumb(j restore.Job) string {
	if j.Kind == store.KindReplace {
		return "Replace"
	}
	return "Restore"
}

func (h *handler) restoreStatus(w http.ResponseWriter, r *http.Request) {
	p, ok, err := h.jobData(r)
	switch {
	case err != nil:
		h.fail(w, r, err)
	case !ok:
		h.notFound(w, r)
	default:
		target, pre, preFiles := h.jobBackups(r, p.Job)
		_ = writeComponent(w, r, http.StatusOK, restoreStatusBody(restoreJobOf(p.Base, p, target, pre, preFiles, h.filesProgress(p.Job.ID)))) // a failed write means the client went away
	}
}

// filesProgress is the per-file progress of a job, nil when this process did not run it.
func (h *handler) filesProgress(id store.RestoreID) *restore.FilesProgress {
	if p, ok := h.d.Restore.FilesProgress(id); ok {
		return &p
	}
	return nil
}
