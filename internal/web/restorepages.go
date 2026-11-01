package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The restore pages (design v2 screen "restore", WP P10): the confirmation with what will
// change, then the progress of the job in four steps, ending done or failed.

// restoreConfirmView is the confirmation of a restore.
type restoreConfirmView struct {
	Sub        string // "Living Room Ceiling Light · backup from 22 Oct 2026, 09:41 · Before update · Tasmota 14.2.0 · 4.1 KiB"
	Error      string
	Missing    bool // the acknowledgement was not ticked
	Refused    bool // the live checks refuse this restore: no form
	HasPreview bool
	Changes    []ui.DiffRow // Old = now on the device, New = after the restore
	Risks      []string     // risks the admin may accept (the live checks' detail): warn banners
	Warnings   []string     // what the restore will do that can hurt: danger banners
	Action     string
	CSRF       string
	Hidden     []ui.KVRow // the risk flags the form carries
	Ack        string     // the acknowledgement's text
	Path       string     // how the restore reaches the device
	Cred       string     // the sentence about old credentials and the transfer
	CancelHref string
	// Files (#324)
	Files     *filesCard // the backup's files; nil when it has none to show
	FilesNote string     // the backup holds no files, and why
	FilesWarn string     // files exist in the backup but this restore does not write them
	StepsText []string   // the "What happens" list
}

// filesCard is the list of files on the confirmation page.
type filesCard struct {
	Title string
	Rows  []fileRowView
	Foot  string
}

// fileRowView is one line of a file list (confirmation page, progress page).
type fileRowView struct {
	Name       string
	Size       string
	PrivateKey bool
	State      string // badge label; empty on the confirmation page
	Tone       ui.Tone
	Why        string // second line
	Stays      bool   // a file that stays as it is on the device
}

const privateKeyTip = "TLS private key: the content is never shown, the archive is admin only"

// fileRowsOf lists what a plan writes, then the files the backup does not hold.
func fileRowsOf(plan restore.FilesPlan) []fileRowView {
	var rows []fileRowView
	for _, f := range plan.Files {
		rows = append(rows, fileRowView{Name: f.Name(), Size: humanBytes(f.Size), PrivateKey: f.PrivateKey, Why: fileWhy(f.Name())})
	}
	for _, n := range plan.NotStored {
		rows = append(rows, notStoredRow(n))
	}
	return rows
}

func notStoredRow(n backup.ManifestSkip) fileRowView {
	return fileRowView{Name: strings.TrimPrefix(n.Path, "/"), Size: humanBytes(n.Size), PrivateKey: n.PrivateKey, State: "Not in the backup", Tone: ui.ToneNeutral, Stays: true,
		Why: "Not in the backup (" + lowerFirst(strings.Replace(n.Reason, "larger than", "over", 1)) + "): the file on the device stays as it is"}
}

// fileWhy explains the files whose name alone does not say what they are.
func fileWhy(name string) string {
	switch name {
	case "zbdata", "zb", "zbv4":
		return "Zigbee device data"
	}
	return ""
}

// filesPlural words "6 files".
func filesPlural(n int) string { return plural(n, "file") }

// backupSub is the line under the restore title: device, backup date, trigger, firmware, size.
func backupSub(name string, b model.Backup, files int) string {
	parts := []string{name, "backup from " + shortDate(b.TakenAt)}
	if t := triggerShort(b.Trigger); t != "" {
		parts = append(parts, t)
	}
	if b.Version != "" {
		parts = append(parts, "Tasmota "+b.Version)
	}
	parts = append(parts, humanBytes(b.Size))
	if files > 0 {
		parts = append(parts, filesPlural(files))
	}
	return strings.Join(parts, " · ")
}

