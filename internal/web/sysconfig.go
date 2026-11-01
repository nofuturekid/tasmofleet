package web

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// SysConfigService is the part of sysconfig.Service the UI uses. Optional: the
// system settings page exists only when it is set.
type SysConfigService interface {
	State() []sysconfig.FieldState
	ReadOnly() []sysconfig.Row
	Skipped() map[string]string
	Apply(ctx context.Context, req sysconfig.Request) (sysconfig.Result, error)
	TestMQTT(ctx context.Context, req sysconfig.TestRequest) (string, error)
	TestSSO(ctx context.Context, req sysconfig.TestRequest) (string, error)
	Reset(ctx context.Context, req sysconfig.ResetRequest) (sysconfig.Result, error)
	// Preview is the effective configuration with form values on top, validated, not stored.
	// The second result lists stored secrets left out because the form names another target.
	Preview(values map[string]string) (config.Config, []string, error)
}

const sysPath = "/settings/system"

func (h *handler) registerSysConfig(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.SysConfig == nil {
		return
	}
	route("GET "+sysPath, adm, h.sysPage)
	route("POST "+sysPath, adm, h.sysSave)
	route("POST "+sysPath+"/stepup", adm, h.sysStepUpPost)
	route("POST "+sysPath+"/mqtt-test", adm, h.sysMQTTTest)
	route("POST "+sysPath+"/sso-test", adm, h.sysSSOTest)
	route("POST "+sysPath+"/notify-test/{channel}", adm, h.sysNotifyTest)
	route("GET "+sysPath+"/reset", adm, h.sysResetConfirm)
	route("POST "+sysPath+"/reset", adm, h.sysReset)
	h.registerSnapshots(route, adm)
}

type sysRow struct {
	sysconfig.FieldState
	Posted        string // what the admin typed, shown again after an error
	Err           string
	InRestartCard bool     // the card header carries the Restart badge for all of its settings
	Deps          []sysRow // settings that only matter while this switch is on (see sysFieldCopy.Dep)
}

// ErrField is the input name of a row with an error (the field-error pattern
// compares it with the input's own name).
func (r sysRow) ErrField() string {
	if r.Err == "" {
		return ""
	}
	return r.InputName()
}

func (r sysRow) InputName() string { return "v." + r.Key }

// sysMQTT is the broker of the MQTT tab as host, port and TLS (N18). Legacy is set when the
// stored URL cannot be written as these parts; the page then keeps the URL field.
type sysMQTT struct {
	Host, Port             string
	StoredHost, StoredPort string // the broker that is saved: the keep option shows while the fields differ from it
	KeepTicked             bool   // the keep option was ticked in the save that is shown again
	HasPassword            bool   // a password is stored: the broker row offers to keep it for a changed broker
	TLS                    bool
	Legacy                 string // why the parts cannot hold the stored URL; empty when they can
}

// sysDiscovery is the status card of the Discovery tab: what MQTT discovery does right now.
type sysDiscovery struct {
	Tone        string // ok, warn, danger, neutral
	Title, Text string
	Waiting     int  // new devices waiting to be added
	Link        bool // the Discovered page exists
}

type sysPage struct {
	Base, CSRF   string
	Tabs         []sysTab
	ReadOnly     []sysconfig.Row
	Skipped      map[string]string
	Pending      []string // keys that wait for a restart
	Error        string
	NeedStepUp   bool
	StepUp       ui.StepUpData
	CannotStepUp bool
	StepUpKeys   []string
	MQTTResult   string
	SSOResult    string
	Active       string // slug of the tab shown
	StepUpErr    string // why the last confirmation failed (wrong password, too many attempts)
	MQTT         sysMQTT
	Discovery    sysDiscovery
	Storage      *sysStorage // Backups tab: storage summary and orphans (N9); nil without them
	Resetting    bool        // the step-up dialog is for "Reset settings…"
	StepUpWhat   string      // what the step-up dialog says it is for, when it is not a settings save
	DB           *sysDB      // the Snapshots page: stored snapshots and the schedule's status; nil without snapshots (filled by snapshotsRender)
	Notify       sysNotify   // Notifications tab: status lines, test results, the message preview
}

// sysStepUp is what the principal can do about a confirmation. Header
// principals can never confirm: the proxy is their only authority and
// tasmofleet has nothing to re-check (docs/security/web-import.md).
func (h *handler) sysStepUp(r *http.Request) sysconfig.StepUp {
	if p, _ := auth.FromContext(r.Context()); p.Method == "header" {
		return sysconfig.StepUp{}
	}
	return sysconfig.StepUp{Possible: true, Done: !h.needsStepUp(r)}
}

