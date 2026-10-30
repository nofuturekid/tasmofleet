package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// healthStates is the display order of the health states.
var healthStates = []model.HealthState{
	model.HealthOnline, model.HealthStale, model.HealthOffline, model.HealthAuthFailed, model.HealthUnknown,
}

// vitals are the uptime, Wi-Fi and restart cells of a row.
type vitals struct {
	Uptime  string    // compact, "12h 55m"
	Restart time.Time // zero when unknown
	RSSI    int       // percent, 0 unknown
	Bars    int       // 0..4
	DBm     int       // approximate, from the percentage
	// WifiW is the filled width of the 34 px signal bar (the prototype's scale: -92 dBm empty,
	// about -40 dBm full, at least a sliver), WifiOK a signal above -70 dBm (green, else amber).
	WifiW  int
	WifiOK bool
	// Down: the device stopped answering (offline, wrong password); Seen is when it last answered,
	// shown in the Uptime column instead of the uptime ("seen 3h ago", the prototype's).
	Down bool
	Seen time.Time
}

type deviceRow struct {
	Device  model.Device
	Health  model.Health
	Minimal bool
	Vitals  vitals
	Power   powerCell
	// Outdated and Deviations are set by fleetFacts.annotate on the device
	// list and the dashboard only (N7); elsewhere they stay zero.
	// ViaMQTT: commands to the device go over MQTT now (Paths.Commands is MQTT), the "MQTT" marker.
	ViaMQTT    bool
	Outdated   bool // a newer stable release is in the cached feed
	Deviations int  // policy values that differ from a rule
}

type rowsData struct {
	Base    string
	Admin   bool // Restart and Delete in the row menu
	Console bool // Console and Restart need the command service
	Select  bool
	Rows    []deviceRow
	None    bool   // the inventory is empty (not just filtered to nothing)
	AddHref string // where Add devices leads for an admin, empty for everyone else
}

type deviceFilter struct{ Q, Health, Family, Tag, Chip string }

type devicesPage struct {
	Filter   deviceFilter
	Query    string
	States   []model.HealthState
	Families []facet // the Chip select: every chip family in the fleet with its device count
	Tags     []facet
	Rows     []deviceRow
	RowsData rowsData
	Summary  fleetSummary // N7: Summary.Chips are the filter chips with their counts
	Sort     tableSort
	SortBase string // the page's URL with the filters, without the sort
	// Table and Cards are the rendered device table and phone cards (devicelist.go).
	Table, Cards template.HTML
}

func (h *handler) rows(ctx context.Context) ([]deviceRow, error) {
	devs, err := h.d.Inventory.List(ctx)
	if err != nil {
		return nil, err
	}
	all := h.d.Health.All()
	minimal, _ := h.d.Health.(interface{ Minimal(model.DeviceID) bool })
	failing := h.failingHTTP(ctx, devs)
	out := make([]deviceRow, 0, len(devs))
	for _, d := range devs {
		hl, ok := all[d.ID]
		if !ok {
			hl = model.Health{DeviceID: d.ID, State: model.HealthUnknown}
		}
		hl = shownHealth(d, hl)
		r := deviceRow{Device: d, Health: hl, Vitals: vitalsOf(hl), Power: h.powerCellOf(httpx.BasePath(ctx), d.ID, hl)}
		if minimal != nil {
			r.Minimal = minimal.Minimal(d.ID)
		}
		if f, ok := failing[d.ID]; ok {
			r.ViaMQTT = transport.Choose(f).Commands == transport.MQTT
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Device.Name) < strings.ToLower(out[j].Device.Name)
	})
	return out, nil
}

// shownHealth is the health the device list, its counts and the dashboard show (design v2,
// P2): a device that never answered stays "Unknown" after its polls fail. Its identity is read
// on the first contact and kept, so a device without one has never answered; "Offline" means
// it answered once and stopped. The health tracker itself is unchanged (alerts, bulk skips).
func shownHealth(d model.Device, hl model.Health) model.Health {
	if hl.State == model.HealthOffline && hl.LastSeen.IsZero() && d.Identity.Version == "" {
		hl.State = model.HealthUnknown
	}
	if hl.State == model.HealthOffline {
		hl.LastSeen = lastContact(d, hl)
	}
	return hl
}