// restoreConfirmOf turns the checked restore (confirmPage) into the page's view.
func restoreConfirmOf(v view, p confirmPage) restoreConfirmView {
	c := restoreConfirmView{Sub: backupSub(p.Device.Name, p.Backup, len(p.Files.Files)), Refused: p.Refused, HasPreview: p.HasPreview,
		Action: fmt.Sprintf("%s/backups/%d/restore", v.Base, p.Backup.ID), CSRF: v.CSRF,
		CancelHref: fmt.Sprintf("%s/devices/%d/backups", v.Base, p.Device.ID)}
	if p.Error == needConfirm {
		c.Missing = true
	} else {
		c.Error = p.Error
	}
	for _, r := range p.Rows {
		row := ui.DiffRow{Key: r.Path, Old: r.Old, New: r.New, Changed: true}
		switch {
		case r.Secret:
			row.Old, row.New = "secret (not shown)", "secret value changed (not shown)"
		case r.Hidden:
			row.Old, row.New = "text (visible to admins only)", "text changed (visible to admins only)"
		case r.Added:
			row.Old = "(absent)"
		case r.Removed:
			row.New = "(removed)"
		}
		c.Changes = append(c.Changes, row)
	}
	c.Path = sentence(p.PathNote)
	c.Cred = "Old credentials come back: the Wi-Fi, MQTT and web passwords in the backup replace the current ones. The download is plain HTTP on your device network."
	if p.ViaMQTT {
		c.Cred = "Old credentials come back: the Wi-Fi, MQTT and web passwords in the backup replace the current ones. The configuration travels over your MQTT broker."
	}
	if p.Detail != "" {
		c.Risks = append(c.Risks, sentence(p.Detail))
	}
	if p.MACUnknown {
		chip := "chip family unknown"
		if p.LiveChip != "" {
			chip = "chip " + p.LiveChip
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf("The inventory has no MAC for %s. Device at %s reports MAC %s, %s — this will be recorded as its identity. Check that this is the right device.", p.Device.Name, p.LiveAddr, p.LiveMAC, chip))
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "expect_mac", Value: p.LiveMAC})
	}
	if p.Secrets {
		c.Warnings = append(c.Warnings, "Secret values differ between the backup and the current configuration; they will be replaced.")
	}
	if p.WifiChange {
		c.Warnings = append(c.Warnings, "The Wi-Fi or network settings differ. If the backup holds settings the device cannot use, it may not rejoin the network and will have to be recovered on site.")
	}
	if p.NeedMajor {
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "major_ok", Value: "1"})
	}
	if p.NeedFamily {
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "family_ok", Value: "1"})
	}
	if p.NeedLockout {
		c.Hidden = append(c.Hidden, ui.KVRow{Key: "lockout_ok", Value: "1"})
	}
	c.StepsText = []string{
		"TasmoFleet backs up the current state first, so you can undo this.",
		"The backup is uploaded and the device restarts (relays switch off for a few seconds).",
		"Settings are read back and compared with the backup.",
	}
	ack := "I understand that " + p.Device.Name + " restarts, its configuration is replaced by this backup and the old credentials (Wi-Fi, MQTT, web password) come back"
	c.Hidden = append(c.Hidden, ui.KVRow{Key: "files", Value: strconv.Itoa(confirmedFiles(p.Files))})
	if p.Files.Why != "" {
		c.FilesWarn = sentence(p.Files.Why) // also without a list (a damaged archive has none)
	}
	if n := len(p.Files.Files); n > 0 {
		stored := fmt.Sprintf("%s, %s", filesPlural(n), humanBytes(p.Files.Bytes))
		card := &filesCard{Rows: fileRowsOf(p.Files)}
		if p.Files.Restores() {
			card.Title = "Files written first (" + stored + ")"
			card.Foot = "Files first, then the settings, one restart. Files of the same name are replaced. Nothing on the device is deleted: files that are not in the backup stay as they are."
			c.StepsText = []string{
				"TasmoFleet backs up the current state first, with its files, so you can undo this (a file that backup cannot hold is not overwritten).",
				fmt.Sprintf("The %s are written to the device's filesystem.", filesPlural(n)),
				"The backup is uploaded and the device restarts (relays switch off for a few seconds).",
				"Settings and files are read back and compared with the backup.",
			}
			ack = "I understand that " + p.Device.Name + " restarts, its configuration is replaced by this backup, its " + filesPlural(n) + " are written (files of the same name are replaced) and the old credentials (Wi-Fi, MQTT, web password) come back"
		} else {
			card.Title = "Files in this backup (" + stored + "): not restored"
			card.Foot = "Nothing on the device is deleted or changed."
		}
		c.Files = card
	} else if p.Files.Note != "" {
		c.FilesNote = "This backup holds no files (" + p.Files.Note + ")."
	}
	if p.MACUnknown {
		ack += "; that MAC " + p.LiveMAC + " is recorded as its identity"
	}
	if p.NeedMajor {
		ack += "; that the backup is from another firmware major version"
	}
	if p.NeedLockout {
		ack += "; that this restore changes the MQTT settings of " + p.Device.Name + ", which TasmoFleet reaches only over MQTT, and TasmoFleet may lose it"
	}
	if p.NeedFamily {
		ack += "; and I confirm that the backup belongs to the same chip family as " + p.Device.Name
	}
	c.Ack = ack + "."
	return c
}

