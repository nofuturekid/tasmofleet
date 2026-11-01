package web

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/importer"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/tasmoadmin"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// ImportService is the TasmoAdmin importer (package importer).
type ImportService = importer.Service

// stepUpper is implemented by *auth.Service. Local admins re-enter their
// password, OIDC admins authenticate at the IdP again (POST /oidc/stepup).
// The system settings page uses it; the TasmoAdmin import does not
// (decision 2026-10-21, docs/security/web-import.md). A guard that cannot
// step up fails closed.
type stepUpper interface {
	SteppedUp(r *http.Request) bool
	StepUp(r *http.Request, password string) (ok, limited bool)
}

// sessionKeyer gives the login session of a request (empty without session).
type sessionKeyer interface{ SessionKey(r *http.Request) string }

const (
	importPlanTTL    = 10 * time.Minute
	importMaxPending = 4
	// 3 files of at most 1 MiB plus multipart framing.
	importMaxBody = 3*importer.MaxFileBytes + 64<<10
)

// pendingPlan holds a dry-run result between upload and confirmation, in
// memory only: it contains decrypted device passwords and must never be
// written to disk or rendered.
type pendingPlan struct {
	plan    importer.Plan
	binding string // session (or, for header principals, subject)
	expires time.Time
	timer   *time.Timer
}

type importPlans struct {
	mu  sync.Mutex
	m   map[string]pendingPlan
	ttl time.Duration // default importPlanTTL
}

// forget drops the passwords of a plan (best effort: Go strings cannot be
// wiped, but nothing keeps the plan reachable afterwards).
func forget(p pendingPlan) {
	if p.timer != nil {
		p.timer.Stop()
	}
	for i := range p.plan.Rows {
		p.plan.Rows[i].Device.HTTPPassword, p.plan.Rows[i].Device.MQTTPassword = "", ""
	}
}

// put stores plan for binding. A second upload of the same session replaces
// the first; when other sessions fill the store the new plan is refused
// instead of evicting someone else's.
func (p *importPlans) put(now time.Time, binding string, plan importer.Plan) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]pendingPlan{}
	}
	ttl := p.ttl
	if ttl == 0 {
		ttl = importPlanTTL
	}
	others := 0
	for k, v := range p.m {
		switch {
		case !now.Before(v.expires) || v.binding == binding:
			forget(v)
			delete(p.m, k)
		default:
			others++
		}
	}
	if others >= importMaxPending {
		return "", false
	}
	id := rand.Text()
	entry := pendingPlan{plan: plan, binding: binding, expires: now.Add(ttl)}
	// The janitor: a plan never outlives its TTL, even if nobody uploads or
	// applies again.
	entry.timer = time.AfterFunc(ttl, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if v, ok := p.m[id]; ok {
			forget(v)
			delete(p.m, id)
		}
	})
	p.m[id] = entry
	return id, true
}

func (p *importPlans) size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.m)
}

// take removes and returns the plan if it belongs to binding and is fresh.
func (p *importPlans) take(now time.Time, id, binding string) (importer.Plan, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[id]
	if !ok {
		return importer.Plan{}, false
	}
	if v.binding != binding {
		return importer.Plan{}, false // someone else's plan stays where it is
	}
	delete(p.m, id)
	if v.timer != nil {
		v.timer.Stop()
	}
	if !now.Before(v.expires) {
		forget(v)
		return importer.Plan{}, false
	}
	return v.plan, true
}

// bindingOf ties a plan to the login session; header principals have none and
// fall back to their subject.
func (h *handler) bindingOf(r *http.Request) string {
	if k, ok := h.d.Guard.(sessionKeyer); ok {
		if id := k.SessionKey(r); id != "" {
			return "session:" + id
		}
	}
	return "subject:" + actor(r)
}

