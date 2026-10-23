// Package web is the server-rendered UI: html/template pages enhanced with
// vendored HTMX (static/htmx-*.js, no JS build) and an SSE stream
// (/events) for live health and OTA progress. All links are built from
// httpx.BasePath so the UI works under a prefix and behind Home Assistant
// ingress. Templates and static files are embedded.
//
// Handlers depend on service interfaces only (inventory.Service,
// health.Tracker, backup.Service, ...), so each page can be tested with
// fakes via httptest. Roles: every page needs at least the operator role;
// inventory edits, the audit log and backup downloads need admin.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

//go:embed static
var Assets embed.FS

// OTA is the subset of *ota.Engine the UI uses.
type OTA interface {
	Preview(ctx context.Context, reqs []ota.Request) ([]ota.PreviewItem, error)
	Submit(ctx context.Context, actor string, reqs []ota.Request) (model.BatchID, error)
	Cancel(ctx context.Context, actor string, id model.JobID) error
	CancelBatch(ctx context.Context, actor string, id model.BatchID) error
	// SubmitRetry creates a new batch (transaction) for the failed or
	// cancelled devices of a batch (devices empty = all of them), linked to it
	// (store.Batch.RetryOf), re-planned from the devices' current state. It
	// returns ota.ErrBatchRunning while the batch still has running jobs,
	// ota.ErrNothingToRetry when nothing is left, *ota.RefusedError with
	// per-device reasons, store.ErrNotFound for an unknown batch; devices that
	// need nothing now are in RetryResult.Skipped.
	SubmitRetry(ctx context.Context, actor string, batch model.BatchID, devices []model.DeviceID) (ota.RetryResult, error)
}

// Prober talks to a device to learn its identity. It lives in package app
// (it needs the device client and fwselect); the UI only asks.
type Prober interface {
	// Describe probes d (address, port, credentials) and returns it with MAC,
	// topic, default name, MQTT capability and identity filled in. Nothing is stored.
	Describe(ctx context.Context, d model.Device) (model.Device, error)
	// Refresh probes the stored device and stores the new identity.
	Refresh(ctx context.Context, id model.DeviceID) error
}

// HealthHistory lists stored health transitions (store.HealthStore).
type HealthHistory interface {
	ListHealthEvents(ctx context.Context, id model.DeviceID, limit int) ([]store.HealthEvent, error)
}

// AuditLister reads the audit log (store.AuditStore).
type AuditLister interface {
	ListAudit(ctx context.Context, f store.AuditFilter) ([]model.AuditEvent, error)
}

// JobLister reads OTA jobs (store.JobStore). Optional.
type JobLister interface {
	ListJobs(ctx context.Context, f store.JobFilter) ([]model.Job, error)
	JobEvents(ctx context.Context, id model.JobID) ([]model.JobEvent, error)
	GetJob(ctx context.Context, id model.JobID) (model.Job, error)
}

// ArtifactLister lists the cached firmware files (store.ArtifactStore).
type ArtifactLister interface {
	ListArtifacts(ctx context.Context) ([]model.Artifact, error)
}

// Guard protects routes; *auth.Service implements both methods.
type Guard interface {
	Require(min model.Role) func(http.Handler) http.Handler
	CSRFToken(r *http.Request) string
}

// KV is one row of the About page.
type KV struct{ Key, Value string }

