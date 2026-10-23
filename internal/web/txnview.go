package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The wording and view helpers of the Updates pages (design v2, WP P4): transaction ids, titles,
// results, the short state of a device row and the "when" of a history row.

// txID is a transaction's id as the pages show it: "TX-0143" (the prototype's form). The URL keeps
// the bare number.
func txID(id model.BatchID) string { return fmt.Sprintf("TX-%04d", id) }

// txTitle names what a transaction does: "Update to Tasmota 14.3.0", "Make compliant · TelePeriod",
// "Back up 58 devices", "Send Status 5". The command is the redacted text (it may hold a secret).
func txTitle(j model.Job, devices int) string {
	switch {
	case j.IsPolicy():
		if s := policySettings(j.Commands); s != "" {
			return "Make compliant · " + s
		}
		return "Make compliant"
	case j.IsCommand():
		return txTarget(j)
	case j.IsBackup():
		return "Back up " + plural(devices, "device")
	case j.TargetCustom != "":
		return "Update to custom firmware " + j.TargetRelease
	}
	return "Update to Tasmota " + j.TargetRelease
}

// policySettings lists the settings a "Make compliant" job changes, in the order of its commands:
// the first word of each command line ("TelePeriod 300" -> "TelePeriod").
func policySettings(commands string) string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(commands, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		out = append(out, f[0])
	}
	return strings.Join(out, ", ")
}

// txResult is the result badge of a transaction: Running, Completed, Cancelled, Failed, or what
// came of the devices when they differ ("30 done · 1 failed").
func txResult(done, running, queued, failed, canceled int) (string, ui.Tone) {
	total := done + running + queued + failed + canceled
	switch {
	case running+queued > 0:
		return "Running", ui.ToneAccent
	case done == total:
		return "Completed", ui.ToneOK
	case canceled == total:
		return "Cancelled", ui.ToneNeutral
	case failed == total:
		return "Failed", ui.ToneDanger
	}
	var parts []string
	for _, p := range []struct {
		n    int
		word string
	}{{done, "done"}, {failed, "failed"}, {canceled, "cancelled"}} {
		if p.n > 0 {
			parts = append(parts, strconv.Itoa(p.n)+" "+p.word)
		}
	}
	if failed > 0 {
		return strings.Join(parts, " · "), ui.ToneDanger
	}
	return strings.Join(parts, " · "), ui.ToneNeutral
}

// jobBadge is the short state of one device of a transaction, the word of its badge: what the
// device is doing now. The OtaUrl step is the start of the device's download, the Upgrade step
// its download and flash in one pass (Tasmota writes the image while it receives it).
func jobBadge(j model.Job) (string, ui.Tone) {
	if j.CancelRequested && !j.State.Terminal() {
		return "Cancelling", ui.ToneNeutral // a flash cannot be stopped: the job ends after this step
	}
	switch j.State {
	case model.JobQueued:
		return "Queued", ui.ToneNeutral
	case model.JobPreflight:
		return "Checking", ui.ToneAccent
	case model.JobBackup:
		return "Backing up", ui.ToneAccent
	case model.JobSetOtaURL:
		return "Downloading", ui.ToneAccent
	case model.JobUpgradeIssued:
		return "Flashing", ui.ToneAccent
	case model.JobAwaitReboot:
		return "Rebooting", ui.ToneAccent
	case model.JobVerify:
		return "Verifying", ui.ToneAccent
	case model.JobRestoreOtaURL, model.JobRestorePower:
		return "Restoring", ui.ToneAccent
	case model.JobApplying:
		return "Applying", ui.ToneAccent
	case model.JobSucceeded:
		return "Done", ui.ToneOK
	case model.JobFailed:
		return "Failed", ui.ToneDanger
	case model.JobCancelled:
		return "Cancelled", ui.ToneNeutral
	}
	return label(string(j.State)), ui.ToneNeutral
}

// changeBadge is the state of one change of a "Make compliant" device card.
func changeBadge(s model.JobState) (string, ui.Tone) {
	switch s { //nolint:exhaustive // every other state is the job at work
	case model.JobSucceeded:
		return "Applied", ui.ToneOK
	case model.JobFailed:
		return "Failed", ui.ToneDanger
	case model.JobCancelled:
		return "Cancelled", ui.ToneNeutral
	case model.JobQueued:
		return "Queued", ui.ToneNeutral
	}
	return "Applying", ui.ToneAccent
}

// whenText is the "when" of a history row: "Today 09:41", "Yesterday 21:10", "19 Aug, 20:02",
// with the year only when it is not this year's.
func whenText(t, now time.Time) string {
	t = t.In(now.Location())
	const day = "2006-01-02"
	switch t.Format(day) {
	case now.Format(day):
		return "Today " + t.Format("15:04")
	case now.AddDate(0, 0, -1).Format(day):
		return "Yesterday " + t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("2 Jan, 15:04")
	}
	return t.Format("2 Jan 2006, 15:04")
}

// chipName is a chip family as the datasheets write it: "ESP8266", "ESP32", "ESP32-C3".
func chipName(f model.ChipFamily) string {
	s := strings.ToUpper(string(f))
	if rest, ok := strings.CutPrefix(s, "ESP32"); ok && rest != "" {
		return "ESP32-" + rest
	}
	return s
}

// sizeText is a file size the way the update pages show it: "658 KB", "1.9 MB".
func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + " KB"
	}
	return strconv.FormatInt(n, 10) + " B"
}

// updatePerDevice is the time one device's update typically takes on a LAN (preflight, backup,
// download and flash, reboot, verify). It is for the estimate on the update pages only; the
// state machine's timeouts are the limits.
const updatePerDevice = time.Minute