func (h *handler) registerImport(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	route("POST /import/tasmoadmin", adm, h.importUpload)
	route("POST /import/tasmoadmin/apply", adm, h.importApply)
	route("GET /import/tasmoadmin/report.csv", adm, h.importReport)
}

// importBody is the drop zone with the files and, after a check, the plan or the result.
type importBody struct {
	Base, CSRF string
	Uploaded   []string // the file names of the last check, as uploaded
	Error      string
	Plan       *importPlanView
	Report     *importReportView
}

type importPlanView struct {
	Token                 string
	Rows                  []importRow
	Create, Merge, Reject int
	Warnings              []string
	Hint                  string
	Probe                 bool
}

// CanApply is false when the plan would write nothing (no row is created or merged): the page
// then says so instead of offering to apply it.
func (v importPlanView) CanApply() bool { return v.Create+v.Merge > 0 }

// importRow is one line of the plan or the result: a badge, the device and why.
type importRow struct {
	Label   string
	Tone    ui.Tone
	Name    string
	Address string
	Why     string
}

type importReportView struct {
	Summary string
	Rows    []importRow
	Hint    string
}

// planView turns an importer plan into the page's rows: Create, Merge, or Reject for a row the
// import refuses and for a line of devices.csv that gave no row at all.
func planView(plan importer.Plan) *importPlanView {
	v := &importPlanView{Warnings: plan.Warnings, Hint: plan.Hint, Probe: plan.Probe}
	for _, row := range plan.Rows {
		r := importRow{Name: row.Device.Name, Address: netaddr.Display(row.Device.Address, row.Device.Port, "http"), Why: row.Reason}
		switch row.Action {
		case importer.ActionCreate:
			v.Create++
			r.Label, r.Tone = "Create", ui.ToneOK
		case importer.ActionMerge:
			v.Merge++
			r.Label, r.Tone = "Merge", ui.ToneAccent
		default:
			v.Reject++
			r.Label, r.Tone = "Reject", ui.ToneDanger
		}
		if r.Why == "" {
			r.Why = "New device"
		}
		v.Rows = append(v.Rows, r)
	}
	for _, e := range plan.Errors {
		if rowWarning(e.Err) { // the row is planned anyway (port 80, no password): a warning, not a rejection
			v.Warnings = append(v.Warnings, e.Error())
			continue
		}
		v.Rows = append(v.Rows, importRow{Label: "Reject", Tone: ui.ToneDanger, Name: fmt.Sprintf("devices.csv line %d", e.Line), Why: capitalize(e.Err.Error())})
	}
	v.Reject += plan.Rejected
	if plan.ErrorsOmitted > 0 {
		v.Rows = append(v.Rows, importRow{Label: "Reject", Tone: ui.ToneDanger, Name: plural(plan.ErrorsOmitted, "more line") + " with errors", Why: "Not listed one by one."})
	}
	return v
}

// rowWarning tells the errors of a line that is still imported (the importer's rule).
func rowWarning(err error) bool {
	return errors.Is(err, tasmoadmin.ErrBadPort) || errors.Is(err, tasmoadmin.ErrEncryptedNoKey) || errors.Is(err, tasmoadmin.ErrDecrypt)
}

// reportView is the result after Apply: one sentence with the counts, a row per device.
func reportView(rep importer.Report) *importReportView {
	v := &importReportView{Hint: rep.Hint}
	var parts []string
	if rep.Created > 0 || rep.Updated == 0 {
		parts = append(parts, plural(rep.Created, "device")+" created")
	}
	if rep.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d merged into existing devices", rep.Updated))
	}
	if rep.Failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", rep.Failed))
	}
	if rep.Skipped > 0 {
		parts = append(parts, plural(rep.Skipped, "row")+" rejected")
	}
	if rep.Unprobed > 0 {
		parts = append(parts, fmt.Sprintf("%d not probed", rep.Unprobed))
	}
	v.Summary = joinAnd(parts) + "."
	for _, l := range rep.Lines {
		r := importRow{Name: l.Name, Address: netaddr.Display(l.Address, l.Port, "http"), Why: l.Detail}
		switch l.Status {
		case "created":
			r.Label, r.Tone = "Created", ui.ToneOK
			r.Why = withDefault(r.Why, "Added to Devices")
		case "updated":
			r.Label, r.Tone = "Merged", ui.ToneAccent
			r.Why = withDefault(r.Why, "Merged into the existing device")
		case "failed":
			r.Label, r.Tone = "Failed", ui.ToneDanger
		default:
			r.Label, r.Tone = "Rejected", ui.ToneDanger
		}
		v.Rows = append(v.Rows, r)
	}
	return v
}