func (h *handler) sysData(r *http.Request, posted map[string]string, errs map[string]string) sysPage {
	svc := h.d.SysConfig
	p := sysPage{Base: httpBase(r), CSRF: h.d.Guard.CSRFToken(r), ReadOnly: svc.ReadOnly(), Skipped: svc.Skipped()}
	rows := map[string]sysRow{}
	discoveryOn, hasMQTTPassword := false, false
	for _, st := range svc.State() {
		row := sysRow{FieldState: st, Posted: st.Value, Err: errs[st.Key]}
		if v, ok := posted[st.Key]; ok && !st.Secret {
			row.Posted = v
		}
		rows[st.Key] = row
		if st.PendingRestart {
			p.Pending = append(p.Pending, st.Key)
		}
		switch st.Key {
		case "MQTT_URL":
			p.MQTT = sysMQTTOf(row.Posted)
			stored := sysMQTTOf(st.Value)
			p.MQTT.StoredHost, p.MQTT.StoredPort = stored.Host, stored.Port
		case "MQTT_PASSWORD":
			hasMQTTPassword = st.HasValue && !st.Locked // an environment password cannot follow a changed broker
		case "DISCOVERY_MQTT":
			discoveryOn = row.Posted == "true"
		}
	}
	saved := map[string]string{}
	for _, st := range svc.State() {
		saved[st.Key] = st.Value
	}
	p.Notify = h.sysNotifyOf(saved)
	p.MQTT.HasPassword = hasMQTTPassword
	p.MQTT.KeepTicked = posted["reuse.MQTT_PASSWORD"] == "1"
	p.Tabs = sysTabsOf(rows)
	p.Discovery = h.sysDiscoveryOf(r, discoveryOn)
	p.Active = r.URL.Query().Get("tab")
	if onSnapshots(r) {
		p.Active = sysSnapshotsSlug
	} else if p.Active == sysSnapshotsSlug {
		p.Active = "" // the Snapshots page is no tab of the Settings page
	}
	for _, tab := range p.Tabs {
		for _, s := range tab.Cards {
			for _, row := range s.Rows {
				if row.hasProblem() {
					p.Active = tab.Slug // an error is shown where it happened
				}
			}
		}
	}
	if !slices.ContainsFunc(p.Tabs, func(t sysTab) bool { return t.Slug == p.Active }) && len(p.Tabs) > 0 {
		p.Active = p.Tabs[0].Slug
	}
	return p
}

// sysMQTTOf splits the broker URL into the parts the MQTT tab edits.
func sysMQTTOf(raw string) sysMQTT {
	b, err := config.SplitMQTTURL(raw)
	if err != nil {
		return sysMQTT{Legacy: err.Error()}
	}
	m := sysMQTT{Host: b.Host, TLS: b.TLS}
	if b.Port != 0 {
		m.Port = strconv.Itoa(b.Port)
	}
	return m
}

// sysDiscoveryOf words the state of MQTT discovery for the card on top of the Discovery tab:
// switched off, waiting for a broker, or listening (with the new devices that wait).
func (h *handler) sysDiscoveryOf(r *http.Request, on bool) sysDiscovery {
	d := sysDiscovery{Link: h.d.Discovery != nil}
	state := ""
	if h.d.MQTT != nil {
		state = h.d.MQTT()
	}
	switch {
	case !on:
		d.Tone, d.Title, d.Text = "neutral", "MQTT discovery is off", "Announcements on tasmota/discovery/+/config are ignored. Switch them on below."
	case state == "connected":
		d.Tone, d.Title, d.Text = "ok", "MQTT discovery is listening", "Topic tasmota/discovery/+/config"
		if d.Link {
			d.Waiting = h.newDeviceCount(r)
		}
	case state == "disconnected":
		d.Tone, d.Title, d.Text = "danger", "MQTT discovery is waiting for the broker", "The broker is not connected; announcements arrive once it is."
	default:
		d.Tone, d.Title, d.Text = "warn", "MQTT discovery needs a broker", "Set the broker on the MQTT tab; until then devices are only found by a network scan."
	}
	return d
}