// Deps bundles the services the UI needs. Filled by package app.
type Deps struct {
	Guard        Guard
	Inventory    inventory.Service
	Health       health.Tracker
	Backups      backup.Service
	Prober       Prober
	History      HealthHistory
	Audit        AuditLister
	Recorder     audit.Recorder
	Jobs         JobLister        // optional
	Batches      store.BatchStore // optional: batch info and retries of a batch
	OTA          OTA              // optional: nil without FIRMWARE_BASE_URL
	DeviceCmd    DeviceCmd        // optional: power toggle and console
	Firmware     firmware.Cache
	Import       ImportService // optional: TasmoAdmin web import
	Discovery    Discovery     // optional
	Artifacts    ArtifactLister
	Hub          *events.Hub
	Clock        clock.Clock
	Pages        *Pages             // optional: the account pages of package auth join the app shell
	Settings     SettingsService    // optional (M2-2): per-device settings editor
	Policy       PolicyService      // optional (M2-2): nil hides the policy view
	PolicyFix    PolicyFix          // optional: "Make compliant" on the policy page
	CommandBatch PolicyFix          // optional: "Send command" to several devices (admin)
	BackupBatch  BackupBatch        // optional: "Back up" several devices (admin)
	Restore      RestoreService     // optional: nil without FIRMWARE_BASE_URL (M2-5)
	SysConfig    SysConfigService   // optional: system settings page
	PolicyRules  PolicyRulesService // optional: policy rule editor
	// PolicyLivePause is optional: Pause / Resume of the automatic policy live check.
	PolicyLivePause PolicyLivePause
	Setup           SetupService         // optional: first-run setup
	Prefs           store.PrefsStore     // optional: per-user table columns and page size
	Principals      store.PrincipalStore // optional: display names of signed-in users
	// DefaultDevicePassword returns the fleet default device password (N20,
	// settings page); nil or "" means none. Adding a device and accepting a
	// discovery proposal use it when the form asks for it (use_default_password=1).
	DefaultDevicePassword func() string
	Database              DBInfoer // optional: About's database row (N13)
	// StaleAfter is the configured silence after which a device is stale (tooltip text only); nil = the default.
	StaleAfter func() time.Duration

	// FirmwareSource names where releases come from ("GitHub releases (arendst/Tasmota)"),
	// FirmwareFeed its short name ("GitHub"); the firmware page's subtitle and status line.
	FirmwareSource, FirmwareFeed string

	// NoteLinkHosts lists the hosts links in firmware release notes may point
	// to ("*.example.org" for subdomains); nil = firmware.DefaultNoteLinkHosts.
	NoteLinkHosts []string

	// SSERecheck is how often an open event stream re-validates its session
	// (default 20 s, also the heartbeat). MaxStreamsPerUser caps concurrent
	// streams per principal (default 8); MaxStreams caps all (default 200).
	SSERecheck        time.Duration
	MaxStreamsPerUser int
	MaxStreams        int

	// BackupCooldown is the minimum time between two manual backups of one
	// device (default 5 minutes), so repeated clicks cannot evict restore points.
	BackupCooldown time.Duration

	Version        string
	DecoderVersion string
	MQTT           func() string // "connected", "disconnected" or "disabled"
	// MQTTBroker compares a device's configured broker with ours; nil when
	// tasmofleet has no broker.
	MQTTBroker interface {
		Same(host string, port int) bool
	}
	Config []KV // effective configuration, secrets already masked
}

type handler struct {
	sseMu      sync.Mutex
	sseUsers   map[string]int
	fw         firmwareState // firmware page: prefetcher
	sseTotal   int
	backupMu   sync.Mutex
	plans      importPlans
	reports    importReports
	cmds       pendingCmds
	lastBackup map[model.DeviceID]time.Time
	d          Deps
	started    time.Time // About: uptime
	pr         *principalDir
	log        *slog.Logger
}