func vitalsOf(h model.Health) vitals {
	v := vitals{RSSI: h.RSSI, Restart: restartAt(h), Seen: h.LastSeen,
		Down: h.State == model.HealthOffline || h.State == model.HealthAuthFailed}
	if h.UptimeSec > 0 {
		v.Uptime = humanDuration(time.Duration(h.UptimeSec) * time.Second)
	}
	if h.RSSI > 0 {
		v.DBm = h.RSSI/2 - 100 // Tasmota maps -100..-50 dBm to 0..100 %
		v.Bars = min(4, (h.RSSI+24)/25)
		pct := max(6, min(100, int(math.Round(float64(v.DBm+92)*1.9))))
		v.WifiW = int(math.Round(float64(pct) * 34 / 100))
		v.WifiOK = v.DBm > -70
	}
	return v
}

func filterOf(q url.Values) deviceFilter {
	return deviceFilter{
		Q: strings.TrimSpace(q.Get("q")), Health: q.Get("health"), Family: q.Get("family"), Tag: q.Get("tag"), Chip: q.Get("chip"),
	}
}

func (f deviceFilter) match(r deviceRow) bool {
	if f.Q != "" {
		needle := strings.ToLower(f.Q)
		hay := strings.ToLower(r.Device.Name + " " + r.Device.Address + " " + r.Device.MAC + " " + r.Device.Topic)
		if !strings.Contains(hay, needle) {
			return false
		}
	}
	if f.Health != "" && string(r.Health.State) != f.Health {
		return false
	}
	if f.Family != "" && string(r.Device.Identity.Family) != f.Family {
		return false
	}
	if !chipMatch(f.Chip, r) {
		return false
	}
	return f.Tag == "" || slices.Contains(r.Device.Tags, f.Tag)
}

func (f deviceFilter) values() url.Values {
	v := url.Values{}
	for k, s := range map[string]string{"q": f.Q, "health": f.Health, "family": f.Family, "tag": f.Tag, "chip": f.Chip} {
		if s != "" {
			v.Set(k, s)
		}
	}
	return v
}

func (h *handler) devicesPageData(r *http.Request, v view) (devicesPage, error) {
	base := v.Base
	rows, err := h.rows(r.Context())
	if err != nil {
		return devicesPage{}, err
	}
	f := filterOf(r.URL.Query())
	facts := h.fleetFacts(r.Context(), actor(r))
	facts.annotate(rows)
	fams, tags := map[string]int{}, map[string]int{}
	var shown []deviceRow
	for _, row := range rows {
		if row.Device.Identity.Family != model.ChipUnknown {
			fams[string(row.Device.Identity.Family)]++
		}
		for _, t := range row.Device.Tags {
			tags[t]++
		}
		if f.match(row) {
			shown = append(shown, row)
		}
	}
	ts := h.sortOf(r, "devices", tableSort{Key: "name", Dir: "asc"})
	sortDevices(shown, ts)
	p := devicesPage{
		Filter: f, Query: ts.values(f.values()).Encode(), Rows: shown, Sort: ts,
		RowsData: rowsData{Base: base, Select: h.d.DeviceCmd != nil, Rows: shown,
			Admin: v.IsAdmin, Console: h.d.DeviceCmd != nil},
	}
	p.RowsData.None = len(rows) == 0
	p.Summary = h.fleetSummaryFor(r.Context(), v, rows, facts)
	if v.IsAdmin {
		p.RowsData.AddHref = base + "/devices/new"
		if v.Features.Discovery {
			p.RowsData.AddHref = base + "/discovery"
		}
	}
	p.SortBase = base + "/devices"
	if q := f.values().Encode(); q != "" {
		p.SortBase += "?" + q
	}
	p.States = append(p.States, healthStates...)
	p.Families = facets(fams, func(v string) string { return chipLabel(model.ChipFamily(v)) })
	p.Tags = facets(tags, func(v string) string { return v })
	if err := h.deviceList(r.Context(), &p); err != nil {
		return devicesPage{}, err
	}
	return p, nil
}