// withDefault is s, or def when s is empty.
func withDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// joinAnd joins "a", "b" and "c" as "a, b and c".
func joinAnd(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// needsStepUp fails closed: only header principals are exempt.
func (h *handler) needsStepUp(r *http.Request) bool {
	p, _ := auth.FromContext(r.Context())
	if p.Method == "header" {
		return false
	}
	su, ok := h.d.Guard.(stepUpper)
	return !ok || !su.SteppedUp(r)
}

// importPage.StepUpKind values: how the prompt asks for the confirmation.
const (
	stepUpPassword = "password" // local account
	stepUpOIDC     = "oidc"     // re-authenticate at the IdP
)

func (h *handler) stepUpKind(r *http.Request) string {
	if p, _ := auth.FromContext(r.Context()); p.Method == "oidc" {
		return stepUpOIDC
	}
	return stepUpPassword
}

func (h *handler) importBodyOf(r *http.Request) importBody {
	v := h.view(r, "", "", nil)
	return importBody{Base: v.Base, CSRF: v.CSRF}
}

// importAnswer sends the box body (the drop zone with the plan, the result or the error):
// the upload and Apply replace it in place.
func (h *handler) importAnswer(w http.ResponseWriter, r *http.Request, status int, b importBody) {
	var buf bytes.Buffer
	if err := importBodyPart(b).Render(r.Context(), &buf); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes()) // #nosec G705 -- rendered by templ, which escapes every value; a failed write means the client went away
}

func (h *handler) importError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	b := h.importBodyOf(r)
	b.Error = msg
	h.importAnswer(w, r, status, b)
}

func (h *handler) auditImport(r *http.Request, action, result string) {
	if h.d.Recorder == nil {
		return
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{
		At: h.d.Clock.Now(), Actor: actor(r), Action: action, Result: result, RemoteAddr: httpx.ClientIP(r.Context()).String(),
	})
}

// auditImportPlan records who checked which files and what the plan holds.
// File names are clipped and stripped of control characters; contents, device
// rows and passwords are never part of the entry.
func (h *handler) auditImportPlan(r *http.Request, names map[string]string, rows int, v *importPlanView) {
	if h.d.Recorder == nil {
		return
	}
	clean := map[string]string{}
	for field, n := range names {
		n = strings.Map(func(c rune) rune {
			if c < ' ' || c == 0x7f {
				return -1
			}
			return c
		}, n)
		clean[field] = n[:min(len(n), 64)]
	}
	detail, err := json.Marshal(map[string]any{"files": clean, "rows": rows, "create": v.Create, "merge": v.Merge, "skip": v.Reject})
	if err != nil {
		return
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{
		At: h.d.Clock.Now(), Actor: actor(r), Action: "import.tasmoadmin.plan", Target: "inventory", Result: "ok",
		Detail: string(detail), RemoteAddr: httpx.ClientIP(r.Context()).String(),
	})
}

// Accepted content types per upload. Browsers label CSV/JSON/key files
// inconsistently, so generic types pass; the bytes are sniffed afterwards.
var importTypes = []string{"", "text/csv", "text/plain", "application/csv", "application/vnd.ms-excel", "application/json", "application/octet-stream"}

