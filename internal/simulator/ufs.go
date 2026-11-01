package simulator

import (
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The flash file system of a device (Tasmota's xdrv_50_filesystem.ino),
// modelled on live captures in testdata/fixtures/ufs/ (2026-11-01, v16.0.0).
// Only the commands and routes TasmoFleet uses exist: UfsType, Ufs, UfsSize,
// UfsFree, UfsList(2), GET /ufsd, POST /ufsu. UfsDelete, UfsRename, UfsRun
// and the file manager's delete/edit links are not simulated.
//
// The SD card is not simulated either: the simulator has one flash file
// system, and dir=2 (flash) is what a client sends.

// Faults of the file system routes. After is "" or "ufsd" / "ufsu".
const (
	// FaultUfsStall: GET /ufsd?download= sends the headers and half of the
	// file, then the device hangs for Duration (0 = until the client gives up).
	FaultUfsStall = "ufs_stall"
	// FaultUfsUploadError: POST /ufsu answers "Upload Failed" and writes nothing.
	FaultUfsUploadError = "ufs_upload_error"
)

const (
	fsTypeLittleFS = 3
	// fsBaseKB is what the hidden settings files of the capture occupy, so a
	// device with only the captured _persist.json reports Free 208 of 320 kB.
	fsBaseKB   = 108
	fsBlockKB  = 4 // LittleFS allocates 4 kB blocks
	fsNameMax  = 49
	fsTimeForm = "2006-01-02T15:04:05"
)

// FSFile is a seeded file of a device's file system.
type FSFile struct {
	Data     []byte
	Modified time.Time // zero: the moment the device was created
}

type simFile struct {
	data []byte
	mod  time.Time
}

// simFS is the flash file system plus the web server's global upload state.
type simFS struct {
	sizeKB int
	files  map[string]*simFile // absolute path -> file
	dirs   map[string]bool     // absolute folder paths (explicit and implied)
	// ufsPath is ufs_path: the folder a following POST /ufsu writes to. Every
	// GET /ufsd resets it to "/" and a download of a folder sets it.
	ufsPath string
}

// defaultFSFiles is what an ESP32 profile starts with when the spec names no
// files: a Berry script, a library folder, and a TLS key (fake content).
func defaultFSFiles(t time.Time) map[string]FSFile {
	return map[string]FSFile{
		"/autoexec.be":   {Data: []byte("print('autoexec')\n"), Modified: t},
		"/lib/helper.be": {Data: []byte("# helper\n"), Modified: t},
		"/tlskey":        {Data: []byte("-----BEGIN FAKE KEY-----\nnot-a-real-key\n-----END FAKE KEY-----\n"), Modified: t},
	}
}

func newSimFS(spec DeviceSpec, esp32 bool, minimal bool, now time.Time) *simFS {
	if spec.NoFS || minimal {
		return nil
	}
	files := spec.Files
	if files == nil && esp32 {
		files = defaultFSFiles(now)
	}
	if files == nil && len(spec.Dirs) == 0 {
		return nil // an ESP8266 build without USE_UFILESYS, unless the spec gives it files
	}
	fs := &simFS{sizeKB: spec.FSSizeKB, files: map[string]*simFile{}, dirs: map[string]bool{"/": true}, ufsPath: "/"}
	if fs.sizeKB <= 0 {
		fs.sizeKB = 320
	}
	for p, f := range files {
		mod := f.Modified
		if mod.IsZero() {
			mod = now
		}
		fs.put(p, f.Data, mod)
	}
	for _, d := range spec.Dirs {
		fs.mkdirAll(d)
	}
	return fs
}

func (fs *simFS) mkdirAll(p string) {
	for p != "/" && p != "." && p != "" {
		fs.dirs[p] = true
		p = path.Dir(p)
	}
}

func (fs *simFS) put(p string, data []byte, mod time.Time) {
	fs.mkdirAll(path.Dir(p))
	fs.files[p] = &simFile{data: append([]byte(nil), data...), mod: mod}
}

func (fs *simFS) usedKB() int {
	used := fsBaseKB
	for _, f := range fs.files {
		used += (len(f.data) + fsBlockKB*1024 - 1) / (fsBlockKB * 1024) * fsBlockKB
	}
	return used
}

func (fs *simFS) freeKB() int { return max(fs.sizeKB-fs.usedKB(), 0) }

func cleanFolder(p string) string {
	if p == "" {
		return "/"
	}
	if len(p) > 1 {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// listLocked is UFSListDir: the direct children of dir. Dot entries are
// hidden unless all is set (UfsList2). A missing folder is simply empty.
func (fs *simFS) list(dir string, all bool) (names []string, rows map[string]string) {
	dir = cleanFolder(dir)
	rows = map[string]string{}
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	add := func(p string, isDir bool, f *simFile) {
		if !strings.HasPrefix(p, prefix) || p == dir {
			return
		}
		rest := p[len(prefix):]
		if rest == "" || strings.Contains(rest, "/") || (!all && strings.HasPrefix(rest, ".")) {
			return
		}
		if isDir {
			rows[rest] = "[" + jsonString(rest) + ",\"\",0]"
		} else {
			rows[rest] = "[" + jsonString(rest) + "," + jsonString(f.mod.Format(fsTimeForm)) + "," + strconv.Itoa(len(f.data)) + "]"
		}
	}
	for p := range fs.dirs {
		add(p, true, nil)
	}
	for p, f := range fs.files {
		add(p, false, f)
	}
	for n := range rows {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, rows
}

// ufsCmdLocked answers the Ufs* commands. ok is false for anything else, and
// for a device without a file system (Tasmota does not know the commands then).
func (d *simDevice) ufsCmdLocked(base string, idx int, hasIdx bool, data string) ([]byte, bool) {
	fs := d.fs
	if fs == nil {
		return nil, false
	}
	switch base {
	case "ufstype":
		return []byte(`{"UfsType":` + strconv.Itoa(fsTypeLittleFS) + `}`), true
	case "ufs":
		return []byte(`{"Ufs":{"Type":` + strconv.Itoa(fsTypeLittleFS) + `,"Size":` + strconv.Itoa(fs.sizeKB) + `,"Free":` + strconv.Itoa(fs.freeKB()) + `}}`), true
	case "ufssize":
		return []byte(`{"UfsSize":` + strconv.Itoa(fs.sizeKB) + `}`), true
	case "ufsfree":
		return []byte(`{"UfsFree":` + strconv.Itoa(fs.freeKB()) + `}`), true
	case "ufslist":
		// "UfsList" and "UfsList2" (dot files too); UfsList sets ufs_path like the real command.
		dir := "/"
		if data != "" {
			dir = data
		}
		fs.ufsPath = dir
		names, rows := fs.list(dir, hasIdx && idx == 2)
		if len(names) == 0 {
			return []byte(`{"UfsList":"Done"}`), true
		}
		parts := make([]string, len(names))
		for i, n := range names {
			parts[i] = rows[n]
		}
		return []byte(`{"UfsList":[` + strings.Join(parts, ",") + `]}`), true
	}
	return nil, false
}

func (d *simDevice) fsPageBody() []byte {
	b, err := os.ReadFile(filepath.Join(d.f.o.FixturesDir, "ufs", "esp32-littlefs", "ufsd-page.html")) //nolint:gosec // fixtures dir is trusted test input
	if err != nil {
		return []byte("<html><body>Manage File system</body></html>")
	}
	return b
}

func (d *simDevice) notFound(w http.ResponseWriter, r *http.Request) {
	d.setHeaders(w, "", "text/plain")
	d.writeBody(w, http.StatusNotFound, []byte("File Not Found\n\nURI: "+r.URL.Path+"\nMethod: "+r.Method+"\nArguments: "+strconv.Itoa(len(r.URL.Query()))+"\n"))
}

// serveUfsd models UfsDirectory: GET /ufsd[?dir=][&download=]. A file streams
// as an attachment, a folder (and no download argument) answers the HTML file
// manager, and a missing file makes the handler return WITHOUT sending
// anything: the connection just closes (live capture, 2026-11-01).
func (d *simDevice) serveUfsd(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	fs := d.fs
	if d.minimalLocked() {
		fs = nil // minimal and safeboot images have no /ufsd
	}
	d.mu.Unlock()
	if fs == nil || r.Method != http.MethodGet {
		d.notFound(w, r)
		return
	}
	q := r.URL.Query()
	dl, hasDl := q["download"]
	rec := "GET /ufsd"
	if v := q.Get("dir"); v != "" {
		rec += " dir=" + v
	}
	if hasDl {
		rec += " download=" + dl[0]
	}
	d.record(rec)
	d.mu.Lock()
	fs.ufsPath = "/"
	d.uploadType = uploadUFS // the GET leaves Web.upload_file_type = UPL_UFSFILE
	var file *simFile
	if hasDl {
		p := dl[0]
		if f, ok := fs.files[p]; ok {
			file = f
		} else if fs.dirs[cleanFolder(p)] {
			fs.ufsPath = p
		} else {
			d.mu.Unlock()
			drop(w) // handler returns, nothing is sent
			return
		}
	}
	stall := d.faultsForLocked("ufsd", FaultUfsStall)
	var data []byte
	if file != nil {
		data = append([]byte(nil), file.data...)
	}
	d.mu.Unlock()
	if file == nil {
		d.setHeaders(w, "", "text/html")
		d.writeBody(w, http.StatusOK, d.fsPageBody())
		return
	}
	// Real headers of a file: no Server or Cache headers, but a length.
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", "attachment; filename="+path.Base(dl[0]))
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	if len(stall) > 0 {
		_, _ = w.Write(data[:len(data)/2]) //nolint:gosec // simulated device reply
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-time.After(stallDuration(stall[0])):
		case <-r.Context().Done():
		case <-d.ctx.Done():
		}
		drop(w)
		return
	}
	_, _ = w.Write(data) //nolint:gosec // simulated device reply
}

func stallDuration(f Fault) time.Duration {
	if f.Duration > 0 {
		return f.Duration
	}
	return time.Hour // until the client gives up
}

// serveUfsu models HandleUploadUFSLoop/HandleUploadUFSDone: POST /ufsu?fsz=
// with a multipart field "ufsu". The file goes to ufs_path + "/" + filename,
// after the old file was removed. ufs_path is whatever the last GET /ufsd
// left. Before v16 a POST without a preceding GET writes nothing yet answers
// "Successful" (the upload type is unset), like the firmware upload.
func (d *simDevice) serveUfsu(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	fs := d.fs
	if d.minimalLocked() {
		fs = nil
	}
	d.mu.Unlock()
	if fs == nil || r.Method != http.MethodPost {
		d.notFound(w, r)
		return
	}
	d.mu.Lock()
	armed := d.uploadType == uploadUFS // HandleUploadUFSDone leaves the type set
	failFault := len(d.faultsForLocked("ufsu", FaultUfsUploadError)) > 0
	oldFW := !atLeastVersion(d.version, 16, 0)
	d.mu.Unlock()
	reply := func(ok bool, msg string) {
		d.setHeaders(w, "", "text/html")
		d.writeBody(w, http.StatusOK, uploadPage(ok, msg))
	}
	if oldFW && !armed {
		_, _ = io.Copy(io.Discard, r.Body)
		d.record("POST /ufsu (no upload type set)")
		reply(true, "")
		return
	}
	fsz, _ := strconv.Atoi(r.URL.Query().Get("fsz"))
	mr, err := r.MultipartReader()
	if err != nil {
		reply(false, "No file selected")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "ufsu" || part.FileName() == "" {
		reply(false, "No file selected")
		return
	}
	body, err := io.ReadAll(io.LimitReader(part, 16<<20))
	if err != nil {
		drop(w)
		return
	}
	name := path.Base(part.FileName())
	d.mu.Lock()
	ufsPath := fs.ufsPath
	d.mu.Unlock()
	// UfsUploadFileOpen: snprintf(npath, UFS_FILENAME_SIZE, "%s/%s", ufs_path, filename). The buffer
	// holds 49 characters; the rest is cut, and a root upload is "//name". The cut name is what
	// gets removed and written, which can be a different, existing file.
	npath := ufsPath + "/" + name
	if len(npath) > fsNameMax {
		npath = npath[:fsNameMax]
	}
	target := path.Clean(npath)
	folder := path.Dir(target)
	d.record("POST /ufsu " + target + " " + strconv.Itoa(len(body)) + " fsz=" + strconv.Itoa(fsz))
	d.mu.Lock()
	var msg string
	switch {
	case failFault:
		msg = "Upload failed. Enable logging 3"
	case fsz > fs.freeKB()*1024: // UPLOAD_FILE_START: upload_size > freeBytes
		msg = "File too large"
	case !fs.dirs[folder]:
		msg = "Not enough space" // UfsUploadFileOpen failed
	default:
		fs.put(target, body, time.Now())
	}
	d.mu.Unlock()
	if msg != "" {
		reply(false, msg)
		return
	}
	reply(true, "")
}

func atLeastVersion(v string, major, minor int) bool {
	fields := strings.SplitN(v, ".", 3)
	if len(fields) < 2 {
		return false
	}
	a, err1 := strconv.Atoi(fields[0])
	b, err2 := strconv.Atoi(fields[1])
	return err1 == nil && err2 == nil && (a > major || (a == major && b >= minor))
}
