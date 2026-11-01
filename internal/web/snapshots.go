package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/snapshot"
)

// SnapshotService is the database snapshots of System -> Snapshots (*snapshot.Manager implements
// it). Optional: without it that page and its routes are absent.
type SnapshotService interface {
	Export(ctx context.Context, withKey bool) (*snapshot.Export, error)
	List() ([]snapshot.Info, error)
	Open(name string) (*os.File, snapshot.Info, error)
	Delete(name string) error
	Status() snapshot.Status
	Dir() string
}

// snapPath is the System -> Snapshots page; the stored snapshots' downloads and deletes live below it.
const snapPath = sysPath + "/snapshots"

// onSnapshots says whether the request is for the Snapshots page or something below it, so the shared
// settings handlers answer with that page (its save, its step-up) and not with the Settings page.
func onSnapshots(r *http.Request) bool {
	return r.URL.Path == snapPath || strings.HasPrefix(r.URL.Path, snapPath+"/")
}

func (h *handler) registerSnapshots(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Snapshots == nil {
		return
	}
	route("GET "+snapPath, adm, h.sysPage)
	route("POST "+snapPath, adm, h.sysSave)
	route("POST "+snapPath+"/stepup", adm, h.sysStepUpPost)
	route("POST "+sysPath+"/snapshot", adm, h.snapDownload)
	route("GET "+snapPath+"/{name}/download", adm, h.snapStoredDownload)
	route("POST "+snapPath+"/{name}/download", adm, h.snapStoredDownload) // the confirmation of a proxy-header sign-in
	route("GET "+snapPath+"/{name}/delete", adm, h.snapDeleteConfirm)
	route("POST "+snapPath+"/{name}/delete", adm, h.snapDelete)
}

// snapAckText is the checkbox a proxy-header sign-in ticks instead of a password: it has nothing
// to confirm itself with (sysStepUp), but the file holds every user's password hash and the
// encrypted device and broker secrets, so the download is never one click.
const snapAckText = "I understand this file holds secrets."

// snapIsHeader says whether the sign-in is a proxy header.
func snapIsHeader(r *http.Request) bool {
	p, _ := auth.FromContext(r.Context())
	return p.Method == "header"
}

// snapStepUp asks for the password (local account) or a new sign-in at the identity provider
// before a snapshot leaves the server; same confirmation as for the sign-in settings (five
// minutes per session, package auth). A proxy-header principal cannot do that, so it confirms on
// a page with a checkbox (ack=1 in a CSRF-protected POST). It answers the page and returns false
// when the download must not go ahead yet. action is the POST path (without the base) the
// confirmation posts to, fields its hidden values.
func (h *handler) snapStepUp(w http.ResponseWriter, r *http.Request, what, action string, fields ...confirmField) bool {
	if snapIsHeader(r) {
		if r.Method == http.MethodPost && r.PostForm.Get("ack") == "1" {
			return true
		}
		h.renderActionConfirm(w, r, "snapshots", actionConfirm{
			Title: "Download a database snapshot?", Heading: "Download a database snapshot", Text: "The file holds every user's password hash and the encrypted device, broker and sign-in secrets. Keep it like a password. Your sign-in comes from the proxy, so it cannot be asked for a password; confirm instead.",
			Crumbs: []confirmLink{{"", "System"}, {snapPath, "Snapshots"}}, Last: "Download snapshot",
			Action: action, Fields: fields, Ack: snapAckText, Verb: "Download", CancelHref: snapPath, CancelLabel: "Cancel",
		})
		return false
	}
	if !h.needsStepUp(r) {
		return true
	}
	p := h.sysData(r, nil, nil)
	p.Active, p.NeedStepUp, p.StepUpWhat = sysSnapshotsSlug, true, what
	h.sysRender(w, r, http.StatusForbidden, p)
	return false
}

// snapKeyParam is the query value that carries the choice "Include secret.key" through a step-up: only
// the boolean, never a secret (#387).
const snapKeyParam = "dlkey"

// snapKeyChosen says whether the person chose to include secret.key: in the download POST that the
// step-up interrupted, or in the address the step-up returned to.
func snapKeyChosen(r *http.Request) bool {
	return r.URL.Query().Get(snapKeyParam) == "1" || r.PostForm.Get("include_key") == "1"
}