// sysStepUpData is the "Confirm it's you" dialog for a refused save: a local account types its
// password, a single sign-on account signs in at the identity provider again. Both come back to
// the tab with ?restore=1, where form.js puts the typed values back.
func (h *handler) sysStepUpData(r *http.Request, p sysPage) ui.StepUpData {
	pr, _ := auth.FromContext(r.Context())
	name := pr.Name
	if name == "" {
		name = h.pr.Text(pr.Subject)
	}
	sub := "Signed in with a local account"
	if pr.Method == "oidc" {
		sub = "Signed in with " + h.idpName()
	}
	var labels []string
	for _, k := range p.StepUpKeys {
		if f, ok := sysconfig.Lookup(k); ok {
			labels = append(labels, f.Label)
		}
	}
	what := "Changing the sign-in settings" // a failed password confirmation no longer knows the keys
	switch {
	case p.StepUpWhat != "":
		what = p.StepUpWhat
	case p.Resetting:
		what = "Resetting all settings"
	case len(labels) > 0:
		what = "Changing " + strings.Join(labels, ", ")
	}
	back := sysPath + "?tab=" + p.Active + "&restore=1"
	keyQuery := ""
	if p.Active == sysSnapshotsSlug {
		back = snapPath + "?restore=1"
		if snapKeyChosen(r) {
			keyQuery = "&" + snapKeyParam + "=1" // the snapshot download continues with the box as it was ticked (#387)
			back += keyQuery
		}
	}
	d := ui.StepUpData{ID: "stepup", Name: name, Sub: sub, What: what,
		CSRF: p.CSRF, Next: back, Open: true, Error: p.StepUpErr}
	if h.stepUpKind(r) == stepUpOIDC {
		d.IdP, d.IdPHref, d.Action = h.idpName(), p.Base+"/oidc/stepup", p.Base+"/oidc/stepup"
	} else {
		d.Local, d.Action = true, p.Base+sysPath+"/stepup?tab="+p.Active+keyQuery
		if p.Active == sysSnapshotsSlug {
			d.Action = p.Base + snapPath + "/stepup?" + strings.TrimPrefix(keyQuery, "&")
		}
	}
	return d
}

// idpName is the identity provider as the sign-in button names it (OIDC_DISPLAY_NAME).
func (h *handler) idpName() string {
	for _, st := range h.d.SysConfig.State() {
		if st.Key == "OIDC_DISPLAY_NAME" && st.Value != "" {
			return st.Value
		}
	}
	return "single sign-on"
}

func (h *handler) sysRender(w http.ResponseWriter, r *http.Request, status int, p sysPage) {
	if p.NeedStepUp {
		p.StepUp = h.sysStepUpData(r, p)
	}
	if p.Active == sysSnapshotsSlug {
		h.snapshotsRender(w, r, status, p)
		return
	}
	// The Snapshots page has its own place in the System menu: the Settings page shows its other tabs.
	p.Tabs = slices.DeleteFunc(p.Tabs, func(t sysTab) bool { return t.Slug == sysSnapshotsSlug })
	v := h.view(r, "Settings", "system", nil)
	switch q := r.URL.Query(); {
	case q.Get("ok") == "reset":
		v.Flash = "All settings are back at their defaults."
	case p.Active == "backups" && backupsNotice(r) != "":
		v.Flash = backupsNotice(r) // a backup of a deleted device was kept, released or deleted
	}
	if p.Storage == nil { // the tabs switch without a page load, so the Backups tab is always in the page
		st, err := h.sysStorageOf(r, p.Base)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.Storage = st
	}
	h.show(w, r, status, "system_settings", v, sysSettingsPage(p))
}

// snapshotsRender draws System -> Snapshots: only its own tab, with the stored snapshots read now.
func (h *handler) snapshotsRender(w http.ResponseWriter, r *http.Request, status int, p sysPage) {
	p.Tabs = slices.DeleteFunc(p.Tabs, func(t sysTab) bool { return t.Slug != sysSnapshotsSlug })
	v := h.view(r, "Snapshots", "snapshots", nil)
	if r.URL.Query().Get("ok") == "snapshot-deleted" {
		v.Flash = "Snapshot deleted."
	}
	if p.DB == nil {
		db, err := h.sysDBOf(r, p.Base)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		p.DB = db
	}
	h.show(w, r, status, "snapshots", v, snapshotsPage(p))
}

func (h *handler) sysPage(w http.ResponseWriter, r *http.Request) {
	h.sysRender(w, r, http.StatusOK, h.sysData(r, nil, nil))
}

// sysResetText is what "Reset settings…" does, in the dialog and on its page.
const sysResetText = "Every setting on this page goes back to its default: the device networks, the MQTT broker, " +
	"the sign-in method and everything else that was saved here. Settings set by the container configuration stay. " +
	"Devices, backups and users are kept."

// sysResetConfirm is the confirmation of "Reset settings…" as a page (no JavaScript).
func (h *handler) sysResetConfirm(w http.ResponseWriter, r *http.Request) {
	h.renderActionConfirm(w, r, "system", actionConfirm{
		Title: "Reset all settings?", Heading: "Reset all settings", Text: sysResetText,
		Crumbs: []confirmLink{{"/settings/system", "Settings"}, {sysPath + "?tab=advanced", "Advanced"}}, Last: "Reset",
		Action: sysPath + "/reset", Verb: "Reset settings", CancelHref: sysPath + "?tab=advanced", CancelLabel: "Cancel",
	})
}