func (h *handler) renderConfirm(w http.ResponseWriter, r *http.Request, status int, p confirmPage) {
	v := h.view(r, "Restore backup", "devices", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: p.Device.Name, Path: fmt.Sprintf("/devices/%d", p.Device.ID)}, {Label: "Restore"}}
	h.show(w, r, status, "restore_confirm", v, restoreConfirm(restoreConfirmOf(v, p)))
}

// restoreStepView is one step of the progress list.
type restoreStepView struct {
	N             int
	Label, Detail string
	State         string // done, active, failed or todo
}

// Mark is what the step's circle shows.
func (s restoreStepView) Mark() string {
	switch s.State {
	case "done":
		return "✓"
	case "failed":
		return "✕"
	case "active":
		return "•"
	}
	return fmt.Sprint(s.N)
}

// restoreJobView is the progress page of a restore or a replace, and its polled fragment.
type restoreJobView struct {
	Title, Sub   string
	StatusURL    string // polled while the job runs
	Running      bool
	Done, Failed bool
	Banner       string // after "Restore finished." / "Restore failed."
	Lead         string
	Steps        []restoreStepView
	RollbackHref string // restore the backup taken before the restore
	RollbackText string
	RetryHref    string
	BackHref     string
	DeviceHref   string // a finished replace: open the device
	// The files list beside the steps (#324); empty when this process did not run the job or it had no files.
	FilesMeta string // "2 of 6 written"
	FileRows  []fileRowView
}