// facets turns value counts into select options, sorted by value.
func facets(n map[string]int, label func(string) string) []facet {
	out := make([]facet, 0, len(n))
	for k, c := range n {
		out = append(out, facet{Value: k, Label: label(k), N: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

func (h *handler) devices(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Devices", "devices", nil)
	p, err := h.devicesPageData(r, v)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Data, v.Live = p, true
	h.show(w, r, http.StatusOK, "devices", v, devicesContent(v, p))
}

// deviceTable is the HTMX partial that reloads the table (after inventory
// changes or an SSE resync).
func (h *handler) deviceTable(w http.ResponseWriter, r *http.Request) {
	v := h.view(r, "Devices", "devices", nil)
	p, err := h.devicesPageData(r, v)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Data = p
	h.component(w, r, http.StatusOK, devicesTableBody(v, p))
}

type backupData struct {
	Base        string
	ID          model.DeviceID
	Backups     []model.Backup
	CanBackup   bool
	CanDownload bool
	CanRestore  bool // admin and the restore service exists
	CanDelete   bool // admin and the backup service can delete
	CSRF        string
	IsAdmin     bool
	Notice      string
	Error       string
	Blocked     string // why "Back up now" cannot be used now (updating, never answered); empty = usable
}

// IsNewest reports whether a backup taken at t is the newest run of the
// device: deleting it gets the extra warning.
func (b backupData) IsNewest(t time.Time) bool { return t.Equal(backup.NewestRun(b.Backups)) }

// DeleteText is the text of the confirmation dialog of the delete button. The
// newest backup of the device gets an extra callout in the dialog (IsNewest).
func (b backupData) DeleteText(x model.Backup) string {
	return "Delete the backup of " + ui.StampText(x.TakenAt, ui.StampDateTimeSec) + "? Its dump, snapshot and decoded copy are removed. This cannot be undone."
}

// deviceTabs is the device header and its tabs (devicehead.templ): the pages of one device.
type deviceTabs struct {
	Base     string
	ID       model.DeviceID
	Active   string // overview, settings, console or backups
	Settings bool   // the Settings tab exists (admin, feature on)
	Console  bool   // the Console tab exists (feature on)
	Reason   string // why nothing that needs the device can be used now (running, never answered, offline); empty = usable
	// The reason per action, "" = usable. Without a transport service they all equal Reason; with
	// one, the path TasmoFleet would take decides (deviceconn.go).
	SettingsWhy, ConsoleWhy, BackupWhy, RestoreWhy, UpdateWhy string

	// The device header (devicehead.templ) is the same on every tab.
	Name       string
	Health     model.HealthState
	Updating   bool // an operation holds the device: it is expected to be silent
	CanReplace bool // the Replace device tab
	Drift      bool // the Drift tab (live snapshots can be read)

	CSRF         string
	IsAdmin      bool
	Address      string    // host[:port], as the device list shows it
	Chip         string    // chip family, "" when unknown
	Version      string    // firmware version, "" when unknown
	UpdateTo     string    // a newer stable release from the cached feed; "" when current or unknown
	CanRestart   bool      // admin and the command service
	OfflineSince time.Time // the offline banner: when the device went offline; zero when unknown
	// OfflineSinceText is its fallback text (StampDay) for a page without JavaScript
	OfflineSinceText string
}

// tabsFor builds the device tabs for the viewer. Settings and Console are
// disabled, with the reason as a title, while the device is updating or has
// never answered: nothing they do could reach it.
func tabsFor(v view, d model.Device, hl model.Health, running *model.Job, active string) deviceTabs {
	t := deviceTabs{Base: v.Base, ID: d.ID, Active: active,
		Settings: v.IsAdmin && v.Features.Settings, Console: v.Features.DeviceCmd,
		Name: d.Name, Health: hl.State, Updating: running != nil,
		CSRF: v.CSRF, IsAdmin: v.IsAdmin, Address: d.Address, Chip: chipText(d.Identity.Family), Version: d.Identity.Version,
		CanRestart: v.IsAdmin && v.Features.DeviceCmd}
	if t.Health == "" {
		t.Health = model.HealthUnknown
	}
	if d.Port != 0 && d.Port != 80 {
		t.Address += ":" + strconv.Itoa(d.Port)
	}
	t.Reason = blockReason(d, hl, running)
	t.applyPaths(transport.Paths{}, false)
	return t
}

// blockReason says why nothing that needs the device (Settings, Console, Back up
// now) can be used right now, or "" when the device can be used. While an
// operation holds the device it is silent on purpose; a device that never
// answered has nothing to talk to.
func blockReason(d model.Device, hl model.Health, running *model.Job) string {
	switch {
	case running != nil:
		return "Unavailable while " + runningPhrase(*running) + "."
	case hl.LastSeen.IsZero() && d.Identity.DetectedAt.IsZero():
		return "The device has not answered yet."
	case hl.State == model.HealthOffline:
		return reasonOffline
	}
	return ""
}

// runningPhrase names the operation that holds the device, for the reason sentence.
func runningPhrase(j model.Job) string {
	switch {
	case j.IsCommand():
		return "a command is being sent to the device"
	case j.IsBackup():
		return "a backup of the device is running"
	case j.IsPolicy():
		return "the device is being made compliant with the policy"
	}
	return "the device is being updated"
}

// deviceState reads what blockReason needs: the health and whether an operation
// is running on the device. A failed read only costs the reason; the page renders.
func (h *handler) deviceState(r *http.Request, d model.Device) (model.Health, *model.Job) {
	hl := h.d.Health.Get(d.ID)
	var running *model.Job
	if h.d.Jobs != nil {
		jobs, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{DeviceID: d.ID, Limit: 20})
		if err != nil {
			h.log.WarnContext(r.Context(), "device state: jobs unavailable", "err", err)
		}
		for i := range jobs {
			if running == nil && !jobs[i].State.Terminal() {
				running = &jobs[i]
			}
		}
	}
	return hl, running
}

// tabsOf builds the device header and tabs of a sub page (settings, console, drift, backups):
// it reads the health and the running jobs the way the overview does. A read that fails
// only costs the reason; the tabs still render.
func (h *handler) tabsOf(r *http.Request, v view, d model.Device, active string) deviceTabs {
	hl, running := h.deviceState(r, d)
	return h.headOf(r.Context(), v, d, hl, running, active)
}

// canReplace says whether the viewer gets the Replace device link.
func (h *handler) canReplace(v view) bool {
	_, ok := h.d.Restore.(restore.Replacer)
	return ok && v.IsAdmin
}

func (h *handler) deviceID(w http.ResponseWriter, r *http.Request) (model.DeviceID, bool) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFoundDevice(w, r)
		return 0, false
	}
	return model.DeviceID(n), true
}

// device loads the device or answers 404.
func (h *handler) device(w http.ResponseWriter, r *http.Request) (model.Device, bool) {
	id, ok := h.deviceID(w, r)
	if !ok {
		return model.Device{}, false
	}
	d, err := h.d.Inventory.Get(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.notFoundDevice(w, r)
		return d, false
	case err != nil:
		h.fail(w, r, err)
		return d, false
	}
	return d, true
}

// mqttText describes in one line what the device does with MQTT and whether
// tasmofleet watches it. brokerConfigured says whether tasmofleet has a broker
// at all; same compares the device's broker with ours. Only host and port are
// shown, never the device's MQTT user or password.
func mqttText(d model.Device, brokerConfigured bool, same func(host string, port int) bool) string {
	if d.MQTT == model.MQTTNone {
		return "MQTT disabled"
	}
	if d.MQTT == model.MQTTUnknown {
		return "not probed yet"
	}
	topic := ""
	if d.Topic != "" {
		topic = ", topic " + d.Topic
	}
	if d.MQTTHost == "" { // probed before the broker address was recorded
		if d.MQTT == model.MQTTOurs {
			return "reports to our broker" + topic
		}
		return "MQTT configured (broker address known after the next probe)" + topic
	}
	port := d.MQTTPort
	if port == 0 {
		port = 1883
	}
	target := net.JoinHostPort(d.MQTTHost, strconv.Itoa(port))
	switch {
	case !brokerConfigured:
		return "reports to " + target + topic + " (not monitored: TasmoFleet has no MQTT broker configured)"
	case d.MQTT == model.MQTTOurs || (same != nil && same(d.MQTTHost, d.MQTTPort)):
		return "reports to our broker (" + target + ")" + topic
	default:
		return "other broker " + target + topic
	}
}

// refreshProblem is the sentence under a failed "Refresh identity".
func (h *handler) refreshProblem(r *http.Request, err error) string {
	// Another device answered at the stored address (#65): the prober says what and why.
	var refused interface{ Refusal() string }
	if errors.As(err, &refused) {
		return "Identity not refreshed. " + refused.Refusal()
	}
	if msg, ok := deviceProblem(err); ok {
		return "Identity not refreshed. " + msg
	}
	h.log.WarnContext(r.Context(), "identity probe failed", "err", err)
	return "Identity not refreshed. The device sent an answer that did not fit. The server log has the details."
}

func (h *handler) deviceBackup(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	switch r.PostFormValue("from") {
	case "list": // the row menu of the device list: a one-line result, not the card
		h.deviceBackupFromList(w, r, d)
		return
	case "tab": // the empty Backups tab: the page reloads with the new backup, or the reason shows under the button
		h.deviceBackupFromTab(w, r, d)
		return
	}
	// The header's "Back up" (a plain form on every tab): back to the tab it was pressed on with
	// the result as the flash; a refusal is a message page that names the reason.
	back := devPath(d.ID, headBack(r.PostFormValue("back")))
	refuse := func(status int, msg string) {
		h.message(w, r, status, h.view(r, "Backup not taken", "devices", messageData{Text: msg, LinkText: "Back to " + d.Name, Link: back}))
	}
	if wait := h.backupWait(d.ID); wait != "" {
		refuse(http.StatusTooManyRequests, wait+".")
		return
	}
	if _, err := h.d.Backups.Backup(r.Context(), actor(r), d.ID); err != nil {
		refuse(http.StatusOK, h.backupProblem(err))
		return
	}
	redirect(w, r, back+"?ok=backup-taken")
}

// headBack is the tab a header button returns to: one of the device's tabs, never a path the
// form made up.
func headBack(tab string) string {
	switch tab {
	case "/settings", "/console", "/backups", "/drift", "/replace":
		return tab
	}
	return ""
}

// backupWait reserves a manual backup slot for the device and returns the refusal sentence
// ("" when the slot is granted) when the last one is too recent. The cooldown keeps repeated
// clicks from evicting restore points, so it counts from the last backup run even when that
// backup was deleted since; the sentence names that time so the refusal explains itself (#208).
// Only deleting the device's last backup ends it (backupDeleted): nothing is left to evict.
func (h *handler) backupWait(id model.DeviceID) string {
	h.backupMu.Lock()
	defer h.backupMu.Unlock()
	now := h.d.Clock.Now()
	if last, ok := h.lastBackup[id]; ok && now.Sub(last) < h.d.BackupCooldown {
		return cooldownText(now.Sub(last), h.d.BackupCooldown-now.Sub(last))
	}
	h.lastBackup[id] = now
	return ""
}

// backupDeleted ends the cooldown of a device whose last backup was just deleted, so the user
// is not locked out of taking a fresh one (#208).
func (h *handler) backupDeleted(d backup.Deletion) {
	if d.Remaining > 0 {
		return
	}
	h.backupMu.Lock()
	delete(h.lastBackup, d.Device)
	h.backupMu.Unlock()
}

// cooldownText is "Last backup 4 min 40 s ago; a new one is possible in 20 s".
func cooldownText(ago, wait time.Duration) string {
	return fmt.Sprintf("Last backup %s ago; a new one is possible in %s", minSec(ago), minSec(wait.Round(time.Second)))
}

// minSec renders a short span as "4 min 40 s", "5 min", "20 s".
func minSec(d time.Duration) string {
	sec := int(d.Round(time.Second) / time.Second)
	switch m, s := sec/60, sec%60; {
	case m == 0:
		return fmt.Sprintf("%d s", s)
	case s == 0:
		return fmt.Sprintf("%d min", m)
	default:
		return fmt.Sprintf("%d min %d s", m, s)
	}
}

// deviceBackupFromTab answers the "Back up now" of the empty Backups tab: on
// success the page reloads (the tab lists the new backup), otherwise the sentence
// goes into the status line under the button.
func (h *handler) deviceBackupFromTab(w http.ResponseWriter, r *http.Request, d model.Device) {
	data := notice{}
	status := http.StatusOK
	if wait := h.backupWait(d.ID); wait != "" {
		data.Error = wait + "."
		status = http.StatusTooManyRequests
	} else if _, err := h.d.Backups.Backup(r.Context(), actor(r), d.ID); err != nil {
		data.Error = h.backupProblem(err)
	} else {
		redirect(w, r, fmt.Sprintf("/devices/%d/backups?ok=backed-up", d.ID))
		return
	}
	h.component(w, r, status, listNotice(data))
}

// deviceBackupFromList answers the "Back up now" of the device list with a toast (design v2:
// "Backup of Attic Dimmer saved"; a failure stays until dismissed). The toast region is a live
// region, so screen readers hear it; the swap target above the table is emptied.
func (h *handler) deviceBackupFromList(w http.ResponseWriter, r *http.Request, d model.Device) {
	data := struct{ Notice, Error string }{}
	status := http.StatusOK
	if wait := h.backupWait(d.ID); wait != "" {
		data.Error = d.Name + ": " + strings.ToLower(wait[:1]) + wait[1:] + "."
		status = http.StatusTooManyRequests
	} else if _, err := h.d.Backups.Backup(r.Context(), actor(r), d.ID); err != nil {
		msg := h.backupProblem(err)
		data.Error = "Backup of " + d.Name + " failed: " + strings.ToLower(msg[:1]) + msg[1:]
	} else {
		data.Notice = "Backup of " + d.Name + " saved"
	}
	text, tone := data.Notice, "ok"
	if data.Error != "" {
		text, tone = data.Error, "danger"
	}
	setHXTrigger(w, map[string]any{"toast": map[string]string{"text": text, "tone": tone}})
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
}