// sysReset puts every stored setting back to its default (N9). It always needs the step-up:
// without it the Advanced tab opens the "Confirm it's you" dialog and nothing changes.
func (h *handler) sysReset(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, err := h.d.SysConfig.Reset(r.Context(), sysconfig.ResetRequest{Actor: actor(r), StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	var need *sysconfig.StepUpRequiredError
	switch {
	case err == nil:
		redirect(w, r, sysPath+"?tab=advanced&ok=reset")
	case errors.As(err, &need):
		p := h.sysData(r, nil, nil)
		p.Active = "advanced"
		if need.Possible {
			p.NeedStepUp, p.Resetting = true, true
		} else {
			p.Error = "Nothing was reset. Resetting all settings needs a confirmation that this account cannot give."
		}
		h.sysRender(w, r, http.StatusForbidden, p)
	default:
		h.sysOutcome(w, r, nil, err, nil)
	}
}

// sysForm reads the posted form. Only keys the page offers are accepted: v.KEY
// for an editable field, clear.KEY to go back to the default, b.KEY for an on/off
// field drawn as a checkbox, and csrf. Any other key fails the whole request (no
// mass assignment). A checkbox sends nothing when it is off, so b.KEY says the
// field was on the page: without v.KEY next to it the field is "false".
func (h *handler) sysForm(w http.ResponseWriter, r *http.Request, allowClear bool, extras ...string) (values map[string]string, clear []string, extra map[string]string, ok bool) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return nil, nil, nil, false
	}
	values, extra = map[string]string{}, map[string]string{}
	var boxes, lists []string // lists: choice fields drawn as one checkbox per choice (m.KEY marks them)
	for k, vs := range r.PostForm {
		switch {
		case k == "csrf":
		case strings.HasPrefix(k, "b."):
			f, known := sysconfig.Lookup(strings.TrimPrefix(k, "b."))
			if !known || !f.Bool || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			boxes = append(boxes, f.Key)
		case strings.HasPrefix(k, "m."):
			f, known := sysconfig.Lookup(strings.TrimPrefix(k, "m."))
			if !known || len(f.Choices) == 0 || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			lists = append(lists, f.Key)
		case slices.Contains(extras, k) && len(vs) == 1:
			extra[k] = vs[0]
		case strings.HasPrefix(k, "v."):
			f, known := sysconfig.Lookup(strings.TrimPrefix(k, "v."))
			if known && len(f.Choices) > 0 {
				for i := range vs {
					vs[i] = strings.TrimSpace(vs[i])
				}
				values[f.Key] = strings.Join(vs, ",")
				continue
			}
			if !known || len(vs) != 1 {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			v := strings.TrimSpace(vs[0])
			if f.Multiline {
				v = strings.Join(strings.FieldsFunc(v, func(c rune) bool { return c == '\n' || c == '\r' || c == ',' || c == ' ' }), ",")
			}
			if f.Secret && v == "" {
				continue // write-only: empty means unchanged
			}
			values[f.Key] = v
		case allowClear && strings.HasPrefix(k, "reuse."):
			if _, known := sysconfig.Lookup(strings.TrimPrefix(k, "reuse.")); !known || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			extra[k] = "1"
		case allowClear && strings.HasPrefix(k, "clear."):
			if _, known := sysconfig.Lookup(strings.TrimPrefix(k, "clear.")); !known || len(vs) != 1 || vs[0] != "1" {
				http.Error(w, "unknown setting", http.StatusBadRequest)
				return nil, nil, nil, false
			}
			clear = append(clear, strings.TrimPrefix(k, "clear."))
		default:
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return nil, nil, nil, false
		}
	}
	for _, k := range boxes {
		if _, on := values[k]; !on && !slices.Contains(clear, k) {
			values[k] = "false"
		}
	}
	for _, k := range lists {
		if _, on := values[k]; !on && !slices.Contains(clear, k) {
			values[k] = "" // no box ticked: store "no source" instead of silently keeping the old value
		}
	}
	for _, k := range clear {
		if _, both := values[k]; both {
			http.Error(w, "a setting cannot be set and cleared at once", http.StatusBadRequest)
			return nil, nil, nil, false
		}
	}
	return values, clear, extra, true
}

func (h *handler) sysSave(w http.ResponseWriter, r *http.Request) {
	values, clear, extra, ok := h.sysForm(w, r, true, sysFormExtras...)
	if !ok {
		return
	}
	var reuse []string
	// shown is what the page shows again after a refused save: the values, and the keep option as ticked.
	shown := func() map[string]string {
		m := maps.Clone(values)
		if slices.Contains(reuse, "MQTT_PASSWORD") {
			m["reuse.MQTT_PASSWORD"] = "1"
		}
		return m
	}
	for k := range extra {
		if strings.HasPrefix(k, "reuse.") {
			reuse = append(reuse, strings.TrimPrefix(k, "reuse."))
		}
	}
	if slices.Contains(clear, "MQTT_URL") {
		// A reset to the default wins over the broker fields, which the form always posts.
		for _, k := range mqttPartKeys {
			delete(extra, k)
		}
	} else if err := composeMQTTURL(values, extra, h.storedMQTTURL()); err != nil {
		h.sysOutcome(w, r, shown(), err, nil)
		return
	}
	if v, ok := values["FIRMWARE_SOURCES"]; ok {
		values["FIRMWARE_SOURCES"] = keepSourceOrder(v, h.storedValue("FIRMWARE_SOURCES"))
	}
	if err := composeSchedule(values, clear, extra); err != nil {
		h.sysOutcome(w, r, shown(), err, nil)
		return
	}
	_, err := h.d.SysConfig.Apply(r.Context(), sysconfig.Request{
		Actor: actor(r), Values: values, Clear: clear, Reuse: reuse, StepUp: h.sysStepUp(r), Audit: h.d.Recorder,
	})
	// Back to the tab that was saved, so the values just written are what the person sees.
	back := sysPath + "?ok=saved"
	if tab := sysTabSlug(r.URL.Query().Get("tab")); tab != "" && tab != sysSnapshotsSlug {
		back = sysPath + "?tab=" + tab + "&ok=saved"
	}
	if onSnapshots(r) {
		back = snapPath + "?ok=saved"
	}
	h.sysOutcome(w, r, shown(), err, func() { redirect(w, r, back) })
}

// sysOutcome renders the answer to Apply or TestMQTT errors, or calls done.
func (h *handler) sysOutcome(w http.ResponseWriter, r *http.Request, posted map[string]string, err error, done func()) {
	var ve *sysconfig.ValidationError
	var need *sysconfig.StepUpRequiredError
	switch {
	case err == nil:
		done()
	case errors.As(err, &ve):
		p := h.sysData(r, posted, ve.Fields)
		p.Error = "Nothing was saved. Fix the marked fields."
		// A message about a setting this page does not edit (for instance PUBLIC_URL
		// for single sign-on) has no field to sit at: say it in the summary.
		for _, k := range sortedKeys(ve.Fields) {
			if _, editable := sysconfig.Lookup(k); !editable {
				p.Error += " " + k + ": " + ve.Fields[k] + " (set it in the container configuration)."
			}
		}
		h.sysRender(w, r, http.StatusUnprocessableEntity, p)
	case errors.Is(err, sysconfig.ErrLocked):
		p := h.sysData(r, nil, nil)
		p.Error = "Nothing was saved. A setting in the request is set by the environment and cannot be changed here."
		h.sysRender(w, r, http.StatusForbidden, p)
	case errors.As(err, &need):
		p := h.sysData(r, posted, nil)
		p.StepUpKeys = need.Keys
		if need.Possible {
			p.NeedStepUp = true // the dialog says what happens; nothing was saved
		} else {
			p.CannotStepUp = true
			p.Error = "Nothing was saved. These settings need a confirmation that this account cannot give; change them in the container configuration instead."
		}
		h.sysRender(w, r, http.StatusForbidden, p)
	case errors.Is(err, sysconfig.ErrUnknownKey):
		http.Error(w, "unknown setting", http.StatusBadRequest)
	case errors.Is(err, sysconfig.ErrTooSoon):
		p := h.sysData(r, posted, nil)
		p.Error = "Wait a few seconds between connection tests."
		h.sysRender(w, r, http.StatusTooManyRequests, p)
	default:
		h.fail(w, r, err)
	}
}

const mqttTestHelp = "Tries one connection with the values above and saves nothing. A password typed above is the one tested; otherwise the stored one, only for the broker it was entered for."

// sysTestFragment answers the script-driven test button (HX-Request) with only the test row. The
// page stays as it is, so a password typed into the form survives the test and reaches the save
// that follows; a full reload would blank it, because a secret is never rendered back.
func (h *handler) sysTestFragment(w http.ResponseWriter, r *http.Request, msg string, err error) {
	var ve *sysconfig.ValidationError
	switch {
	case err == nil:
	case errors.As(err, &ve):
		var why []string
		for _, k := range sortedKeys(ve.Fields) {
			why = append(why, ve.Fields[k])
		}
		msg = "failed: " + strings.Join(why, "; ")
	case errors.Is(err, sysconfig.ErrTooSoon):
		msg = "failed: wait a few seconds between connection tests"
	default:
		h.fail(w, r, err)
		return
	}
	h.component(w, r, http.StatusOK, sysTestRow("Test connection", httpBase(r)+sysPath+"/mqtt-test?tab=mqtt", "MQTT connection test", msg, mqttTestHelp))
}

// sysMQTTTest checks the broker. The "Test MQTT" button sits in the MQTT tab of the settings
// form, so the whole form arrives (every key is still validated by sysForm); only the broker
// fields count, and nothing is saved. A password typed into the form is the one tested,
// otherwise the setup page's test_password, otherwise the stored one (bound to its broker).
func (h *handler) sysMQTTTest(w http.ResponseWriter, r *http.Request) {
	all, _, extra, ok := h.sysForm(w, r, true, append([]string{"test_password"}, sysFormExtras...)...)
	if !ok {
		return
	}
	htmx := r.Header.Get("HX-Request") != ""
	if err := composeMQTTURL(all, extra, h.storedMQTTURL()); err != nil {
		if htmx {
			h.sysTestFragment(w, r, "", err)
			return
		}
		h.sysOutcome(w, r, all, err, nil)
		return
	}
	values := map[string]string{}
	for _, k := range []string{"MQTT_URL", "MQTT_USER", "MQTT_FULLTOPIC"} {
		if v, ok := all[k]; ok {
			values[k] = v
		}
	}
	password := extra["test_password"]
	if password == "" {
		password = all["MQTT_PASSWORD"]
	}
	msg, err := h.d.SysConfig.TestMQTT(r.Context(), sysconfig.TestRequest{Actor: actor(r), Values: values, Password: password, StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	if errors.Is(err, sysconfig.ErrNoTester) {
		http.NotFound(w, r)
		return
	}
	if htmx {
		h.sysTestFragment(w, r, msg, err)
		return
	}
	h.sysOutcome(w, r, values, err, func() {
		p := h.sysData(r, values, nil)
		p.MQTTResult = msg
		h.sysRender(w, r, http.StatusOK, p)
	})
}

// sysStepUpPost takes the password of a local admin (OIDC admins use
// POST /oidc/stepup, header principals cannot confirm).
func (h *handler) sysStepUpPost(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	page := func(status int, msg string) {
		p := h.sysData(r, nil, nil)
		p.NeedStepUp, p.StepUpErr = true, msg
		h.sysRender(w, r, status, p)
	}
	p, _ := auth.FromContext(r.Context())
	su, ok := h.d.Guard.(stepUpper)
	if p.Method != "local" || !ok {
		h.auditImport(r, "sysconfig.stepup", "denied: password step-up needs a local account ("+p.Method+")")
		page(http.StatusForbidden, "A password confirmation is only available for local accounts. Accounts from the identity provider confirm at the identity provider.")
		return
	}
	okPw, limited := su.StepUp(r, r.PostFormValue("current"))
	switch {
	case limited:
		page(http.StatusTooManyRequests, "Too many attempts. Wait a minute and try again.")
	case !okPw:
		page(http.StatusForbidden, "The password is wrong.")
	default:
		back := sysPath + "?restore=1" // form.js puts the typed values back
		if tab := sysTabSlug(r.URL.Query().Get("tab")); tab != "" && tab != sysSnapshotsSlug {
			back = sysPath + "?tab=" + tab + "&restore=1"
		}
		if onSnapshots(r) {
			back = snapPath + "?restore=1"
			if r.URL.Query().Get(snapKeyParam) == "1" {
				back += "&" + snapKeyParam + "=1"
			}
		}
		redirect(w, r, back)
	}
}

// sysTabSlug accepts a tab name from a URL only when it is one of the page's tabs.
func sysTabSlug(s string) string {
	for _, t := range sysLayout {
		if t.Slug == s {
			return s
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sysSSOTest checks that the issuer answers discovery. Like the MQTT test it is a button of
// the settings form: the whole form arrives, only the issuer counts, nothing is saved.
func (h *handler) sysSSOTest(w http.ResponseWriter, r *http.Request) {
	all, _, _, ok := h.sysForm(w, r, true, sysFormExtras...)
	if !ok {
		return
	}
	values := map[string]string{}
	if v, ok := all["OIDC_ISSUER"]; ok {
		values["OIDC_ISSUER"] = v
	}
	msg, err := h.d.SysConfig.TestSSO(r.Context(), sysconfig.TestRequest{Actor: actor(r), Values: values, Audit: h.d.Recorder})
	if errors.Is(err, sysconfig.ErrNoTester) {
		http.NotFound(w, r)
		return
	}
	h.sysOutcome(w, r, values, err, func() {
		p := h.sysData(r, values, nil)
		p.SSOResult = msg
		h.sysRender(w, r, http.StatusOK, p)
	})
}

// mqttPartKeys are the broker fields of the MQTT tab (N18): host, port and a
// TLS checkbox instead of one URL field. They are not settings themselves:
// composeMQTTURL turns them into MQTT_URL, the stored value.
var mqttPartKeys = []string{"mqtt.host", "mqtt.port", "mqtt.tls"}

// scheduleIntervals are the settings that sit behind a schedule switch on the Backups tab: the
// device backup interval and the database snapshot interval.
var scheduleIntervals = []string{"BACKUP_INTERVAL", "SNAPSHOT_INTERVAL"}

// scheduleKeys are the schedule switches of the Backups tab: swm.* says the switch was on the
// page (an unticked box sends nothing), sw.* is its state. They are a view of the interval settings.
var scheduleKeys = func() []string {
	var out []string
	for _, k := range scheduleIntervals {
		out = append(out, "swm."+k, "sw."+k)
	}
	return out
}()

// sysFormExtras are the form fields that are no setting: the broker parts and the schedule switches.
// Every button that posts the whole form must accept them.
var sysFormExtras = append(slices.Clone(mqttPartKeys), scheduleKeys...)

// composeSchedule turns each schedule switch into its interval setting: off is 0 (manual only /
// no snapshots), on needs a real interval. A reset to the default wins over the switch.
func composeSchedule(values map[string]string, clear []string, extra map[string]string) error {
	var bad map[string]string
	for _, key := range scheduleIntervals {
		marked := extra["swm."+key] == "1"
		on := extra["sw."+key] == "1"
		delete(extra, "swm."+key)
		delete(extra, "sw."+key)
		if !marked || slices.Contains(clear, key) {
			continue
		}
		if !on {
			values[key] = "0"
			continue
		}
		switch values[key] {
		case "", "0", "0s":
			if bad == nil {
				bad = map[string]string{}
			}
			bad[key] = "enter how often, for example 24h"
		}
	}
	if bad != nil {
		return &sysconfig.ValidationError{Fields: bad}
	}
	return nil
}

// composeMQTTURL replaces the posted broker parts with the MQTT_URL they
// compose (config.JoinMQTTURL). A form without mqtt.host is left alone (the
// plain v.MQTT_URL field still works); a form with both is refused. An empty
// host turns MQTT off. Errors are field errors of MQTT_URL. When the parts
// name the broker stored already (current), current is kept as it is: a
// stored mqtt:// or tls:// URL, or one without a port, would otherwise be
// rewritten on every save, which reconnects MQTT and, because the scheme is
// part of the MQTT password's target, asks for that password again.
func composeMQTTURL(values, extra map[string]string, current string) error {
	host, ok := extra["mqtt.host"]
	if !ok {
		if _, port := extra["mqtt.port"]; port || extra["mqtt.tls"] != "" {
			return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "enter the broker host"}}
		}
		return nil
	}
	if _, both := values["MQTT_URL"]; both {
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "send the broker either as a URL or as host, port and TLS"}}
	}
	b := config.Broker{Host: host}
	switch extra["mqtt.tls"] {
	case "":
	case "1", "on", "true":
		b.TLS = true
	default:
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "TLS must be on or off"}}
	}
	if p := strings.TrimSpace(extra["mqtt.port"]); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": "port must be 1-65535"}}
		}
		b.Port = n
	}
	u, err := config.JoinMQTTURL(b)
	if err != nil {
		return &sysconfig.ValidationError{Fields: map[string]string{"MQTT_URL": err.Error()}}
	}
	if cur, err := config.SplitMQTTURL(current); err == nil && current != "" {
		if now, _ := config.SplitMQTTURL(u); now == cur {
			u = current
		}
	}
	values["MQTT_URL"] = u
	for _, k := range mqttPartKeys {
		delete(extra, k)
	}
	return nil
}