// Handler returns the UI router (without the auth routes and the global
// middleware, which package app adds).
func Handler(d Deps) (http.Handler, error) {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.MQTT == nil {
		d.MQTT = func() string { return "disabled" }
	}
	pr := newPrincipalDir(d.Principals)
	if d.SSERecheck == 0 {
		d.SSERecheck = 20 * time.Second
	}
	if d.MaxStreamsPerUser == 0 {
		d.MaxStreamsPerUser = 8
	}
	if d.MaxStreams == 0 {
		d.MaxStreams = 200
	}
	if d.BackupCooldown == 0 {
		d.BackupCooldown = 5 * time.Minute
	}
	h := &handler{d: d, pr: pr, started: d.Clock.Now(), log: slog.Default(), lastBackup: map[model.DeviceID]time.Time{}, sseUsers: map[string]int{}}
	if d.Pages != nil {
		d.Pages.h = h
	}
	static, err := fs.Sub(Assets, "static")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	mux.HandleFunc("GET /favicon.ico", serveFavicon)
	if d.Guard == nil {
		return mux, nil
	}
	op := d.Guard.Require(model.RoleOperator)
	adm := d.Guard.Require(model.RoleAdmin)
	route := func(pattern string, guard func(http.Handler) http.Handler, f http.HandlerFunc) {
		mux.Handle(pattern, guard(h.recordPrincipal(f)))
	}
	route("GET /{$}", op, h.dashboard)
	route("GET /devices", op, h.devices)
	route("GET /devices/table", op, h.deviceTable)
	route("GET /devices/new", adm, h.deviceNewForm)
	route("POST /devices/new", adm, h.deviceCreate)
	route("GET /devices/{id}", op, h.deviceDetail)
	route("GET /devices/{id}/history", op, h.stateHistory)
	route("GET /devices/{id}/edit", adm, h.deviceEditForm)
	route("POST /devices/{id}", adm, h.deviceUpdate)
	route("POST /devices/{id}/delete", adm, h.deviceDelete)
	h.registerDeviceConfirm(route, adm)
	route("POST /devices/{id}/probe", op, h.deviceProbe)
	route("POST /devices/{id}/backup", op, h.deviceBackup)
	route("GET /backups/{id}", adm, h.backupDownload)
	route("GET /audit", adm, h.auditLog)
	h.registerBackupDiff(route, op)
	h.registerBackupAdmin(route, adm)
	h.registerBackupKeep(route, op, adm)
	if d.Restore != nil {
		h.registerRestore(route, adm)
		h.registerReplace(route, adm)
	}
	if d.OTA != nil {
		route("GET /transactions", op, h.transactionsPage)
		route("GET /updates/new", op, h.updateForm)
		route("POST /updates/new", op, h.updateForm) // the device list's bulk bar: the selection as a form post
		route("POST /updates/preview", op, h.updatePreview)
		route("POST /updates", op, h.updateSubmit)
		route("GET /transactions/{batch}", op, h.batchPage)
		route("GET /transactions/{batch}/live", op, h.batchLive)
		route("GET /transactions/{batch}/cancel", op, h.batchCancelConfirm)
		route("POST /transactions/{batch}/cancel", op, h.batchCancel)
		route("GET /transactions/{batch}/retry", op, h.batchRetryForm)
		route("POST /transactions/{batch}/retry", op, h.batchRetry)
		route("GET /jobs/{id}/cancel", op, h.jobCancelConfirm)
		route("POST /jobs/{id}/cancel", op, h.jobCancel)
	}
	if d.Firmware != nil {
		route("GET /firmware", op, h.firmwarePage)
		route("GET /firmware/releases/{version}", op, h.firmwareRelease)
		route("POST /firmware/refresh", adm, h.firmwareRefresh)
		route("POST /firmware/fetch", adm, h.firmwareFetch)
		h.registerFirmwareManage(route, adm)
		h.registerFirmwareCustom(route, adm)
	}
	if d.Prefs != nil {
		route("POST /prefs/{key}", op, h.savePref)
	}
	h.registerSettings(route, adm)
	h.registerSysConfig(route, adm)
	h.registerSetup(route, adm)
	h.registerPolicy(route, op)
	h.registerPolicyLivePause(route, adm)
	h.registerPolicyFix(route, adm)
	h.registerBulkCommand(route, adm)
	h.registerBulkAction(route, adm)
	h.registerPolicyRules(route, op, adm)
	if d.Import != nil {
		h.registerImport(route, adm)
	}
	h.registerDiscovery(route, op, adm)
	route("GET /about", adm, h.about)
	route("GET /whats-new", op, h.whatsNew) // every signed-in user (N5)
	route("GET /nav", op, h.navPage)
	route("GET /styleguide", op, h.styleguide) // sample data only: every signed-in user (D17)
	route("GET /events", op, h.events)
	h.registerDeviceCmd(route, op)
	h.registerUnavailable(route, op)
	// Every other path answers with the app's own not-found page, not Go's plain text.
	route("/", op, h.notFound)
	return mux, nil
}