// uploadField sorts a file of the drop zone by its name: a .csv is devices.csv, a .json is
// MyConfig.json, anything else is the key file (.device-password.key).
func uploadField(filename string) string {
	switch n := strings.ToLower(filename); {
	case strings.HasSuffix(n, ".csv"):
		return "devices_csv"
	case strings.HasSuffix(n, ".json"):
		return "my_config"
	}
	return "key_file"
}

// readUpload reads the three optional file parts into memory. Nothing is
// written to disk (multipart.Reader streams, unlike ParseMultipartForm).
func readUpload(w http.ResponseWriter, r *http.Request) (files map[string][]byte, names map[string]string, status int, err error) {
	mt, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if perr != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, nil, http.StatusUnsupportedMediaType, errors.New("the upload must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, importMaxBody)
	mr := multipart.NewReader(r.Body, params["boundary"])
	files, names = map[string][]byte{}, map[string]string{}
	for parts := 0; ; parts++ {
		if parts > 6 {
			return nil, nil, http.StatusBadRequest, errors.New("too many form parts")
		}
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			return files, names, 0, nil
		}
		if perr != nil {
			var mbe *http.MaxBytesError
			if errors.As(perr, &mbe) {
				return nil, nil, http.StatusRequestEntityTooLarge, errors.New("the upload is too large")
			}
			return nil, nil, http.StatusBadRequest, errors.New("the upload could not be read")
		}
		name := part.FormName()
		if part.FileName() == "" {
			_ = part.Close() // plain field (csrf, probe): read from the header/URL instead
			continue
		}
		if name == "files" { // the drop zone's one chooser: the file name says which file it is
			name = uploadField(part.FileName())
			if _, dup := files[name]; dup {
				return nil, nil, http.StatusBadRequest, errors.New("choose one file of each kind: devices.csv, MyConfig.json and .device-password.key")
			}
		}
		if name != "devices_csv" && name != "my_config" && name != "key_file" {
			return nil, nil, http.StatusBadRequest, errors.New("unexpected file field")
		}
		if ct, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type")); !slices.Contains(importTypes, ct) {
			return nil, nil, http.StatusUnsupportedMediaType, fmt.Errorf("%s: content type %q is not accepted", name, ct)
		}
		b, rerr := io.ReadAll(io.LimitReader(part, importer.MaxFileBytes+1))
		if rerr != nil {
			var mbe *http.MaxBytesError
			if errors.As(rerr, &mbe) {
				return nil, nil, http.StatusRequestEntityTooLarge, errors.New("the upload is too large")
			}
			return nil, nil, http.StatusBadRequest, errors.New("the upload could not be read")
		}
		if len(b) > importer.MaxFileBytes {
			return nil, nil, http.StatusRequestEntityTooLarge, fmt.Errorf("%s is larger than 1 MiB", name)
		}
		if name != "key_file" || len(b) != 32 { // a raw 32 byte key is binary by nature
			if ct := http.DetectContentType(b); len(b) > 0 && !strings.HasPrefix(ct, "text/plain") {
				return nil, nil, http.StatusUnsupportedMediaType, fmt.Errorf("%s is not a text file", name)
			}
		}
		files[name], names[name] = b, part.FileName()
	}
}