// snapDownload makes a consistent copy of the live database for one download; with include_key
// the response is a .tar of the database and secret.key. The copy exists only for the response.
func (h *handler) snapDownload(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	for k := range r.PostForm {
		if k != "csrf" && k != "include_key" && k != "ack" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}
	withKey := r.PostForm.Get("include_key") == "1"
	var fields []confirmField
	if withKey {
		fields = append(fields, confirmField{"include_key", "1"})
	}
	if !h.snapStepUp(w, r, "Downloading a snapshot of the database", sysPath+"/snapshot", fields...) {
		return
	}
	ex, err := h.d.Snapshots.Export(r.Context(), withKey)
	if err != nil {
		h.snapAudit(r, "snapshot.download", "failed: "+snapReason(err), map[string]any{"with_key": withKey})
		h.fail(w, r, err)
		return
	}
	defer ex.Close() //nolint:errcheck // the scratch copy is removed best effort; the response is out
	h.snapAudit(r, "snapshot.download", "ok", map[string]any{"with_key": withKey, "file": ex.Name, "bytes": ex.Size})
	snapHeaders(w, ex.Name, ex.ContentType)
	http.ServeContent(w, r, ex.Name, ex.Taken, ex.File)
}

// snapStoredDownload sends a stored snapshot. The name is looked up in the snapshots folder; it
// is never used as a path (package snapshot).
func (h *handler) snapStoredDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !snapshot.ValidName(name) {
		h.notFound(w, r)
		return
	}
	if r.Method == http.MethodPost {
		limitBody(w, r)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	}
	if !h.snapStepUp(w, r, "Downloading a snapshot of the database", snapPath+"/"+name+"/download") {
		return
	}
	f, in, err := h.d.Snapshots.Open(name)
	if errors.Is(err, snapshot.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer f.Close()                  //nolint:errcheck // read-only handle
	if r.Method != http.MethodHead { // a HEAD sends no file
		h.snapAudit(r, "snapshot.download", "ok", map[string]any{"with_key": false, "file": in.Name, "bytes": in.Size, "stored": true})
	}
	snapHeaders(w, in.Name, "application/vnd.sqlite3")
	http.ServeContent(w, r, in.Name, in.Taken, f)
}

// snapExists says whether name is a stored snapshot.
func (h *handler) snapExists(name string) bool {
	list, err := h.d.Snapshots.List()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(list, func(in snapshot.Info) bool { return in.Name == name })
}

// snapDeleteConfirm is the confirmation of Delete as a page (no JavaScript).
func (h *handler) snapDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !h.snapExists(name) {
		h.notFound(w, r)
		return
	}
	h.renderActionConfirm(w, r, "snapshots", actionConfirm{
		Title: "Delete this snapshot?", Heading: "Delete this snapshot", Text: snapDeleteText(name),
		Crumbs: []confirmLink{{"", "System"}, {snapPath, "Snapshots"}}, Last: "Delete snapshot",
		Action: snapPath + "/" + name + "/delete", Verb: "Delete snapshot", CancelHref: snapPath, CancelLabel: "Cancel",
	})
}

// snapDelete removes one stored snapshot. Like deleting a device backup it acts at once; the
// confirmation is the dialog on the button.
func (h *handler) snapDelete(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	name := r.PathValue("name")
	err := h.d.Snapshots.Delete(name)
	switch {
	case errors.Is(err, snapshot.ErrNotFound):
		h.notFound(w, r)
	case err != nil:
		h.snapAudit(r, "snapshot.delete", "failed: "+snapReason(err), map[string]any{"file": name})
		h.fail(w, r, err)
	default:
		h.snapAudit(r, "snapshot.delete", "ok", map[string]any{"file": name})
		redirect(w, r, snapPath+"?ok=snapshot-deleted")
	}
}

// snapHeaders marks the response as a file the browser saves and never caches or sniffs.
func snapHeaders(w http.ResponseWriter, name, ctype string) {
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
}

// snapAudit records a download or delete. Nothing of the file's content, and no key, is in it.
func (h *handler) snapAudit(r *http.Request, action, result string, detail map[string]any) {
	if h.d.Recorder == nil {
		return
	}
	p, _ := auth.FromContext(r.Context())
	if p.Method != "" {
		detail["method"] = p.Method // how the admin signed in: local, oidc or header
	}
	d := ""
	if b, err := json.Marshal(detail); err == nil {
		d = string(b)
	}
	h.d.Recorder.Record(r.Context(), model.AuditEvent{
		At: h.d.Clock.Now(), Actor: actor(r), Action: action, Target: "database", Detail: d, Result: result,
		RemoteAddr: httpx.ClientIP(r.Context()).String(),
	})
}