// recordPrincipal keeps the display name of the signed-in user current (the
// guard has already authenticated the request).
func (h *handler) recordPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.FromContext(r.Context()); ok {
			h.pr.Record(r.Context(), p)
		}
		next.ServeHTTP(w, r)
	})
}

// view is the data every full page receives.
type view struct {
	Title    string
	Nav      string
	Base     string
	CSRF     string
	User     model.Principal
	IsAdmin  bool
	RoleName string
	Flash    string
	Live     bool   // open the SSE stream (pages with live data; the top bar then shows its state)
	Path     string // request path below the base path (crumbs)
	Theme    string // the user's theme choice, "" = Auto
	// Crumbs is a ported page's own breadcrumb trail; nil derives it from the navigation
	Crumbs []ui.Crumb
	// NewDevices counts discovered devices waiting to be added (admins with discovery)
	NewDevices int
	News       int // releases new since the user last opened What's new (N5, avatar dot)
	Features   struct{ Updates, Firmware, Policy, Settings, Import, Discovery, DeviceCmd, SysConfig, BulkCommand, BulkBackup, BulkRestart bool }
	Banner     string // a standing notice for admins, e.g. no device networks yet
	Static     bool   // style guide only: partials render their open variant (data-static)
	// SortPath is the page URL the sort links of its tables return to when the
	// request is not a GET of that page (the result of a POST).
	SortPath string
	Data     any
}

var flashes = map[string]string{
	"saved":     "Saved.",
	"imported":  "Imported.",
	"reverted":  "Reverted: a new version with the old rules was saved.",
	"added":     "Device added.",
	"deleted":   "Device deleted.",
	"restarted": "Restart sent. The device is back in a few seconds.",
	// the header's Back up; the Backups tab's own button says "Backup taken." in its list (ok=backed-up)
	"backup-taken": "Backup taken.",
}

func (h *handler) view(r *http.Request, title, nav string, data any) view {
	p, _ := auth.FromContext(r.Context())
	v := view{
		Title: title, Nav: nav, Base: httpx.BasePath(r.Context()), CSRF: h.d.Guard.CSRFToken(r),
		User: p, IsAdmin: p.Role >= model.RoleAdmin, RoleName: roleName(p.Role),
		Flash: flashes[r.URL.Query().Get("ok")], Data: data, Path: r.URL.Path,
	}
	v.Features.Updates = h.d.OTA != nil
	v.Features.Firmware = h.d.Firmware != nil
	v.Features.Policy = h.d.Policy != nil
	v.Features.Settings = h.d.Settings != nil
	v.Features.Import = h.d.Import != nil
	v.Features.Discovery = h.d.Discovery != nil
	v.Features.DeviceCmd = h.d.DeviceCmd != nil
	v.Features.SysConfig = h.d.SysConfig != nil
	v.Features.BulkCommand = h.d.CommandBatch != nil && h.d.DeviceCmd != nil
	v.Features.BulkRestart = v.Features.BulkCommand
	v.Features.BulkBackup = h.d.BackupBatch != nil
	if h.d.Setup != nil && v.IsAdmin && !h.d.Setup.NetworksConfigured() {
		v.Banner = "No device networks are configured: no device is contacted yet."
	}
	return v
}