func (h *handler) importUpload(w http.ResponseWriter, r *http.Request) {
	if h.d.Import == nil {
		h.notFound(w, r)
		return
	}
	files, names, status, err := readUpload(w, r)
	if err != nil {
		h.importError(w, r, status, err.Error())
		return
	}
	csv, ok := files["devices_csv"]
	if !ok || len(csv) == 0 {
		h.importError(w, r, http.StatusUnprocessableEntity, "devices.csv is required.")
		return
	}
	probe := r.URL.Query().Get("probe") != "0"
	var cfgR, keyR io.Reader
	if b := files["my_config"]; len(b) > 0 {
		cfgR = strings.NewReader(string(b))
	}
	if b := files["key_file"]; len(b) > 0 {
		keyR = strings.NewReader(string(b))
	}
	plan, err := h.d.Import.Plan(r.Context(), strings.NewReader(string(csv)), cfgR, keyR, probe)
	clear(files["key_file"])
	if err != nil {
		h.importError(w, r, http.StatusUnprocessableEntity, "Import failed: "+err.Error())
		return
	}
	v := planView(plan)
	h.auditImportPlan(r, names, len(plan.Rows), v)
	token, stored := h.plans.put(h.d.Clock.Now(), h.bindingOf(r), plan)
	if !stored {
		h.importError(w, r, http.StatusTooManyRequests, "Other imports are waiting for their confirmation. Try again in a few minutes.")
		return
	}
	v.Token = token
	b := h.importBodyOf(r)
	b.Plan, b.Uploaded = v, uploadedNames(names)
	h.importAnswer(w, r, http.StatusOK, b)
}

func (h *handler) importApply(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if h.d.Import == nil {
		h.notFound(w, r)
		return
	}
	plan, ok := h.plans.take(h.d.Clock.Now(), r.PostFormValue("plan"), h.bindingOf(r))
	if !ok {
		h.importError(w, r, http.StatusGone, "The plan expired or was already applied. Upload the files again.")
		return
	}
	rep, err := h.d.Import.Apply(r.Context(), actor(r), plan)
	for i := range plan.Rows {
		plan.Rows[i].Device.HTTPPassword, plan.Rows[i].Device.MQTTPassword = "", "" // best effort, see forget
	}
	if err != nil {
		h.log.ErrorContext(r.Context(), "import apply", "err", err)
	}
	b := h.importBodyOf(r)
	b.Report = reportView(rep)
	if err != nil {
		b.Error = "The import was interrupted; the report lists what was done."
	}
	h.reports.put(h.d.Clock.Now(), h.bindingOf(r), rep)
	h.importAnswer(w, r, http.StatusOK, b)
}

// uploadedNames lists the checked files in a fixed order (devices.csv first), as the drop zone
// names them; the names are the browser's, clipped.
func uploadedNames(names map[string]string) []string {
	var out []string
	for _, f := range []string{"devices_csv", "my_config", "key_file"} {
		if n := names[f]; n != "" {
			out = append(out, n[:min(len(n), 64)])
		}
	}
	return out
}

// importReports keeps the last result of each session for "Download report", in memory and
// only for importPlanTTL. A report holds names and addresses, never a password.
type importReports struct {
	mu sync.Mutex
	m  map[string]reportEntry
}

type reportEntry struct {
	rep     importer.Report
	expires time.Time
}

func (p *importReports) put(now time.Time, binding string, rep importer.Report) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]reportEntry{}
	}
	for k, v := range p.m {
		if !now.Before(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[binding] = reportEntry{rep: rep, expires: now.Add(importPlanTTL)}
}

func (p *importReports) get(now time.Time, binding string) (importer.Report, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[binding]
	if !ok || !now.Before(v.expires) {
		return importer.Report{}, false
	}
	return v.rep, true
}

// importReport is "Download report": the last result of this session as CSV.
func (h *handler) importReport(w http.ResponseWriter, r *http.Request) {
	rep, ok := h.reports.get(h.d.Clock.Now(), h.bindingOf(r))
	if !ok {
		h.notFound(w, r)
		return
	}
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"name", "address", "port", "status", "detail"}) // writes into memory
	for _, l := range rep.Lines {
		port := ""
		if l.Port != 0 {
			port = strconv.Itoa(l.Port)
		}
		_ = cw.Write([]string{csvCell(l.Name), csvCell(l.Address), port, l.Status, csvCell(l.Detail)})
	}
	cw.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="tasmofleet-import-report.csv"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes()) // a failed write means the client went away
}

// csvCell keeps a spreadsheet from running a cell as a formula: names come from the uploaded file.
func csvCell(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