// restoreJobOf builds the progress view of a job.
func restoreJobOf(base string, p jobPage, target, pre model.Backup, preFiles bool, prog *restore.FilesProgress) restoreJobView {
	j := p.Job
	replace := j.Kind == store.KindReplace
	v := restoreJobView{Title: "Restore backup", StatusURL: fmt.Sprintf("%s/restores/%d/status", base, j.ID),
		Running: !p.Terminal, Done: j.State == store.RestoreDone, Failed: j.State == store.RestoreFailed,
		BackHref: fmt.Sprintf("%s/devices/%d/backups", base, j.DeviceID)}
	nFiles := 0
	if prog != nil {
		nFiles = len(prog.Files)
	}
	if target.ID != 0 {
		v.Sub = backupSub(p.Device.Name, target, nFiles)
	} else {
		v.Sub = fmt.Sprintf("%s · backup #%d", p.Device.Name, j.BackupID)
	}
	labels := [5]string{"Back up the current state", "Write the files", "Upload the backup to the device", "Restart and wait for the device", "Verify settings and files"}
	if replace {
		v.Title = "Replace device"
		v.Sub = fmt.Sprintf("%s · onto the new device %s", p.Device.Name, j.NewMAC)
		labels = [5]string{"Back up the new device", "Write the files", "Upload the backup to the new device", "Restart and wait for the new device", "Verify settings and files and take over the entry"}
	}
	step := restore.Step(j)
	for i, l := range labels {
		n := i + 1
		s := restoreStepView{N: n, Label: l, State: "todo"}
		switch {
		case n < step:
			s.State = "done"
		case n == step && v.Failed:
			s.State, s.Detail = "failed", sentence(j.Detail)
		case n == step && n == restore.StepFiles:
			s.State, s.Detail = "active", upperFirst(j.Detail) // "Writing lib/x.be (3 of 6)"
		case n == step:
			s.State, s.Detail = "active", sentence(j.Detail)
		}
		if s.State == "done" {
			switch n {
			case restore.StepBackup:
				if pre.ID != 0 {
					s.Detail = "Saved " + shortDate(pre.TakenAt) + " · " + humanBytes(pre.Size)
					if preFiles {
						s.Detail += " and its files"
					}
					if t := backupTriggerText(pre.Trigger, pre.TriggerRef); t != "" {
						s.Detail += " · labelled “" + t + "”"
					}
				}
			case restore.StepFiles:
				s.Detail = filesStepDetail(j, prog)
			case restore.StepUpload:
				if target.ID != 0 {
					s.Detail = humanBytes(target.Size) + " uploaded over " + map[bool]string{true: "MQTT", false: "HTTP"}[j.Transport == model.TransportMQTT]
				}
			case restore.StepRestart:
				s.Detail = "Back online"
			case restore.StepVerify:
				_, rest := restore.SplitDetail(j.Detail)
				s.Detail = sentence(rest)
			}
		}
		v.Steps = append(v.Steps, s)
	}
	what := "Restore"
	if replace {
		what = "Replace"
	}
	switch {
	case v.Done && replace:
		v.Lead = what + " finished."
		v.Banner = p.Device.Name + " now stands for the new device (" + j.NewMAC + ")."
		if restore.FilesWritten(j.Detail) {
			v.Banner += " Its files and settings are those from " + shortDate(target.TakenAt) + "."
		}
		v.DeviceHref = fmt.Sprintf("%s/devices/%d", base, j.DeviceID)
	case v.Done:
		v.Lead = what + " finished."
		withFiles := restore.FilesWritten(j.Detail)
		what := "settings"
		if withFiles {
			what = "settings and files"
		}
		v.Banner = "The device is back online with the " + what + " from " + shortDate(target.TakenAt) + "."
		if j.Verified {
			v.Banner += " Verified: the device holds the restored configuration"
			if withFiles && prog != nil {
				v.Banner += " and all " + filesPlural(prog.Count(restore.FileWritten)+prog.Count(restore.FileSame))
			}
			v.Banner += "."
		}
	case v.Failed && pre.ID != 0 && (step > restore.StepUpload || restore.FilesChanged(j.Detail)):
		// Only a failure after the upload (or after files were written) may have changed the device;
		// before, the failure text says nothing was changed and there is nothing to roll back.
		v.Lead = what + " failed."
		v.Banner = "The state before was backed up at " + shortDate(pre.TakenAt) + " before anything changed, so you can roll back."
		if !replace {
			v.RollbackHref = fmt.Sprintf("%s/backups/%d/restore", base, pre.ID)
			v.RollbackText = "Restore the " + shortDate(pre.TakenAt) + " backup"
		}
	case v.Failed:
		v.Lead = what + " failed."
		v.Banner = "Nothing was changed on the device."
	}
	if prog != nil && len(prog.Files) > 0 {
		v.FilesMeta = fmt.Sprintf("%d of %d written", prog.Count(restore.FileWritten)+prog.Count(restore.FileSame), len(prog.Files))
		for _, f := range prog.Files {
			v.FileRows = append(v.FileRows, progressRow(f))
		}
		for _, n := range prog.NotStored {
			r := notStoredRow(n)
			r.Why = "Over 1 MiB: not stored, so the restore leaves the file on the device as it is"
			if !strings.Contains(n.Reason, "1 MiB") {
				r.Why = "Not stored (" + n.Reason + "), so the restore leaves the file on the device as it is"
			}
			v.FileRows = append(v.FileRows, r)
		}
	}
	if v.Failed && !replace {
		v.RetryHref = fmt.Sprintf("%s/backups/%d/restore", base, j.BackupID)
	}
	return v
}