// keepSourceOrder merges the ticked firmware sources with the stored list. The order matters (the
// first source is the primary one), but the checkboxes are drawn in the order of the known sources:
// the same set keeps the stored value untouched (no spurious change, restart notice or audit entry),
// a changed set keeps the stored order for the sources still ticked and appends the new ones.
func keepSourceOrder(posted, current string) string {
	split := func(s string) []string {
		var out []string
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	ticked, stored := split(posted), split(current)
	if len(ticked) == len(stored) && !slices.ContainsFunc(ticked, func(t string) bool { return !slices.Contains(stored, t) }) {
		return current
	}
	var out []string
	for _, c := range stored {
		if slices.Contains(ticked, c) {
			out = append(out, c)
		}
	}
	for _, t := range ticked {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// storedValue is the current value of a setting from the settings service.
func (h *handler) storedValue(key string) string {
	for _, f := range h.d.SysConfig.State() {
		if f.Key == key {
			return f.Value
		}
	}
	return ""
}

// storedMQTTURL is the current MQTT_URL from the settings service.
func (h *handler) storedMQTTURL() string {
	for _, f := range h.d.SysConfig.State() {
		if f.Key == "MQTT_URL" {
			return f.Value
		}
	}
	return ""
}

// sysTabLinks are the tabs of the page as links (?tab=), the shown one current.
func sysTabLinks(p sysPage) []ui.Tab {
	out := make([]ui.Tab, 0, len(p.Tabs))
	for _, t := range p.Tabs {
		out = append(out, ui.Tab{Label: t.Label, Href: p.Base + sysPath + "?tab=" + t.Slug, Current: t.Slug == p.Active})
	}
	return out
}

// sysLevels are the log levels in the prototype's order and wording (Error, Warn, Info, Debug).
var sysLevels = []string{"error", "warn", "info", "debug"}

func sysLevelOptions(cur string) []ui.Option {
	out := make([]ui.Option, 0, len(sysLevels))
	for _, l := range sysLevels {
		out = append(out, ui.Option{Value: l, Label: strings.ToUpper(l[:1]) + l[1:], Checked: l == cur})
	}
	return out
}

// sysAuthOptions are the sign-in methods as the prototype's radio cards.
func sysAuthOptions(cur string) []ui.Option {
	return []ui.Option{
		{Value: "local", Label: "Local accounts", Text: "Usernames and passwords stored in TasmoFleet.", Checked: cur == "local"},
		{Value: "oidc", Label: "Single sign-on (OpenID Connect)", Text: "Authentik, Keycloak, Authelia and similar. Accounts are created on first sign-in.", Checked: cur == "oidc"},
	}
}

// sysDescribe points a control at its message: the error (and aria-invalid) when the server
// rejected it, else the help line.
func sysDescribe(row sysRow) templ.Attributes {
	if row.Err != "" {
		return templ.Attributes{"aria-invalid": "true", "aria-describedby": ui.SettingErrID(row.Key)}
	}
	if sysFieldCopy(row.Key).Help != "" {
		return templ.Attributes{"aria-describedby": ui.SettingHelpID(row.Key)}
	}
	return templ.Attributes{}
}

// sysFieldDataGroup is sysFieldData for controls that are a group (segments, radio cards, boxes):
// the label is not tied to one input.
func sysFieldDataGroup(row sysRow) ui.SettingFieldData {
	d := sysFieldData(row)
	d.Group = row.Key == "LOG_LEVEL" || row.Key == "AUTH_MODE" || len(row.Choices) > 0 || sysSegmentedEnum(row.Key)
	if row.Key == "AUTH_MODE" {
		// The radio cards carry their own (hidden) legend and the card's header the badges: no label
		// row, so no lone line above the choices.
		d.Label, d.Restart, d.CanReset = "", false, false
	}
	return d
}

// sysSwitchData is sysFieldData for an on/off setting: the switch sits left of the label.
func sysSwitchData(row sysRow) ui.SettingFieldData {
	d := sysFieldData(row)
	d.Switch, d.Warn = true, row.Key == "FIRMWARE_ALLOW_SINGLE_SOURCE"
	return d
}

// sysSecretHint is the placeholder of a write-only field: whether a value is stored.
func sysSecretHint(row sysRow) string {
	if row.HasValue {
		return "unchanged (a value is stored)"
	}
	return "not set"
}

func sysPlaceholder(row sysRow) string {
	if row.Key == "BACKUP_INTERVAL" || row.Key == "SNAPSHOT_INTERVAL" {
		return "24h"
	}
	if row.Key == "DEVICE_NETWORKS" {
		return "192.168.1.0/24"
	}
	return ""
}

// sysSetWord says whether a secret from the environment has a value, never the value.
func sysSetWord(set bool) string {
	if set {
		return "a value is set"
	}
	return "not set"
}

// sysNotice is the message of a stored secret that must be entered again, shown under the field.
func sysNotice(row sysRow) string {
	if row.NeedsReentry {
		return row.Skipped
	}
	return ""
}

// sysIntervalShown is the backup interval as the field shows it: while the schedule is off the
// stored 0 (manual only) is not a value to read, so the field is empty and its placeholder
// proposes one. Saving with the switch off still stores 0 (composeSchedule).
func sysIntervalShown(row sysRow) sysRow {
	if !sysScheduleOn(row) {
		row.Posted = ""
	}
	return row
}

// sysCanReset says whether "Reset to default" is offered: a stored value that differs from the
// default (a secret that is stored always does). A stored value equal to the default has nothing to reset.
func sysCanReset(row sysRow) bool {
	return row.Source == "stored" && (row.Secret || row.Value != config.DefaultValue(row.Key))
}
