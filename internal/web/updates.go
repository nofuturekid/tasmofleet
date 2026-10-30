package web

import (
	"errors"
	"fmt"
	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// updateOption is one row of the device picker. Blocked says why a device
// cannot be selected (empty when it can); Update marks devices whose
// firmware is older than the release the form starts with.
type updateOption struct {
	Device   model.Device
	Health   model.HealthState
	Selected bool
	Blocked  string
	Update   bool
	Status   string  // the health as a word ("Offline"): a block reason that only repeats it is not printed
	Dot      ui.Tone // the status dot
	File     string  // the upstream file the release would install (for the plan's file count), "" unknown
}

// ChipVersion is the row's right-hand side: "ESP32-C3 · 14.2.0".
func (o updateOption) ChipVersion() string {
	var parts []string
	if f := o.Device.Identity.Family; f != model.ChipUnknown {
		parts = append(parts, chipName(f))
	}
	if v := o.Device.Identity.Version; v != "" {
		parts = append(parts, v)
	}
	if len(parts) == 0 {
		return "Unknown"
	}
	return strings.Join(parts, " · ")
}

// familyGroup is a quick selector for all devices of one chip family.
type familyGroup struct {
	Family string
	Count  int
}

type updateForm struct {
	Options    []updateOption
	Families   []familyGroup
	Filter     string // query string of the device filter, kept by the no-JS "select" buttons
	Releases   []string
	Customs    []customOption // uploaded images, offered after the upstream releases, never preselected
	Release    string
	Policy     string
	Variant    string
	SkipBackup bool
	// PowerAfter is the "Power after update" choice: powerLights (default),
	// powerLightsRelays or powerLeave. One value instead of two flags, so the
	// combination "restore relays but skip the restore" cannot be posted.
	PowerAfter string
	// Delivery is the "Delivery" choice: empty (every device downloads the
	// image, default) or "push" (push where possible: tasmofleet uploads it
	// to each ESP32 that can take it, the others download).
	Delivery string
	Error    string
	Notice   string

	Base      string
	RelOpts   []ui.Option // the release cards: the newest two upstream releases and the uploads
	OlderRels []ui.Option // the other upstream releases, folded
	Parallel  int         // devices updated at a time (the fleet-wide limit)
}

// SelectedN is the number of preselected devices (the plan's "Devices" without JavaScript).
func (f updateForm) SelectedN() int {
	n := 0
	for _, o := range f.Options {
		if o.Selected {
			n++
		}
	}
	return n
}

// FileCount is the number of different firmware files the preselected devices need.
func (f updateForm) FileCount() int {
	files := map[string]bool{}
	for _, o := range f.Options {
		if o.Selected && o.File != "" {
			files[o.File] = true
		}
	}
	return len(files)
}

// PolicyOpts, PowerOpts and DeliveryOpts are the choices of the Options section, the current one
// checked.
func (f updateForm) PolicyOpts() []ui.Option {
	return checked(f.Policy, string(model.PolicyOnlyNewer), []ui.Option{
		{Value: string(model.PolicyOnlyNewer), Label: "Only newer", Text: "Devices already at or above the release are left alone."},
		{Value: string(model.PolicyReinstall), Label: "Reinstall", Text: "Flash the release even if the device already runs it."},
		{Value: string(model.PolicyAllowDowngrade), Label: "Allow downgrade", Text: "Also install an older release than the one on the device."},
	})
}

func (f updateForm) PowerOpts() []ui.Option {
	return checked(f.PowerAfter, powerLights, []ui.Option{
		{Value: powerLights, Label: "Restore lights"},
		{Value: powerLightsRelays, Label: "Restore lights and relays"},
		{Value: powerLeave, Label: "Leave as is"},
	})
}

func (f updateForm) DeliveryOpts() []ui.Option {
	return checked(f.Delivery, "", []ui.Option{
		{Value: "", Label: "Download (all devices)"},
		{Value: string(model.DeliveryPush), Label: "Push where possible (ESP32; others download)"},
	})
}

// checked marks the option whose value is v (def when v matches none).
func checked(v, def string, opts []ui.Option) []ui.Option {
	found := false
	for i := range opts {
		if opts[i].Value == v {
			opts[i].Checked, found = true, true
		}
	}
	if !found {
		for i := range opts {
			opts[i].Checked = opts[i].Value == def
		}
	}
	return opts
}

// The values of the "Power after update" select.
const (
	powerLights       = "lights"
	powerLightsRelays = "lights-relays"
	powerLeave        = "leave"
)

// SkipPowerRestore: leave the power state as the update finds it.
func (f updateForm) SkipPowerRestore() bool { return f.PowerAfter == powerLeave }

// powerAfter maps the select value to the engine options. Empty means the
// default (lights only).
func powerAfter(v string) (skip, relays bool, err error) {
	switch v {
	case "", powerLights:
		return false, false, nil
	case powerLightsRelays:
		return false, true, nil
	case powerLeave:
		return true, false, nil
	}
	return false, false, errors.New("unknown power setting")
}

// formDevices returns the devices of the posted/queried "id" values.
func (h *handler) formIDs(r *http.Request) []model.DeviceID {
	vals := r.URL.Query()["id"]
	if r.Method == http.MethodPost {
		vals = r.PostForm["id"]
	}
	var out []model.DeviceID
	seen := map[model.DeviceID]bool{}
	for _, v := range vals {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !seen[model.DeviceID(n)] {
			seen[model.DeviceID(n)] = true
			out = append(out, model.DeviceID(n))
		}
	}
	return out
}

// pickMode names a server-side selection, used by the no-JS buttons of the
// picker (and the "update all matching" link of the device list): "all",
// "none", "update" (firmware older than the release) or "family:<chip>".
type pickMode string

func (m pickMode) picks(o updateOption) bool {
	if o.Blocked != "" {
		return false
	}
	switch {
	case m == "all":
		return true
	case m == "update":
		return o.Update
	case strings.HasPrefix(string(m), "family:"):
		return string(o.Device.Identity.Family) == strings.TrimPrefix(string(m), "family:")
	}
	return false
}

// blockedReason says why a device cannot be updated right now.
func blockedReason(d model.Device, st model.HealthState) string {
	switch {
	case d.OTAExcluded:
		return "Excluded from updates"
	case st == model.HealthOffline:
		return "Offline"
	case st == model.HealthUnknown && d.Identity.Version == "":
		return "Never answered" // listed as Unknown (shownHealth); its hardware is not known either
	case st == model.HealthAuthFailed:
		return "Login to the device failed"
	}
	return ""
}

func (h *handler) updateFormData(r *http.Request, selected []model.DeviceID, mode pickMode, filter deviceFilter) (updateForm, error) {
	f := updateForm{Policy: string(model.PolicyOnlyNewer), Filter: filter.values().Encode()}
	rows, err := h.rows(r.Context())
	if err != nil {
		return f, err
	}
	rels, err := h.d.Firmware.Releases(r.Context())
	if err != nil {
		f.Error = "The firmware index could not be loaded: " + err.Error()
	}
	for _, rel := range rels {
		f.Releases = append(f.Releases, rel.Version)
	}
	if len(f.Releases) > 0 {
		f.Release = f.Releases[0]
	}
	var uploaded []firmware.CustomImage
	if cu, ok := h.customService(); ok {
		imgs, cerr := cu.CustomImages(r.Context())
		if cerr != nil {
			f.Error = "The uploaded images could not be listed: " + cerr.Error()
		}
		for _, img := range imgs {
			if img.Artifact.Source == firmware.CustomSource {
				f.Customs = append(f.Customs, customOption{Key: img.Artifact.Custom, Version: img.Artifact.Release, Label: img.Label})
				uploaded = append(uploaded, img)
			}
		}
	}
	// "Start update" on the firmware page names a release; the way back from the preview names
	// any choice, an upload too.
	if want := r.FormValue("release"); want != "" && (slices.Contains(f.Releases, want) || slices.ContainsFunc(f.Customs, func(c customOption) bool { return customReleasePrefix+c.Key == want })) {
		f.Release = want
	}
	f.RelOpts, f.OlderRels = releaseOptions(rels, uploaded, f.Release)
	f.Parallel = h.otaParallel()
	sel := map[model.DeviceID]bool{}
	for _, id := range selected {
		sel[id] = true
	}
	// A device in a running transaction is refused by the engine; the picker says so up front.
	busy := map[model.DeviceID]bool{}
	if h.d.Jobs != nil {
		if act, err := h.d.Jobs.ListJobs(r.Context(), store.JobFilter{States: activeJobStates}); err == nil {
			for _, j := range act {
				busy[j.DeviceID] = true
			}
		}
	}
	fams := map[string]int{}
	for _, row := range rows {
		d := row.Device
		blocked := blockedReason(d, row.Health.State)
		if blocked == "" && busy[d.ID] {
			blocked = "In a running transaction"
		}
		o := updateOption{Device: d, Health: row.Health.State, Blocked: blocked,
			Update: f.Release != "" && versionOlder(d.Identity.Version, f.Release),
			Status: label(row.Health.State), Dot: healthDot(row.Health.State), File: upstreamFile(d.Identity, f.Release)}
		o.Selected = o.Blocked == "" && (sel[d.ID] || (mode != "" && filter.match(row) && mode.picks(o)))
		f.Options = append(f.Options, o)
		if d.Identity.Family != model.ChipUnknown && o.Blocked == "" {
			fams[string(d.Identity.Family)]++
		}
	}
	for fam, n := range fams {
		f.Families = append(f.Families, familyGroup{Family: fam, Count: n})
	}
	sort.Slice(f.Families, func(i, j int) bool { return f.Families[i].Family < f.Families[j].Family })
	return f, nil
}

// versionOlder reports whether the dotted numeric version have is older than
// want. Anything that does not parse (empty, "unknown") is not "older": the
// picker must not pre-select devices it cannot judge.
func versionOlder(have, want string) bool {
	a, b := versionNums(have), versionNums(want)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func versionNums(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+( "); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// updateForm is the new-transaction page. GET takes a preselection from the query (links), POST
// from the form body (Update firmware in the device list's bulk bar, which posts the selection
// with the other bulk actions; a GET would carry the form's CSRF token into the URL).
func (h *handler) updateForm(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	_ = r.ParseForm() // a broken body only loses the preselection: the page itself still renders
	q := r.URL.Query()
	ids, mode := h.formIDs(r), pickMode(q.Get("select"))
	if len(ids) == 0 && mode == "" && r.Method == http.MethodGet {
		mode = "update" // a fresh page starts with every device that has an update (the prototype's default)
	}
	f, err := h.updateFormData(r, ids, mode, filterOf(q))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if r.Method == http.MethodPost { // the way back from the preview keeps the options
		f.formChoices(r)
	}
	h.showUpdateForm(w, r, http.StatusOK, f)
}

// formChoices takes the options the user chose from the posted form (the preview's "Change").
func (f *updateForm) formChoices(r *http.Request) {
	if v := r.PostFormValue("policy"); v != "" {
		f.Policy = v
	}
	f.Variant, f.PowerAfter, f.Delivery = r.PostFormValue("variant"), r.PostFormValue("power_after"), r.PostFormValue("delivery")
	f.SkipBackup = skipBackup(r)
}

// skipBackup reads the backup choice. The picker's checkbox says "Back up each device" (it posts
// backup=1 when ticked, next to backup_shown=1); the preview's hidden fields carry skip_backup=1.
func skipBackup(r *http.Request) bool {
	if r.PostFormValue("backup_shown") == "1" {
		return r.PostFormValue("backup") != "1"
	}
	return r.PostFormValue("skip_backup") == "1"
}

func (h *handler) showUpdateForm(w http.ResponseWriter, r *http.Request, status int, f updateForm) {
	f.Base = httpBase(r)
	v := h.view(r, "New update", "updates", nil)
	h.show(w, r, status, "updates_new", v, updateNewPage(f, v.CSRF))
}

// requests builds one ota.Request per posted device. A device that runs
// minimal firmware has no variant of its own, so it gets the default
// "tasmota" build, with the variant change allowed explicitly (the preview
// says so); the user's variant override applies to everyone.
func (h *handler) requests(r *http.Request, ids []model.DeviceID) ([]ota.Request, error) {
	release := strings.TrimSpace(r.PostFormValue("release"))
	if release == "" {
		return nil, errors.New("choose a release")
	}
	// "custom:<key>" names an uploaded image; its release is the image's version.
	var custom string
	if key, ok := strings.CutPrefix(release, customReleasePrefix); ok {
		var err error
		if release, err = h.customRelease(r.Context(), key); err != nil {
			return nil, err
		}
		custom = key
	}
	variant := strings.TrimSpace(r.PostFormValue("variant"))
	skipPower, restoreRelays, err := powerAfter(r.PostFormValue("power_after"))
	if err != nil {
		return nil, err
	}
	delivery := model.Delivery(r.PostFormValue("delivery"))
	if delivery != model.DeliveryPull && delivery != model.DeliveryPush {
		return nil, errors.New("unknown delivery")
	}
	policy := model.UpdatePolicy(r.PostFormValue("policy"))
	switch policy {
	case model.PolicyOnlyNewer, model.PolicyReinstall, model.PolicyAllowDowngrade:
	default:
		return nil, errors.New("unknown update policy")
	}
	var reqs []ota.Request
	for _, id := range ids {
		if l := strings.TrimSpace(r.PostFormValue("language_" + strconv.FormatInt(int64(id), 10))); l != "" && !slices.ContainsFunc(languageOptions, func(o struct{ Value, Label string }) bool { return o.Value == strings.ToUpper(l) }) {
			return nil, fmt.Errorf("unknown language %q", l)
		}
		d, err := h.d.Inventory.Get(r.Context(), id)
		if err != nil {
			return nil, fmt.Errorf("device %d: %w", id, err)
		}
		q := ota.Request{DeviceID: id, Release: release, Custom: custom, Delivery: delivery, Variant: variant, AllowVariantChange: variant != "",
			Policy: policy, SkipBackup: skipBackup(r),
			SkipPowerRestore: skipPower, RestoreRelays: restoreRelays,
			// The chip build of a device that has two (ESP32-S2) is chosen per device in the preview; the engine
			// refuses a build that does not belong to the device.
			Build: strings.TrimSpace(r.PostFormValue("build_" + strconv.FormatInt(int64(id), 10))),
			// The language build of a device that only MQTT reaches cannot be read; the person says it (D10).
			Language: strings.TrimSpace(r.PostFormValue("language_" + strconv.FormatInt(int64(id), 10)))}
		if q.Variant == "" && (d.Identity.Variant == "minimal" || d.Identity.Variant == "safeboot") {
			q.Variant, q.AllowVariantChange = "tasmota", true
		}
		reqs = append(reqs, q)
	}
	return reqs, nil
}

type previewRow struct {
	Device   model.Device
	Item     ota.PreviewItem
	Go       bool
	Note     string
	From, To string // the version now and after the update
	Size     int64  // the file's size, 0 unknown
	SkipPwr  bool   // the restore is turned off for this update
	Push     bool   // "push where possible" was chosen: the row says its delivery
	// BuildOptions are the builds to choose from when the device is refused only because its chip has
	// two builds (ESP32-S2); Build is the one the user chose (the row then goes ahead).
	BuildOptions []buildOption
	Build        string
	// LanguageChoice: the row waits for the language build of the device (it cannot be read over MQTT);
	// Language is the one chosen, "" before the choice.
	LanguageChoice bool
	Language       string
}

// languageOptions are the upstream language builds a person can say a device runs ("" is English).
var languageOptions = []struct{ Value, Label string }{
	{"EN", "English"}, {"AF", "Afrikaans"}, {"AD", "Catalan"}, {"BG", "Bulgarian"}, {"BR", "Portuguese (Brazil)"},
	{"CN", "Chinese (simplified)"}, {"TW", "Chinese (traditional)"}, {"CZ", "Czech"}, {"NL", "Dutch"}, {"FR", "French"},
	{"FY", "Frisian"}, {"DE", "German"}, {"GR", "Greek"}, {"HE", "Hebrew"}, {"HU", "Hungarian"}, {"IT", "Italian"},
	{"KO", "Korean"}, {"PL", "Polish"}, {"PT", "Portuguese"}, {"RO", "Romanian"}, {"RU", "Russian"}, {"SK", "Slovak"},
	{"ES", "Spanish"}, {"SE", "Swedish"}, {"TR", "Turkish"}, {"UK", "Ukrainian"}, {"VN", "Vietnamese"},
}

// LanguageName is the field name of the select that chooses the language build of the device.
func (r previewRow) LanguageName() string {
	return "language_" + strconv.FormatInt(int64(r.Device.ID), 10)
}

// PathText says how the update reaches the device when that is not the plain HTTP download: over MQTT,
// with the reason, or the reason there is no path. "" for the usual HTTP rows.
func (r previewRow) PathText() string {
	switch r.Item.Path {
	case transport.DownloadMQTT:
		return "Over MQTT: " + r.Item.PathText
	case transport.None:
		return r.Item.PathText
	case transport.HTTP, transport.MQTT, transport.DownloadHTTP, transport.PushHTTP, transport.PushSafeboot, transport.PushMQTT:
	}
	return ""
}

// buildOption is one chip build the user may choose for a device.
type buildOption struct {
	Value, Label, Text string
}

// buildTexts say, in one line each, what the S2 builds are for (Tasmota's own docs: s2 is the
// standard build, s2cdc is for boards whose USB port is the chip's native USB with the CDC console).
var buildTexts = map[string][2]string{
	"s2":    {"s2: standard", "USB serial via a UART bridge"},
	"s2cdc": {"s2cdc", "for boards whose USB port is the chip's native USB (CDC console)"},
}

func buildOptionsFor(id model.Identity) []buildOption {
	var out []buildOption
	for _, b := range fwselect.BuildOptions(id) {
		t := buildTexts[b]
		out = append(out, buildOption{Value: b, Label: t[0], Text: t[1]})
	}
	return out
}

// BuildName is the field name of the radio group that chooses the build of the device.
func (r previewRow) BuildName() string { return "build_" + strconv.FormatInt(int64(r.Device.ID), 10) }

// DeliveryText says how a device that is updated gets its file when push
// where possible was chosen; "" for the download delivery.
func (r previewRow) DeliveryText() string {
	switch {
	case !r.Go || !r.Push:
		return ""
	case r.Item.Delivery == model.DeliveryPush && r.Item.ViaSafeboot:
		return "Push upload through safeboot (the device restarts into its safeboot partition first)."
	case r.Item.Delivery == model.DeliveryPush:
		return "Push upload."
	case r.Item.DeliveryNote != "":
		return "Downloads instead: " + r.Item.DeliveryNote + "."
	}
	return "Downloads instead."
}

// FileText is the file a device gets and its size: "tasmota32.bin · 1.9 MB".
func (r previewRow) FileText() string {
	if r.Size > 0 {
		return r.Item.Filename + " · " + sizeText(r.Size)
	}
	return r.Item.Filename
}

// Tone is the badge colour of a device that is not updated: refused is a block, skip is neutral.
func (r previewRow) Tone() ui.Tone {
	if r.Item.Decision == "refuse" {
		return ui.ToneDanger
	}
	return ui.ToneNeutral
}

// PowerText says what the power restore would do for a device that is updated, "" when there is
// nothing worth a line (nothing is ON, or the restore is off for the whole update).
func (r previewRow) PowerText() string {
	if !r.Go || r.SkipPwr {
		return ""
	}
	var out []string
	switch {
	case r.Item.PowerUnreadable:
		out = append(out, "Relay state unreadable: the update would stop; turn the restore off to update anyway.")
	case r.Item.PowerRestore != "":
		out = append(out, "Relay "+r.Item.PowerRestore+" on again if the update loses its state.")
	}
	if r.Item.PowerSkipped != "" {
		out = append(out, "Relay "+r.Item.PowerSkipped+" left alone (PulseTime, Interlock, shutter or unclear state).")
	}
	if r.Item.PowerRestore != "" && r.Item.PowerRelaysKept != "" {
		out = append(out, "Relay "+r.Item.PowerRelaysKept+" not restored (relays keep their state).")
	}
	return strings.Join(out, " ")
}

// Upgrades is the number of devices the update would start on.
func (p previewPage) Upgrades() int {
	n := 0
	for _, r := range p.Rows {
		if r.Go {
			n++
		}
	}
	return n
}

// Refused counts the rows the engine refuses: they are blocked, not just left alone. A device
// that waits for the user's build choice is counted by ChoosingBuilds instead.
func (p previewPage) Refused() int {
	n := 0
	for _, r := range p.Rows {
		if r.Item.Decision == "refuse" && len(r.BuildOptions) == 0 && !r.LanguageChoice {
			n++
		}
	}
	return n
}

// ChoosingLanguages counts the devices that wait for the user to say their language build.
func (p previewPage) ChoosingLanguages() int {
	n := 0
	for _, r := range p.Rows {
		if r.LanguageChoice {
			n++
		}
	}
	return n
}

// ChoosingBuilds counts the devices that wait for the user to choose a chip build.
func (p previewPage) ChoosingBuilds() int {
	n := 0
	for _, r := range p.Rows {
		if len(r.BuildOptions) > 0 {
			n++
		}
	}
	return n
}

// Skips counts the devices that need nothing (already at the target); a refused device is not one of them.
func (p previewPage) Skips() int {
	return len(p.Rows) - p.Upgrades() - p.Refused() - p.ChoosingBuilds() - p.ChoosingLanguages()
}

type previewPage struct {
	ReleaseText string // the release as the user chose it, "custom: label (version)" for an upload
	Form        updateForm
	Rows        []previewRow
	Builds      map[string]string // device id -> the build the user chose (rows that go ahead only)
	Languages   map[string]string // device id -> the language build the user said (rows that go ahead only)
	IDs         []string          // the devices the update starts on
	Picked      []string          // every device the user picked (the way back to the picker)
	Error       string

	Base     string
	Parallel int      // devices updated at a time
	Files    []string // the different files the devices get, in row order
	Bytes    int64    // what is sent to the devices in total (0 unknown)
	FwDir    string   // the directory the devices fetch from (pull), "" unknown
}

// Delivery says how the files reach the devices: the text before the address the devices fetch
// from, that address ("" for a push upload or when it is unknown), and the text after it.
func (p previewPage) Delivery() (lead, url, tail string) {
	pushed, pulled := 0, 0
	for _, r := range p.Rows {
		switch {
		case !r.Go:
		case r.Item.Delivery == model.DeliveryPush:
			pushed++
		default:
			pulled++
		}
	}
	switch {
	case pushed > 0 && pulled == 0:
		lead = "TasmoFleet uploads each file to its device"
	case pushed > 0 && p.FwDir != "":
		lead, url = "TasmoFleet uploads the file to "+plural(pushed, "device")+"; the others fetch theirs from ", p.FwDir
	case pushed > 0:
		lead = "TasmoFleet uploads the file to " + plural(pushed, "device") + "; the others fetch theirs from TasmoFleet"
	case p.FwDir != "":
		lead, url = "Each device fetches its file from ", p.FwDir
	default:
		lead = "Each device fetches its file from TasmoFleet"
	}
	tail = " · " + strconv.Itoa(p.Parallel) + " at a time"
	if p.Form.SkipBackup {
		return lead, url, tail + " · no backup"
	}
	return lead, url, tail + " · backup first"
}

func (h *handler) updatePreview(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	ids := h.formIDs(r)
	redo := func(msg string) {
		f, err := h.updateFormData(r, ids, "", deviceFilter{})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		f.Error = msg
		f.Release = r.PostFormValue("release")
		f.formChoices(r)
		f.RelOpts = checked(f.Release, "", f.RelOpts)
		for i := range f.OlderRels {
			f.OlderRels[i].Checked = f.OlderRels[i].Value == f.Release
		}
		h.showUpdateForm(w, r, http.StatusUnprocessableEntity, f)
	}
	if len(ids) == 0 {
		redo("Select at least one device.")
		return
	}
	reqs, err := h.requests(r, ids)
	if err != nil {
		redo(err.Error())
		return
	}
	items, err := h.d.OTA.Preview(r.Context(), reqs)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p := previewPage{Form: updateForm{
		Release: strings.TrimSpace(r.PostFormValue("release")), Policy: string(reqs[0].Policy), Variant: r.PostFormValue("variant"),
		SkipBackup: skipBackup(r), PowerAfter: r.PostFormValue("power_after"),
		Delivery: r.PostFormValue("delivery"),
	}}
	p.ReleaseText = "Tasmota " + reqs[0].Release
	if reqs[0].Custom != "" {
		p.ReleaseText = h.customText(r.Context(), reqs[0].Custom, reqs[0].Release)
	}
	byID := map[model.DeviceID]ota.Request{}
	for _, q := range reqs {
		byID[q.DeviceID] = q
		p.Picked = append(p.Picked, strconv.FormatInt(int64(q.DeviceID), 10))
	}
	p.Base, p.Parallel = httpBase(r), h.otaParallel()
	files := map[string]bool{}
	for _, it := range items {
		d, _ := h.d.Inventory.Get(r.Context(), it.DeviceID) // a vanished device shows as an empty name
		q := byID[it.DeviceID]
		row := previewRow{Device: d, Item: it, Go: it.Decision != "refuse" && it.Decision != "skip",
			From: d.Identity.Version, To: q.Release, SkipPwr: q.SkipPowerRestore, Push: q.Delivery == model.DeliveryPush}
		if it.BuildChoice {
			row.BuildOptions = buildOptionsFor(d.Identity)
		}
		row.Build = q.Build
		row.LanguageChoice, row.Language = it.LanguageChoice, q.Language
		if row.Go && q.Language != "" {
			if p.Languages == nil {
				p.Languages = map[string]string{}
			}
			p.Languages[strconv.FormatInt(int64(it.DeviceID), 10)] = q.Language
		}
		if row.Go && q.Build != "" {
			row.Note = "Build " + q.Build + ", chosen by you."
			if p.Builds == nil {
				p.Builds = map[string]string{}
			}
			p.Builds[strconv.FormatInt(int64(it.DeviceID), 10)] = q.Build
		} else if q.AllowVariantChange && r.PostFormValue("variant") == "" {
			row.Note = "Runs " + d.Identity.Variant + ": installs the default tasmota build."
		}
		if row.Go {
			p.IDs = append(p.IDs, strconv.FormatInt(int64(it.DeviceID), 10))
			if q.Custom == "" && it.Filename != "" {
				// The preview has just put the file into the cache, so this is a lookup.
				if a, err := h.d.Firmware.Ensure(r.Context(), q.Release, it.Filename); err == nil {
					row.Size = a.Size
					if it.Delivery != model.DeliveryPush {
						p.FwDir = strings.TrimSuffix(h.d.Firmware.URLFor(a), a.Filename)
					}
				}
			}
			p.Bytes += row.Size
			if it.Filename != "" && !files[it.Filename] {
				files[it.Filename] = true
				p.Files = append(p.Files, it.Filename)
			}
		}
		p.Rows = append(p.Rows, row)
	}
	v := h.view(r, "New update", "updates", nil)
	h.show(w, r, http.StatusOK, "updates_preview", v, updatePreviewPage(p, v.CSRF))
}

func (h *handler) updateSubmit(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	reqs, err := h.requests(r, h.formIDs(r))
	if err == nil && len(reqs) == 0 {
		err = errors.New("nothing to update")
	}
	var id model.BatchID
	if err == nil {
		id, err = h.d.OTA.Submit(r.Context(), actor(r), reqs)
	}
	var refused *ota.RefusedError
	switch {
	case errors.As(err, &refused):
		var b strings.Builder
		for _, x := range refused.Refused {
			d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
			fmt.Fprintf(&b, "%s: %s. ", d.Name, x.Reason)
		}
		v := h.view(r, "Update refused", "updates", "Nothing was started. "+b.String())
		h.message(w, r, http.StatusConflict, v)
	case errors.Is(err, devlock.ErrBusy):
		h.message(w, r, http.StatusConflict, h.view(r, "Update not started", "updates",
			"Nothing was started. Another operation on a selected device is running ("+busyHolder(err)+")."))
	case err != nil:
		h.message(w, r, http.StatusUnprocessableEntity, h.view(r, "Update not started", "updates", err.Error()))
	default:
		h.recordFirmware(r, "transaction.start", "transaction:"+strconv.FormatInt(int64(id), 10), map[string]any{
			"devices": len(reqs), "release": reqs[0].Release, "variant": reqs[0].Variant, "policy": string(reqs[0].Policy),
		}, "ok")
		h.auditCustomUse(r, id, reqs)
		redirect(w, r, "/transactions/"+strconv.FormatInt(int64(id), 10))
	}
}

type jobView struct {
	Job         model.Job
	Device      string
	FromVersion string // the device's version now (a queued job has not detected it yet)
	Active      bool
	Base        string
	CanRetry    bool   // a finished failed or cancelled job: offer "Retry"
	Step        int    // 0..jobSteps, drives the progress bar
	StepLabel   string // what the device is doing now
	Log         []logLine
}

// logLine is one line of the per-device update log (a persisted job event).
type logLine struct {
	At   time.Time
	Text string
	Err  bool // the event reports a failure
}

type batchPage struct {
	Batch  model.BatchID
	Jobs   []jobView
	Active bool
	Base   string
	// summary bar
	Total, Done, Running, Queued, Failed int
	RetryOf                              model.BatchID   // this transaction retries that one (0: not a retry)
	RetriedBy                            []model.BatchID // retries of this transaction, oldest first
	Canceled                             int
	CancelPending                        int    // still running, but a cancel was requested: it stops after the current step
	By                                   string // who started it
}

func (h *handler) jobViews(r *http.Request, f store.JobFilter) ([]jobView, bool, error) {
	jobs, err := h.d.Jobs.ListJobs(r.Context(), f)
	if err != nil {
		return nil, false, err
	}
	names, versions := map[model.DeviceID]string{}, map[model.DeviceID]string{}
	if devs, err := h.d.Inventory.List(r.Context()); err == nil {
		for _, d := range devs {
			names[d.ID], versions[d.ID] = d.Name, d.Identity.Version
		}
	}
	var out []jobView
	any := false
	for _, j := range jobs {
		v := jobView{Job: j, Device: names[j.DeviceID], FromVersion: versions[j.DeviceID], Active: !j.State.Terminal(), Base: httpBase(r)}
		v.Step, v.StepLabel = jobStep(j.State, j.Stage)
		switch {
		case j.IsPolicy():
			v.Step, v.StepLabel = policyStep(j.State)
		case j.IsCommand():
			v.Step, v.StepLabel = commandStep(j.State)
		case j.IsBackup():
			v.Step, v.StepLabel = backupStep(j.State)
		}
		v.CanRetry = j.State == model.JobFailed || j.State == model.JobCancelled
		if v.Active && j.CancelRequested {
			v.StepLabel += ", cancel requested" // a flash cannot be stopped: the job ends after this step
		}
		if evs, err := h.d.Jobs.JobEvents(r.Context(), j.ID); err == nil {
			for i, e := range evs {
				v.Log = append(v.Log, logLine{At: e.At, Text: eventText(e, j, h.pr, eventStageMinimal(evs, i, j)), Err: errEvents[e.Event] || e.To == model.JobFailed})
			}
		}
		any = any || v.Active
		out = append(out, v)
	}
	return out, any, nil
}

func (h *handler) batchID(w http.ResponseWriter, r *http.Request) (model.BatchID, bool) {
	n, err := strconv.ParseInt(r.PathValue("batch"), 10, 64)
	if err != nil || n <= 0 {
		h.notFoundBatch(w, r)
		return 0, false
	}
	return model.BatchID(n), true
}

func (h *handler) batchData(w http.ResponseWriter, r *http.Request) (batchPage, bool) {
	id, ok := h.batchID(w, r)
	if !ok {
		return batchPage{}, false
	}
	jobs, active, err := h.jobViews(r, store.JobFilter{BatchID: id})
	if err != nil {
		h.fail(w, r, err)
		return batchPage{}, false
	}
	if len(jobs) == 0 {
		h.notFoundBatch(w, r)
		return batchPage{}, false
	}
	p := batchPage{Batch: id, Jobs: jobs, Active: active, Base: httpBase(r), Total: len(jobs)}
	for _, j := range jobs {
		if j.Active && j.Job.CancelRequested {
			p.CancelPending++
		}
		switch j.Job.State { //nolint:exhaustive // every other state counts as running
		case model.JobSucceeded:
			p.Done++
		case model.JobFailed:
			p.Failed++
		case model.JobCancelled:
			p.Canceled++
		case model.JobQueued:
			p.Queued++
		default:
			p.Running++
		}
	}
	p.By = jobs[0].Job.CreatedBy
	if h.d.Batches != nil {
		if b, err := h.d.Batches.GetBatch(r.Context(), id); err == nil {
			p.RetryOf = b.RetryOf
		}
		if rs, err := h.d.Batches.BatchRetries(r.Context(), id); err == nil {
			for _, b := range rs {
				p.RetriedBy = append(p.RetriedBy, b.ID)
			}
		}
	}
	return p, true
}

// batchLive is the transaction page's body for its own refresh (job events, every 3 s while
// it runs), with the same device's log open (?job=).
func (h *handler) batchLive(w http.ResponseWriter, r *http.Request) {
	p, ok := h.txnPageData(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := txnBody(p).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "render transaction", "err", err)
	}
}

func (h *handler) batchPage(w http.ResponseWriter, r *http.Request) {
	p, ok := h.txnPageData(w, r)
	if !ok {
		return
	}
	nav := "transactions"
	if p.Policy {
		nav = "policy" // started from the policy page: Devices / Policy / TX-…
	}
	v := h.view(r, p.ID, nav, nil)
	v.Live = true
	p.CSRF = v.CSRF
	h.show(w, r, http.StatusOK, "update_batch", v, batchPageView(p))
}

// txnPageData builds one transaction's page from its jobs.
func (h *handler) txnPageData(w http.ResponseWriter, r *http.Request) (txnPage, bool) {
	b, ok := h.batchData(w, r)
	if !ok {
		return txnPage{}, false
	}
	// devices in the order they were added to the transaction (the store lists the newest first)
	sort.SliceStable(b.Jobs, func(i, j int) bool { return b.Jobs[i].Job.ID < b.Jobs[j].Job.ID })
	first := b.Jobs[0].Job
	p := txnPage{Base: b.Base, Batch: b.Batch, ID: txID(b.Batch), Title: txTitle(first, b.Total), Policy: first.IsPolicy(), Active: b.Active,
		Total: b.Total, Done: b.Done, Running: b.Running, Queued: b.Queued, Failed: b.Failed, Canceled: b.Canceled, CancelPending: b.CancelPending,
		RetryOf: b.RetryOf, RetriedBy: b.RetriedBy}
	p.State, p.StateTone = txResult(b.Done, b.Running, b.Queued, b.Failed, b.Canceled)
	p.Bar = ui.TxnData{OK: b.Done, Failed: b.Failed, Active: b.Running, Total: b.Total}
	p.RetryN = b.Failed
	if !b.Active {
		p.RetryN += b.Canceled
	}
	p.CancelText = cancelText(b.Queued)
	who := h.pr.Text(b.By)
	switch {
	case p.Policy:
		changes := 0
		for _, j := range b.Jobs {
			changes += len(commandLines(j.Job.Commands))
		}
		p.Title = "Make compliant"
		p.Sub = "started from Policy by " + who + " · " + plural(b.Total, "device") + " · " + plural(changes, "change")
	case first.IsUpdate():
		par := first.BatchParallel
		if par == 0 {
			par = h.otaParallel()
		}
		p.Sub = "started by " + who + " · " + strconv.Itoa(par) + " at a time"
		if first.SkipBackup {
			p.Sub += " · no backup"
		} else {
			p.Sub += " · backup first"
		}
	default:
		p.Sub = "started by " + who
		if first.BatchParallel > 0 {
			p.Sub += " · " + strconv.Itoa(first.BatchParallel) + " at a time"
		}
	}
	sel := h.selectedJob(r, b.Jobs)
	p.LiveURL = b.Base + "/transactions/" + bid(b.Batch) + "/live"
	if sel != nil {
		p.LiveURL += "?job=" + jid(sel.Job.ID)
	}
	for _, j := range b.Jobs {
		if p.Policy {
			p.Cards = append(p.Cards, fixCardOf(j, b.Base))
			continue
		}
		row := txnRow{JobID: j.Job.ID, Name: j.Device, Href: b.Base + "/transactions/" + bid(b.Batch) + "?job=" + jid(j.Job.ID), Selected: sel != nil && sel.Job.ID == j.Job.ID}
		row.Badge, row.Tone = jobBadge(j.Job)
		if j.Job.IsUpdate() {
			row.Upload = j.Job.Delivery == model.DeliveryPush
			row.Build = j.Job.TargetBuild
			row.From, row.To = j.Job.IdentityBefore.Version, j.Job.TargetRelease
			if row.From == "" {
				row.From = j.FromVersion
			}
		} else {
			row.To = txTarget(j.Job)
		}
		if j.Active && j.Job.State != model.JobQueued {
			row.Bar, row.Pct = true, j.Step*100/jobSteps
		}
		p.Rows = append(p.Rows, row)
	}
	if sel != nil && !p.Policy {
		s := &txnSel{JobID: sel.Job.ID, DeviceID: sel.Job.DeviceID, Name: sel.Device,
			CanCancel: sel.Active && !sel.Job.CancelRequested, CanRetry: sel.CanRetry && !b.Active}
		s.Log = append(s.Log, sel.Log...)
		if sel.Job.LastError != "" {
			s.Log = append(s.Log, logLine{At: sel.Job.UpdatedAt, Text: "Error: " + sel.Job.LastError, Err: true})
		}
		p.Sel = s
	}
	return p, true
}

// cancelText is what "Cancel remaining" does, in the dialog and on its page without JavaScript.
func cancelText(queued int) string {
	return "Devices that are already flashing finish their update, the others stop after their current step. The " +
		plural(queued, "queued device") + " " + map[bool]string{true: "is", false: "are"}[queued == 1] + " skipped."
}

// selectedJob is the device whose log is open: the one named by ?job=, else the first failed
// one, else the first that is running, else the first.
func (h *handler) selectedJob(r *http.Request, jobs []jobView) *jobView {
	if len(jobs) == 0 {
		return nil
	}
	if n, err := strconv.ParseInt(r.URL.Query().Get("job"), 10, 64); err == nil {
		for i := range jobs {
			if int64(jobs[i].Job.ID) == n {
				return &jobs[i]
			}
		}
	}
	for _, pick := range []func(j jobView) bool{
		func(j jobView) bool { return j.Job.State == model.JobFailed },
		func(j jobView) bool { return j.Active && j.Job.State != model.JobQueued },
	} {
		for i := range jobs {
			if pick(jobs[i]) {
				return &jobs[i]
			}
		}
	}
	return &jobs[0]
}

// commandLines are the non-empty lines of a policy job's commands.
func commandLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// fixCardOf is a "Make compliant" device: its commands with the job's state, and why it failed.
func fixCardOf(j jobView, base string) fixCard {
	c := fixCard{Name: j.Device, Href: base + "/devices/" + strconv.FormatInt(int64(j.Job.DeviceID), 10)}
	lbl, tone := changeBadge(j.Job.State)
	c.Tone = tone
	if j.Job.State == model.JobSucceeded {
		c.Tone = ui.ToneOK
	}
	for _, cmd := range commandLines(j.Job.Commands) {
		c.Changes = append(c.Changes, fixChange{Text: cmdpolicy.Redacted(cmd), Label: lbl, Tone: tone})
	}
	if j.Job.State == model.JobFailed {
		c.Reason = humanReason(j.Job.LastError)
	}
	return c
}

func (h *handler) batchCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := h.batchID(w, r)
	if !ok {
		return
	}
	switch err := h.d.OTA.CancelBatch(r.Context(), actor(r), id); {
	case err == nil, errors.Is(err, ota.ErrNotActive):
	case errors.Is(err, store.ErrNotFound):
		h.fail(w, r, err)
		return
	default:
		// Some jobs could not be cancelled; the others were. The batch page
		// shows the resulting states.
		h.log.WarnContext(r.Context(), "cancel batch", "batch", id, "err", err)
	}
	redirect(w, r, "/transactions/"+strconv.FormatInt(int64(id), 10))
}

func (h *handler) jobCancel(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || n <= 0 {
		h.notFound(w, r)
		return
	}
	switch err := h.d.OTA.Cancel(r.Context(), actor(r), model.JobID(n)); {
	case errors.Is(err, ota.ErrNotActive):
		http.Error(w, "job is not active", http.StatusConflict)
	case errors.Is(err, store.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.fail(w, r, err)
	default:
		if j, err := h.d.Jobs.GetJob(r.Context(), model.JobID(n)); err == nil {
			redirect(w, r, "/transactions/"+strconv.FormatInt(int64(j.BatchID), 10))
			return
		}
		redirect(w, r, "/transactions")
	}
}

// txView is one row of the transactions list: a batch of per-device update
// jobs. It is not atomic: every device job succeeds or fails on its own, and
// nothing is rolled back, so the outcome says how many worked.
type txView struct {
	ID       model.BatchID
	By       string
	Started  time.Time
	Release  string
	Total    int
	Done     int
	Running  int
	Queued   int
	Failed   int
	Canceled int
	Outcome  string
	Tone     string
	RetryOf  model.BatchID
	Update   bool      // a firmware update (not a command, backup or policy run)
	Title    string    // what the transaction does ("Update to Tasmota 14.3.0")
	first    model.Job // the first job seen, for the title
}

// txOutcome words the result of a transaction.
func txOutcome(total, done, running, queued, failed, canceled int) (text, tone string) {
	switch {
	case running+queued > 0:
		return "Running", "info"
	case done == total:
		return "Completed", "ok"
	case done > 0:
		return fmt.Sprintf("Partially completed (%d of %d)", done, total), "warn"
	case canceled == total:
		return "Cancelled", "neutral"
	}
	return "Failed", "bad"
}

// groupTransactions folds jobs into one row per transaction, in the order the
// batches first appear. Shared by the transactions list and the dashboard, so
// both count and word a transaction the same way.
func groupTransactions(jobs []model.Job) []txView {
	var order []model.BatchID
	by := map[model.BatchID]*txView{}
	for _, j := range jobs {
		t, ok := by[j.BatchID]
		if !ok {
			t = &txView{ID: j.BatchID, By: j.CreatedBy, Started: j.CreatedAt, Release: txTarget(j), Update: !j.IsCommand() && !j.IsPolicy() && !j.IsBackup(), first: j}
			by[j.BatchID] = t
			order = append(order, j.BatchID)
		}
		t.Total++
		if j.CreatedAt.Before(t.Started) {
			t.Started = j.CreatedAt
		}
		switch j.State { //nolint:exhaustive // every other state counts as running
		case model.JobSucceeded:
			t.Done++
		case model.JobFailed:
			t.Failed++
		case model.JobCancelled:
			t.Canceled++
		case model.JobQueued:
			t.Queued++
		default:
			t.Running++
		}
	}
	out := make([]txView, 0, len(order))
	for _, id := range order {
		t := by[id]
		t.Outcome, t.Tone = txOutcome(t.Total, t.Done, t.Running, t.Queued, t.Failed, t.Canceled)
		t.Title = txTitle(t.first, t.Total)
		out = append(out, *t)
	}
	return out
}

// txTarget is what a transaction puts on its devices: the release (the build
// variant differs per device and is not part of the answer), or "Make compliant".
func txTarget(j model.Job) string {
	if j.IsCommand() {
		return "Send " + cmdpolicy.Redacted(strings.TrimSpace(j.Commands)) // never the raw text: it may hold a secret
	}
	if j.IsPolicy() || j.IsBackup() {
		return j.Target()
	}
	return j.TargetRelease
}

// Summary is the one-line state of a transaction: "2 of 3 succeeded, 1 failed".
func (t txView) Summary() string {
	s := fmt.Sprintf("%d of %d succeeded", t.Done, t.Total)
	for _, x := range []struct {
		n    int
		word string
	}{{t.Failed, "failed"}, {t.Canceled, "cancelled"}, {t.Running, "running"}, {t.Queued, "waiting"}} {
		if x.n > 0 {
			s += fmt.Sprintf(", %d %s", x.n, x.word)
		}
	}
	return s
}

func (h *handler) transactionsPage(w http.ResponseWriter, r *http.Request) {
	filter := store.JobFilter{Limit: 500}
	p := txListView{Base: httpBase(r), CanStart: h.d.OTA != nil}
	p.Self = p.Base + "/transactions"
	if raw := r.URL.Query().Get("device"); raw != "" { // the device page's "All transactions"
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			h.notFoundDevice(w, r)
			return
		}
		d, err := h.d.Inventory.Get(r.Context(), model.DeviceID(id))
		if err != nil {
			h.notFoundDevice(w, r)
			return
		}
		filter.DeviceID, p.Device, p.Self = d.ID, d.Name, p.Self+"?device="+raw
	}
	jobs, _, err := h.jobViews(r, filter)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	raw := make([]model.Job, len(jobs))
	for i, j := range jobs {
		raw[i] = j.Job
	}
	now := h.d.Clock.Now()
	items := groupTransactions(raw)
	sort.SliceStable(items, func(a, b int) bool { return items[a].ID > items[b].ID }) // newest first
	for _, t := range items {
		href := p.Base + "/transactions/" + bid(t.ID)
		res, tone := txResult(t.Done, t.Running, t.Queued, t.Failed, t.Canceled)
		if t.Running+t.Queued > 0 {
			p.Running = append(p.Running, ui.TxnData{ID: txID(t.ID), Title: t.Title, State: res, StateTone: tone, Summary: t.RunSummary(),
				Href: href, OK: t.Done, Failed: t.Failed, Active: t.Running, Total: t.Total})
		}
		p.History = append(p.History, txHistRow{ID: txID(t.ID), Title: t.Title, Devices: plural(t.Total, "device"), Result: res, Tone: tone,
			By: h.pr.Text(t.By), When: ui.StampDayText(now, t.Started), At: t.Started.Format(time.RFC3339), Href: href})
	}
	v := h.view(r, "Updates", "transactions", nil) // the phone header names the group, as the prototype does
	v.Live = true
	h.show(w, r, http.StatusOK, "transactions", v, transactionsPage(p))
}

// RunSummary is the line under a running transaction's bar: "5 of 12 done · 1 failed · 3 in progress".
func (t txView) RunSummary() string {
	s := fmt.Sprintf("%d of %d done", t.Done, t.Total)
	if t.Failed > 0 {
		s += fmt.Sprintf(" · %d failed", t.Failed)
	}
	if t.Running > 0 {
		s += fmt.Sprintf(" · %d in progress", t.Running)
	}
	return s
}

type retryPage struct {
	Batch   model.BatchID
	Devices []jobView
	All     bool // no device was picked: every failed or cancelled one
	Policy  bool // a "Make compliant" transaction
	Command bool // a "Send command" transaction
	Backup  bool // a "Back up" transaction
	// Consequence and Token: a command transaction with destructive or lock-out
	// commands asks for a new confirmation on this page.
	Consequence string
	Token       string
	Error       string
}

// retryConsequences joins the consequences of the stored commands of a batch.
func retryConsequences(jobs []jobView) string {
	seen := map[string]bool{}
	var out []string
	for _, j := range jobs {
		if v, err := cmdpolicy.Classify(strings.TrimSpace(j.Job.Commands)); err == nil && v.Confirm != "" && !seen[v.Confirm] {
			seen[v.Confirm] = true
			out = append(out, v.Confirm)
		}
	}
	return strings.Join(out, " ")
}

// batchRetryForm is the confirmation step of a retry: it lists what would be
// retried and says that everything is planned afresh from the devices' current
// state. Nothing is created by this GET.
func (h *handler) batchRetryForm(w http.ResponseWriter, r *http.Request) {
	p, ok := h.batchData(w, r)
	if !ok {
		return
	}
	pick := map[model.DeviceID]bool{}
	for _, id := range h.formIDs(r) {
		pick[id] = true
	}
	page := retryPage{Batch: p.Batch, All: len(pick) == 0, Policy: len(p.Jobs) > 0 && p.Jobs[0].Job.IsPolicy(), Command: len(p.Jobs) > 0 && p.Jobs[0].Job.IsCommand(), Backup: len(p.Jobs) > 0 && p.Jobs[0].Job.IsBackup()}
	if page.Command || page.Backup {
		// A command or backup transaction is retried by an admin only, as that admin, and
		// asks again when it holds a destructive command.
		if pr, _ := auth.FromContext(r.Context()); pr.Role < model.RoleAdmin {
			h.message(w, r, http.StatusForbidden, h.view(r, "Retry refused", "transactions", "Only an admin can retry a command or backup transaction."))
			return
		}
		if page.Consequence = retryConsequences(p.Jobs); page.Consequence != "" {
			page.Token = h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), "retry:"+strconv.FormatInt(int64(p.Batch), 10), nil)
		}
	}
	for _, j := range p.Jobs {
		if j.CanRetry && (len(pick) == 0 || pick[j.Job.DeviceID]) {
			page.Devices = append(page.Devices, j)
		}
	}
	if p.Active {
		page.Error = "This transaction is still running. Retry it when every device has finished."
	} else if len(page.Devices) == 0 {
		page.Error = "No failed or cancelled device to retry."
	}
	v := h.view(r, fmt.Sprintf("Retry transaction %d", p.Batch), "transactions", nil)
	v.Crumbs = []ui.Crumb{{Label: "Updates", Path: "/transactions"}, {Label: txID(p.Batch), Path: "/transactions/" + bid(p.Batch)}, {Label: "Retry"}}
	h.show(w, r, http.StatusOK, "transaction_retry", v, retryPageView(h.retryViewOf(v, page)))
}