// newDeviceCount is the number of discovered devices waiting for an admin: new devices to add
// and known devices at a new address to confirm (design v2: the badge on "Add devices" in the
// sub-navigation and the drawer, and on the Discovered tab). A failed read shows no badge.
func (h *handler) newDeviceCount(r *http.Request) int {
	props, err := h.d.Discovery.Proposals(r.Context())
	if err != nil {
		h.log.WarnContext(r.Context(), "count discovered devices", "err", err)
		return 0
	}
	n := 0
	for _, p := range props {
		if p.Kind == discovery.KindNew || p.Kind == discovery.KindAddressChange {
			n++ // both wait for an admin on the Discovered tab
		}
	}
	return n
}

func roleName(r model.Role) string {
	switch r {
	case model.RoleAdmin:
		return "admin"
	case model.RoleOperator:
		return "operator"
	case model.RoleNone:
		return "none"
	}
	return "none"
}

// actor is the audit identity of the signed-in user.
func actor(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return p.Subject
}

// message writes a notice page (noticePage); v.Data is a messageData or the sentence itself.
func (h *handler) message(w http.ResponseWriter, r *http.Request, status int, v view) {
	m, ok := v.Data.(messageData)
	if !ok {
		m = messageData{Text: fmt.Sprint(v.Data)}
	}
	h.show(w, r, status, "message", v, noticePage(v.Base, v.Title, m))
}

// show writes a full page: content in the shell, the user's table layouts applied. page names
// the page for the tables' sort links after a POST (postSortPaths).
func (h *handler) show(w http.ResponseWriter, r *http.Request, status int, page string, v view, content templ.Component) {
	prefs := h.userPrefs(r)
	b, err := h.page(r, v, prefs, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	path := v.SortPath
	if path == "" {
		path = sortPath(r)
	}
	if path == "" {
		// The answer to a POST (a refresh, a failed save) is the same page: its tables keep
		// their sortable headers and layout, linking to the page's GET address.
		if p := postSortPaths[page]; p != "" {
			path = httpBase(r) + p
		}
	}
	writePage(w, status, applyTablePrefsSorted(b, prefs, pageSorts(r, prefs, path)))
}

// postSortPaths maps a page template to the GET path of the page, for pages whose tables
// are sortable and that handlers also render as the answer to a POST.
var postSortPaths = map[string]string{
	"firmware": "/firmware",
}

// component writes a templ fragment (an htmx swap) with the user's table layouts applied.
func (h *handler) component(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	prefs := h.userPrefs(r)
	_, _ = w.Write(applyTablePrefsSorted(buf.Bytes(), prefs, pageSorts(r, prefs, sortPath(r)))) // a failed write means the client went away
}

// fail logs err and answers 500 without leaking its text.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.log.ErrorContext(r.Context(), "web handler failed", "path", r.URL.Path, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// notFound answers 404 for an unknown path or an item the caller has no more
// specific words for. Callers that failed to find a device or batch use
// notFoundDevice / notFoundBatch; the route prefix says nothing about which
// object is missing (a device can exist while one of its backups does not).
func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderNotFound(w, r, "Not found", "This page doesn't exist",
		"The link may be from an older version of TasmoFleet, or the device it pointed to was deleted.",
		ui.Btn{Label: "Go to dashboard", Kind: "primary", Href: "/"}, ui.Btn{Label: "All devices", Href: "/devices"})
}

// notFoundDevice answers 404 for a device that does not exist and links to the list.
func (h *handler) notFoundDevice(w http.ResponseWriter, r *http.Request) {
	h.renderNotFound(w, r, "Device not found", "This device doesn't exist",
		"It may have been deleted, or the link is mistyped.",
		ui.Btn{Label: "All devices", Kind: "primary", Href: "/devices"}, ui.Btn{Label: "Go to dashboard", Href: "/"})
}

// notFoundBatch answers 404 for a transaction that does not exist and links to the list.
func (h *handler) notFoundBatch(w http.ResponseWriter, r *http.Request) {
	h.renderNotFound(w, r, "Transaction not found", "This transaction doesn't exist",
		"The link may be mistyped.",
		ui.Btn{Label: "All transactions", Kind: "primary", Href: "/transactions"}, ui.Btn{Label: "Go to dashboard", Href: "/"})
}