func snapReason(err error) string {
	s := err.Error()
	return s[:min(len(s), 120)]
}

// sysDB is the Snapshots page: the download, the stored snapshots and the status of the schedule.
type sysDB struct {
	// KeepKey: the person ticked "Include secret.key" before the step-up; the box stays ticked (#387).
	KeepKey bool
	// FocusDownload: the page is the return from the step-up; the Download button takes the focus, so
	// Enter or Space finishes what the person started.
	FocusDownload bool
	LiveSize      string // about how big a download is; empty when unknown
	Hint          string // the line next to the download button
	Dir           string // the snapshots folder, as the server sees it
	DataDir       string // the data directory
	Files         []sysSnapshot
	Kept          int // SNAPSHOT_KEEP
	On            bool
	Status        sysSnapStatus
	CanDelete     bool
	ListErr       string // the snapshot folder cannot be listed: shown instead of the list (#399)
}

// sysSnapshot is one stored snapshot.
type sysSnapshot struct {
	Name, Size string
	Taken      time.Time
	Schema     int
	Latest     bool
	Download   string
	Delete     string
}

// sysSnapStatus is the last scheduled attempt as the cards show it. State is "none" (schedule on,
// nothing yet), "ok" or "fail".
type sysSnapStatus struct {
	State   string
	Taken   time.Time // the last good snapshot
	Attempt time.Time // the last failed attempt
	Next    time.Time // the next run; zero when it cannot be told
	File    string
	Reason  string
	Count   int
}

// sysDBOf reads the stored snapshots and the schedule's state for the Snapshots page.
func (h *handler) sysDBOf(r *http.Request, base string) (*sysDB, error) {
	s := h.d.Snapshots
	list, err := s.List()
	db := &sysDB{Dir: s.Dir(), DataDir: filepath.Dir(s.Dir()), CanDelete: true}
	if err != nil {
		// A broken snapshot folder must not take the whole Settings page down: the card says what is
		// wrong, and the scheduled snapshots fail with the same reason (#399).
		list = nil
		db.ListErr = "The snapshot folder cannot be read: " + snapshot.ReasonOf(err) + "."
		if errors.Is(err, syscall.ENOTDIR) {
			db.ListErr = "The snapshot folder is not a directory: " + s.Dir() + " is a file. Move or delete it; scheduled snapshots fail until then."
		}
	}
	db.KeepKey = snapKeyChosen(r)
	db.FocusDownload = r.Method == http.MethodGet && snapKeyChosen(r) // the return from the step-up, not the locked page itself
	if h.d.Database != nil {
		if info, err := h.d.Database.DBInfo(r.Context()); err == nil && info.Size > 0 {
			db.LiveSize = humanBytes(info.Size)
		}
	}
	db.Hint = "A consistent copy of the live database, made while TasmoFleet runs"
	if db.LiveSize != "" {
		db.Hint += " (about " + db.LiveSize + ")"
	}
	db.Hint += ". You are asked to confirm first (password, or your identity provider for SSO)."
	if snapIsHeader(r) {
		db.Hint = "A consistent copy of the live database, made while TasmoFleet runs. You are asked to confirm that the file holds secrets."
	}
	db.Kept, _ = strconv.Atoi(h.storedValue("SNAPSHOT_KEEP"))
	every, _ := time.ParseDuration(h.storedValue("SNAPSHOT_INTERVAL"))
	db.On = every > 0
	for i, in := range list {
		db.Files = append(db.Files, sysSnapshot{
			Name: in.Name, Size: humanBytes(in.Size), Taken: in.Taken, Schema: in.Schema, Latest: i == 0,
			Download: base + snapPath + "/" + in.Name + "/download",
			Delete:   base + snapPath + "/" + in.Name + "/delete",
		})
	}
	st := s.Status()
	db.Status = sysSnapStatus{State: "none", Taken: st.LastGood, Count: len(list)}
	switch {
	case st.Failed:
		db.Status.State, db.Status.Attempt, db.Status.File, db.Status.Reason = "fail", st.Attempted, st.File, st.Reason
		if every > 0 {
			db.Status.Next = st.Attempted.Add(every)
		}
	case !st.LastGood.IsZero():
		db.Status.State = "ok"
		if every > 0 {
			db.Status.Next = st.LastGood.Add(every)
		}
	}
	if db.Status.Next.Before(h.d.Clock.Now()) {
		db.Status.Next = time.Time{} // overdue: the schedule is about to run, no time to promise
	}
	return db, nil
}