// retryViewOf is the retry page's data: the sentences per kind and one row per device.
func (h *handler) retryViewOf(v view, page retryPage) retryView {
	rv := retryView{
		Base: v.Base, CSRF: v.CSRF, Batch: bid(page.Batch), Error: page.Error, Consequence: page.Consequence, Token: page.Token,
		Title: "Retry device", Count: plural(len(page.Devices), "device"),
	}
	if page.All {
		rv.Title = "Retry failed devices"
	}
	rv.Lead = "A new transaction retries " + rv.Count + "."
	switch {
	case page.Command:
		rv.Info = fmt.Sprintf("It sends the same command as transaction #%d again, as you, if the command policy still allows it for your role.", page.Batch)
	case page.Backup:
		rv.Info = fmt.Sprintf("It takes a new backup of each device, as you. A backup changes nothing on the device; a device that is offline now is left out of the retry (transaction #%d).", page.Batch)
	case page.Policy:
		rv.Info = fmt.Sprintf("It sends the same commands as transaction #%d again, saves them once per device and reads the values back.", page.Batch)
	default:
		rv.Info = fmt.Sprintf("It keeps the release and options of transaction #%d. Each device is planned afresh; one already at the target is left out.", page.Batch)
	}
	for _, d := range page.Devices {
		rv.Rows = append(rv.Rows, retryRow{
			ID: devID(d.Job.DeviceID), Name: d.Device, Target: d.Job.Target(),
			Tone: uiToneClass(d.Job.State), State: label(d.Job.State), Hint: h.hint(d.Job.State),
			Reason: humanReason(d.Job.LastError), ReasonTip: tip(d.Job.LastError),
		})
	}
	return rv
}