// renderNotFound is the 404 error page (design v2 screen e404); the buttons' paths are below
// the base path.
func (h *handler) renderNotFound(w http.ResponseWriter, r *http.Request, title, heading, body string, primary, secondary ui.Btn) {
	base := httpx.BasePath(r.Context())
	primary.Href, secondary.Href = base+primary.Href, base+secondary.Href
	h.errorPageIn(w, r, http.StatusNotFound, title, navOfPath(r.URL.Path), errView{Code: "404 · Not found", Title: heading, Body: body, Path: base + r.URL.Path, Primary: primary, Secondary: secondary})
}

// messageData is a message page with one extra way on besides the dashboard.
type messageData struct {
	Text, LinkText, Link string
	// LinkLead: the link opens the sentence ("Transaction #7 was started ...") and is the
	// page's primary action; the dashboard link steps back.
	LinkLead bool
	// LinkPrimary: the link is the page's primary action (the way out), the dashboard link steps back.
	LinkPrimary bool
	// Code is a name (a setting) set in <code> after the text.
	Code string
	Left []leftItem // devices left out, one line each
}

// leftItem is a device that took no part, with the reason.
type leftItem struct{ Name, Reason string }

// registerUnavailable answers the pages of a switched-off feature with an
// explanation instead of a bare 404: a bookmark to /firmware or /transactions
// keeps making sense when the feature is off.
func (h *handler) registerUnavailable(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), op func(http.Handler) http.Handler) {
	explain := func(title, text, setting string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			base := httpx.BasePath(r.Context())
			// The box under the text names the setting that switches the feature on.
			e := errView{Code: "Feature switched off", Title: title, Body: text, Path: setting,
				Primary: ui.Btn{Label: "Go to the dashboard", Kind: "primary", Href: base + "/"}, Secondary: ui.Btn{Label: "All devices", Href: base + "/devices"}}
			if h.d.SysConfig != nil && h.view(r, "", "", nil).IsAdmin {
				e.Primary, e.Secondary = ui.Btn{Label: "Open the system settings", Kind: "primary", Href: base + "/settings/system"}, ui.Btn{Label: "Go to the dashboard", Href: base + "/"}
			}
			h.errorPage(w, r, http.StatusNotFound, title, e)
		}
	}
	const noOTA = "Firmware updates are switched off because no firmware base URL is set. Devices fetch firmware from TasmoFleet over plain http, so they need an address to reach it at."
	if h.d.OTA == nil {
		route("GET /transactions", op, explain("Updates are switched off", noOTA, "FIRMWARE_BASE_URL"))
		route("GET /updates/new", op, explain("Updates are switched off", noOTA, "FIRMWARE_BASE_URL"))
	}
	if h.d.Firmware == nil {
		route("GET /firmware", op, explain("Firmware is switched off", noOTA, "FIRMWARE_BASE_URL"))
		route("GET /firmware/releases/{version}", op, explain("Firmware is switched off", noOTA, "FIRMWARE_BASE_URL"))
	}
}

// redirect sends a browser (303) or an HTMX request (HX-Redirect) to path
// below the base path.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	target := httpx.BasePath(r.Context()) + httpx.LocalPath(path)
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // G710: validated base path + LocalPath
}

// limitBody caps form bodies; the UI posts a few short fields.
func limitBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
}