// jobBackups looks up the restored backup and the one taken before the restore (zero when
// gone or not taken yet). A failed read only costs the details.
func (h *handler) jobBackups(r *http.Request, j restore.Job) (target, pre model.Backup, preFiles bool) {
	list, err := h.d.Backups.List(r.Context(), j.DeviceID)
	if err != nil {
		h.log.WarnContext(r.Context(), "restore page: backups", "err", err)
		return target, pre, false
	}
	for _, b := range list {
		switch b.ID {
		case j.BackupID:
			target = b
		case j.PreBackupID:
			pre = b
		}
	}
	for _, b := range list { // the backup taken before also holds the device's files (they can be put back)
		if b.Kind == model.BackupFiles && pre.ID != 0 && b.TakenAt.Equal(pre.TakenAt) {
			preFiles = true
		}
	}
	return target, pre, preFiles
}

// writeComponent answers with one component (an htmx fragment).
func writeComponent(w http.ResponseWriter, r *http.Request, status int, c templ.Component) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	return c.Render(r.Context(), w)
}

// filesStepDetail is the line under the finished files step: what the step did, plus the files the
// backup does not hold. While later steps run the job's Detail holds their status, so the line comes
// from the progress; the stored summary (first line of a finished job's Detail) serves after a restart.
func filesStepDetail(j restore.Job, prog *restore.FilesProgress) string {
	files, _ := restore.SplitDetail(j.Detail)
	if prog != nil && prog.Summary != "" {
		files = prog.Summary
	}
	if files == "" {
		if j.State == store.RestoreDone {
			return "No files in this backup"
		}
		return ""
	}
	files = upperFirst(files)
	if prog != nil && len(prog.NotStored) > 0 {
		if len(prog.NotStored) == 1 {
			files += " · " + strings.TrimPrefix(prog.NotStored[0].Path, "/") + " is not in the backup and stays as it is"
		} else {
			files += " · " + filesPlural(len(prog.NotStored)) + " are not in the backup and stay as they are"
		}
	}
	return files
}

// confirmedFiles is the number the confirmation form carries: the files this restore writes first,
// 0 when it writes none.
func confirmedFiles(p restore.FilesPlan) int {
	if p.Restores() {
		return len(p.Files)
	}
	return 0
}

// progressRow is a file of the files step with its state.
func progressRow(f restore.FileProgress) fileRowView {
	r := fileRowView{Name: f.Name(), Size: humanBytes(f.Size), PrivateKey: f.PrivateKey, Why: fileWhy(f.Name())}
	switch f.State {
	case restore.FileWriting:
		r.State, r.Tone = "Writing", ui.ToneAccent
	case restore.FileWritten:
		r.State, r.Tone = "Written", ui.ToneOK
	case restore.FileSame:
		r.State, r.Tone = "Already the same", ui.ToneNeutral
	case restore.FileSkipped:
		r.State, r.Tone, r.Why = "Skipped", ui.ToneWarn, upperFirst(f.Why)
	case restore.FileFailed:
		r.State, r.Tone = "Failed", ui.ToneDanger
	case restore.FileWaiting:
		r.State, r.Tone = "Waiting", ui.ToneNeutral
	}
	return r
}

// lowerFirst makes the first letter lower case, nothing else ("Over 1 MiB" -> "over 1 MiB").
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