// batchRetry creates the retry transaction (the engine plans it afresh).
func (h *handler) batchRetry(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	id, ok := h.batchID(w, r)
	if !ok {
		return
	}
	if jobs, _, jerr := h.jobViews(r, store.JobFilter{BatchID: id}); jerr == nil && len(jobs) > 0 && (jobs[0].Job.IsCommand() || jobs[0].Job.IsBackup()) {
		if pr, _ := auth.FromContext(r.Context()); pr.Role < model.RoleAdmin {
			h.message(w, r, http.StatusForbidden, h.view(r, "Retry refused", "transactions", "Only an admin can retry a command or backup transaction."))
			return
		}
		if retryConsequences(jobs) != "" {
			if _, ok := h.cmds.take(h.d.Clock.Now(), r.PostFormValue("token"), h.bindingOf(r)); !ok {
				h.message(w, r, http.StatusConflict, h.view(r, "Retry not started", "transactions", "This retry sends a destructive command again. Open the retry page and confirm it first."))
				return
			}
		}
	}
	pr, _ := auth.FromContext(r.Context())
	res, err := h.d.OTA.SubmitRetry(ota.WithActorKind(r.Context(), pr.Method), actor(r), id, h.formIDs(r))
	var refused *ota.RefusedError
	switch {
	case err == nil:
		redirect(w, r, "/transactions/"+strconv.FormatInt(int64(res.Batch), 10))
	case errors.Is(err, store.ErrNotFound):
		h.notFoundBatch(w, r)
	case errors.Is(err, ota.ErrNotAllowed):
		h.message(w, r, http.StatusForbidden, h.view(r, "Retry refused", "transactions", "Only an active admin can retry a command transaction."))
	case errors.Is(err, ota.ErrBatchRunning):
		h.message(w, r, http.StatusConflict, h.view(r, "Retry not started", "transactions", "The transaction still has running jobs."))
	case errors.Is(err, ota.ErrNothingToRetry):
		text := "Nothing is left to retry."
		for _, s := range res.Skipped {
			d, _ := h.d.Inventory.Get(r.Context(), s.DeviceID) // a vanished device shows as an empty name
			text += " " + d.Name + ": " + humanReason(s.Reason) + "."
		}
		h.message(w, r, http.StatusOK, h.view(r, "Nothing to retry", "transactions", text))
	case errors.As(err, &refused):
		var b strings.Builder
		for _, x := range refused.Refused {
			d, _ := h.d.Inventory.Get(r.Context(), x.DeviceID) // a vanished device shows as an empty name
			fmt.Fprintf(&b, "%s: %s. ", d.Name, humanReason(x.Reason))
		}
		h.message(w, r, http.StatusConflict, h.view(r, "Retry refused", "transactions", "Nothing was started. "+b.String()))
	default:
		if status, msg := deviceErrorText(err); msg != "" {
			h.message(w, r, status, h.view(r, "Retry not started", "transactions", msg))
			return
		}
		h.fail(w, r, err)
	}
}