// ago is the compact relative time used everywhere: "now", "45s", "5m", "2h",
// "3d". Callers put the full timestamp in a tooltip.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// agoWords is ago for running text: "5m ago", "now", "never".
func agoWords(now, t time.Time) string {
	s := ago(now, t)
	if s == "now" || s == "never" {
		return s
	}
	return s + " ago"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// longDuration is a retention period in words: "30 days", "1 day", "36 hours".
func longDuration(d time.Duration) string {
	if h := int(d.Hours()); d >= 24*time.Hour && h%24 == 0 {
		return plural(h/24, "day")
	} else if d >= time.Hour && d%time.Hour == 0 {
		return plural(h, "hour")
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return plural(int(d.Minutes()), "minute")
	}
	return humanDuration(d)
}

// plural renders "1 device" / "3 devices" for an int or int64 count.
func plural(n any, noun string) string {
	var c int64
	switch v := n.(type) {
	case int:
		c = int64(v)
	case int64:
		c = v
	}
	if c == 1 {
		return "1 " + noun
	}
	return strconv.FormatInt(c, 10) + " " + noun + "s"
}

// snapshotGaps returns the part list of an incomplete-snapshot backup note
// ("snapshot incomplete: Status 2, Status 5" gives "Status 2, Status 5"),
// empty for any other note.
func snapshotGaps(note string) string {
	if rest, ok := strings.CutPrefix(note, "snapshot incomplete: "); ok {
		return rest
	}
	return ""
}

// planText says how an update is flashed, in words.
func planText(p model.Plan) string {
	switch p {
	case model.PlanDirect:
		return "direct flash"
	case model.PlanStaged:
		return "staged: minimal firmware first"
	}
	return string(p)
}

// healthDetail returns the detail of a state change only when it says more
// than the Source column next to it ("http" + "http poll ok" repeats itself).
func healthDetail(source model.HealthSource, detail string) string {
	d := strings.TrimSpace(detail)
	s := strings.ToLower(string(source))
	switch strings.ToLower(d) {
	case "", s, s + " poll ok", s + " ok":
		return ""
	}
	return d
}

func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KiB", float64(n)/k)
	default:
		return fmt.Sprintf("%.1f MiB", float64(n)/(k*k))
	}
}

// errText turns service errors into short, user-facing sentences. Device
// output and credentials never reach it: services already redact them.
func errText(err error) string {
	switch {
	case errors.Is(err, inventory.ErrOutsideNetwork):
		return "That address is outside the configured device networks."
	case errors.Is(err, inventory.ErrInvalid):
		return err.Error()
	case errors.Is(err, store.ErrConflict):
		return "A device with the same name, MAC or topic already exists."
	default:
		return err.Error()
	}
}

// httpBase returns the base path for templates that receive their own data.
func httpBase(r *http.Request) string { return httpx.BasePath(r.Context()) }

// channelInfo is one contact channel of a device in the Status cell.
type channelInfo struct {
	Name string // "MQTT", "HTTP"
	Mark string // glyph: "✓", "✕", "–" (the prototype's marks)
	Word string // the same for screen readers
	Tone string // ok, bad, off
	Tip  string
}

// channelsOf says, per channel, whether the device was heard from within the
// stale window. It only presents what the health tracker already knows; the
// health state itself is decided there. A channel the device does not use
// shows "–" (HTTP is just not polled while MQTT delivers).
func channelsOf(now time.Time, window time.Duration, h model.Health) []channelInfo {
	fresh := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) <= window }
	tip := func(name string, t time.Time, none string) string {
		if t.IsZero() {
			return name + ": " + none
		}
		return name + ": last contact " + t.Format("2006-01-02 15:04:05 MST") + " (" + ago(now, t) + ")"
	}
	mk := func(name string, ok, bad bool, tip string) channelInfo {
		switch {
		case ok:
			return channelInfo{name, "\u2713", "ok", "ok", tip}
		case bad:
			return channelInfo{name, "\u2715", "failing", "bad", tip}
		}
		return channelInfo{name, "\u2013", "not used", "off", tip}
	}
	mqttFresh := fresh(h.SeenMQTT)
	return []channelInfo{
		mk("MQTT", mqttFresh, h.LWT != "" || !h.SeenMQTT.IsZero(), tip("MQTT", h.SeenMQTT, "no messages from this device")),
		mk("HTTP", fresh(h.SeenHTTP), !h.SeenHTTP.IsZero() && !mqttFresh, tip("HTTP", h.SeenHTTP, "not polled")),
	}
}