// updateEstimate is how long n devices take when parallel of them update at a time: one round per
// parallel devices, worded in whole minutes ("6 min").
func updateEstimate(n, parallel int) string {
	if n <= 0 {
		return "–"
	}
	if parallel < 1 {
		parallel = 1
	}
	rounds := (n + parallel - 1) / parallel
	return strconv.Itoa(int((time.Duration(rounds) * updatePerDevice).Minutes())) + " min"
}

// chipNameOf is chipName for a family held as text (the picker's quick selection).
func chipNameOf(f string) string { return chipName(model.ChipFamily(f)) }

// healthDot is the colour of a device's status dot in the update picker.
func healthDot(s model.HealthState) ui.Tone {
	switch s {
	case model.HealthOnline:
		return ui.ToneOK
	case model.HealthOffline, model.HealthAuthFailed:
		return ui.ToneDanger
	case model.HealthStale:
		return ui.ToneWarn
	case model.HealthUnknown:
	}
	return ui.ToneNeutral
}

// upstreamFile is the upstream file the release would install on a device as it runs now, ""
// when that cannot be said (unknown identity, an upload). The preview decides for real.
func upstreamFile(id model.Identity, release string) string {
	if release == "" || strings.HasPrefix(release, customReleasePrefix) || id.Family == model.ChipUnknown {
		return ""
	}
	sel, err := fwselect.Select(id, fwselect.Choice{Release: release, Policy: model.PolicyReinstall})
	if err != nil {
		return ""
	}
	return sel.Filename
}

// releaseOptions are the release cards of the new update: the newest two upstream releases ("Latest"
// on the first, with the release date), every upload, and the older releases apart (folded).
func releaseOptions(rels []firmware.Release, uploads []firmware.CustomImage, chosen string) (cards, older []ui.Option) {
	for i, rel := range rels {
		o := ui.Option{Value: rel.Version, Label: "Tasmota " + rel.Version, Checked: rel.Version == chosen}
		var d []string
		if i == 0 {
			d = append(d, "Latest")
		}
		if !rel.Published.IsZero() {
			d = append(d, dateText(rel.Published))
		}
		o.Text = strings.Join(d, " · ")
		if i < 2 || o.Checked {
			cards = append(cards, o)
		} else {
			older = append(older, o)
		}
	}
	for _, img := range uploads {
		v := customReleasePrefix + img.Artifact.Custom
		cards = append(cards, ui.Option{Value: v, Label: "Custom firmware " + img.Artifact.Release, Checked: v == chosen,
			Text: img.Label + " · uploaded " + dateText(img.UploadedAt)})
	}
	return cards, older
}

// dateText is a date as the release cards show it: "6 Oct 2026".
func dateText(t time.Time) string { return t.Format("2 Jan 2006") }

// otaParallel is how many devices update at a time: the system setting OTA_PARALLEL, else the
// engine's default of 2.
func (h *handler) otaParallel() int {
	if h.d.SysConfig != nil {
		for _, f := range h.d.SysConfig.State() {
			if f.Key == "OTA_PARALLEL" {
				if n, err := strconv.Atoi(f.Value); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 2
}

func bid(id model.BatchID) string { return strconv.FormatInt(int64(id), 10) }
func jid(id model.JobID) string   { return strconv.FormatInt(int64(id), 10) }

func joinComma(s []string) string { return strings.Join(s, ", ") }

// txListView is the Updates page: the running transactions as cards, then every transaction.
type txListView struct {
	Base     string
	CanStart bool // updates are switched on
	Running  []ui.TxnData
	History  []txHistRow
	// Device is the name of the device the list is limited to ("" for the whole fleet); Self is
	// the list's own address, so the live refresh keeps the filter.
	Device, Self string
}

// txHistRow is one transaction of the history: id, title, devices, result, who and when.
type txHistRow struct {
	ID, Title, Devices, Result, By, When, At, Href string
	Tone                                           ui.Tone
}

// txnPage is one transaction: header, progress, then its devices with the open log, or for a
// "Make compliant" transaction (Policy) one card per device with its changes.
type txnPage struct {
	Base, CSRF, LiveURL string
	Batch               model.BatchID
	ID, Title, Sub      string
	State               string
	StateTone           ui.Tone
	Policy              bool
	Active              bool

	Bar                                                           ui.TxnData
	Total, Done, Running, Queued, Failed, Canceled, CancelPending int
	RetryN                                                        int // failed and cancelled devices: "Retry failed (N)"
	RetryOf                                                       model.BatchID
	RetriedBy                                                     []model.BatchID
	CancelText                                                    string

	Rows  []txnRow
	Sel   *txnSel
	Cards []fixCard
}

// txnRow is one device of a transaction; the row links to its log.
type txnRow struct {
	JobID          model.JobID
	Name, From, To string
	Badge          string
	Tone           ui.Tone
	Bar            bool // a running device shows its progress
	Pct            int
	Href           string
	Selected       bool
}

// txnSel is the device whose log is open.
type txnSel struct {
	JobID     model.JobID
	DeviceID  model.DeviceID
	Name      string
	CanCancel bool
	CanRetry  bool
	Log       []logLine
}

// fixCard is one device of a "Make compliant" transaction: its changes and their state.
type fixCard struct {
	Name, Href, Reason string
	Tone               ui.Tone
	Changes            []fixChange
}

type fixChange struct {
	Text, Label string
	Tone        ui.Tone
}

// errEvents are the job events that report a failure: their log lines are drawn in red.
var errEvents = map[string]bool{"command_failed": true, "guard_refused": true, "timeout": true, "backup_failed": true}
