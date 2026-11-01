package tasmota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Flash file system types as Tasmota's UfsType reports them
// (xdrv_50_filesystem.ino: 0 none, 1 SD card, 2 FFat, 3 LittleFS).
const (
	FSNone     = 0
	FSSDCard   = 1
	FSFAT      = 2
	FSLittleFS = 3
)

// MinFilesFirmware is the first Tasmota release whose UfsList carries the
// modification time (v14.6.0). Older builds answer a different shape, so the
// file backup does not use them.
const MinFilesFirmware = "14.6.0"

// maxFSPath is UFS_FILENAME_SIZE (50) minus the terminating NUL: the longest
// path the device handles, leading slash included.
const maxFSPath = 49

// fileTimeout covers one file transfer. Tasmota streams about 150 kB/s, so
// the default 1 MiB cap needs about 7 s; the margin is for a busy device.
const fileTimeout = 60 * time.Second

// Errors of the FileSystem client, next to the shared ones.
var (
	// ErrBadPath: the path breaks the rules of CheckFSPath. Nothing was sent.
	ErrBadPath = errors.New("tasmota: invalid file system path")
	// ErrFileNotFound: the file does not exist. Tasmota's /ufsd handler
	// returns without a reply for a missing file, which looks like a dropped
	// link or a refused HTTP API; the client tells them apart with two probes
	// (the main page and a plain /ufsd) before it says "not found".
	ErrFileNotFound = errors.New("tasmota: file not found")
	// ErrFolderNotFound: the folder of an upload does not exist. Nothing was
	// written; the device cannot create folders over this API.
	ErrFolderNotFound = errors.New("tasmota: folder not found")
	// ErrNamesSkipped: ListFiles dropped entries whose names the client does
	// not accept. The returned entries are valid; see SkippedNamesError.
	ErrNamesSkipped = errors.New("tasmota: listed names skipped")
	// ErrNotAFile: /ufsd answered with its HTML page instead of the file, so
	// the path is a folder.
	ErrNotAFile = errors.New("tasmota: path is not a file")
	// ErrNotAFolder: the folder of an upload is a file on the device.
	ErrNotAFolder = errors.New("tasmota: path is not a folder")
	// ErrFileTooLarge: the file is larger than the cap the caller gave.
	// Nothing was read.
	ErrFileTooLarge = errors.New("tasmota: file larger than the limit")
)

// SkippedNamesError is the error of a ListFiles that dropped entries. The
// slice returned with it holds every other entry, so a caller that backs up
// best-effort uses both and reports Names as skipped; a caller that treats any
// error as failure fails safe. errors.Is(err, ErrNamesSkipped) matches.
type SkippedNamesError struct{ Names []string }

func (e *SkippedNamesError) Error() string {
	return ErrNamesSkipped.Error() + ": " + strconv.Itoa(len(e.Names)) + " entries (" + strings.Join(e.Names, ", ") + ")"
}

func (e *SkippedNamesError) Unwrap() error { return ErrNamesSkipped }

// FSInfo describes the flash file system of a device. The SD card is never
// used: its type and size are not reported.
type FSInfo struct {
	// Type is FSNone when the build has no flash file system (ESP8266 1 MB,
	// minimal and safeboot images answer UfsType 0 or "Unknown command"),
	// FSFAT or FSLittleFS otherwise.
	Type int
	// HasSD: an SD card is mounted too. Files are still read from and written
	// to flash only (dir=2).
	HasSD  bool
	SizeKB int
	FreeKB int
}

// HasFlash reports whether the device has a flash file system.
func (i FSInfo) HasFlash() bool { return i.Type == FSFAT || i.Type == FSLittleFS }

// FileEntry is one line of UfsList.
type FileEntry struct {
	Name string // base name, never a path
	Dir  bool   // directories come with an empty date
	Size int64
	// ModTime is the device's wall clock as reported (no zone information),
	// zero for directories and for a date the parser does not know.
	ModTime time.Time
}

// FileSystem is the file system part of the device protocol. The production
// client implements it next to Client; consumers type-assert or take it as a
// separate dependency so the many fakes of Client stay unchanged.
//
// Every call goes through the per-device lock. Paths start with a slash and
// obey CheckFSPath; they are checked before any request. Dot files are
// refused everywhere: UfsList hides them, and /.settings or /.drvset* must
// never be written through this API.
//
// Callers must gate on FilesSupported(version, info) before any call: a device
// with only an SD card has no flash file system, and the calls would then
// reach the SD card.
type FileSystem interface {
	// FSInfo asks UfsType and UfsSize/UfsFree. A device that does not know
	// the commands (no file system in the build) yields Type FSNone, nil.
	FSInfo(ctx context.Context, t Target) (FSInfo, error)
	// ListFiles lists one folder ("/" is the root): GET /ufsd?dir=2 selects
	// the flash file system, then UfsList runs, both under one lock hold (the
	// command reads the device's global file system selection). An empty
	// folder yields an empty slice, and so does a folder that does not exist
	// (the device answers "Done" for both). Entries with a name the client
	// does not accept are dropped: the entries come back together with a
	// *SkippedNamesError.
	ListFiles(ctx context.Context, t Target, dir string) ([]FileEntry, error)
	// DownloadFile fetches GET /ufsd?download=. max is the largest accepted
	// size (<= 0: the client's MaxResponse); a larger file fails with
	// ErrFileTooLarge before a body byte is read.
	DownloadFile(ctx context.Context, t Target, path string, max int64) ([]byte, error)
	// UploadFile writes data to path: GET /ufsd (selects folder and flash)
	// and POST /ufsu under one lock hold. The device deletes an existing
	// file first and creates no folders; a missing folder fails with
	// ErrFolderNotFound before anything is written. The device builds
	// "<folder>/<name>" in a 49 byte buffer and cuts longer names, so a path
	// whose folder (or "/" for the root), a slash and the file name exceed 49
	// bytes is refused with ErrBadPath (root names: at most 47 bytes).
	//
	// After any error once the POST started the state of the file is unknown:
	// the old file may already be deleted and the new content partial. The
	// device also says "Successful" after a short write, so after a nil error
	// the caller must verify the size with ListFiles.
	UploadFile(ctx context.Context, t Target, path string, data []byte) error
}

var _ FileSystem = (*httpClient)(nil)

// FilesSupported is the capability rule for the file backup: the firmware
// lists modification times (>= MinFilesFirmware) and the device has a flash
// file system. A version that cannot be read counts as unsupported.
func FilesSupported(fwVersion string, info FSInfo) bool {
	return info.HasFlash() && versionAtLeast(fwVersion, MinFilesFirmware)
}

// versionAtLeast compares the leading dotted numeric fields ("16.0.0",
// "14.6.0.2", "15.1.0-dev"). Unreadable input is never "at least".
func versionAtLeast(v, min string) bool {
	a, ok1 := numericFields(v)
	b, ok2 := numericFields(min)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return true
}

func numericFields(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && (v[end] == '.' || (v[end] >= '0' && v[end] <= '9')) {
		end++
	}
	parts := strings.Split(strings.TrimSuffix(v[:end], "."), ".")
	if len(parts) < 2 {
		return nil, false
	}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 1<<20 {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

// CheckFSPath validates a device path: leading slash, at most 49 bytes, valid
// UTF-8 without control characters or backslashes, no empty, "." or ".."
// segment, and no segment that starts with a dot (hidden settings files).
// It does not accept "/" (see checkFolder).
func CheckFSPath(p string) error {
	if p == "" || p[0] != '/' {
		return badPath("must start with /")
	}
	if len(p) > maxFSPath {
		return badPath("longer than 49 bytes")
	}
	if !utf8.ValidString(p) {
		return badPath("not valid UTF-8")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' || r == '"' {
			return badPath("control character, backslash or double quote")
		}
	}
	for _, seg := range strings.Split(p[1:], "/") {
		switch {
		case seg == "":
			return badPath("empty segment")
		case seg[0] == '.':
			return badPath("segment starts with a dot")
		case seg[0] == ' ' || seg[len(seg)-1] == ' ':
			return badPath("segment starts or ends with a space")
		}
	}
	return nil
}

// checkFolder is CheckFSPath that also accepts the root.
func checkFolder(p string) error {
	if p == "/" {
		return nil
	}
	return CheckFSPath(p)
}

func badPath(why string) error {
	return &deviceError{ErrBadPath, why}
}

// checkEntryName validates a name the device listed: one segment, no slash.
func checkEntryName(n string) error {
	if n == "" || strings.Contains(n, "/") {
		return badPath("listed name is empty or has a slash")
	}
	return CheckFSPath("/" + n)
}

func (c *httpClient) FSInfo(ctx context.Context, t Target) (FSInfo, error) {
	raw, err := c.Command(ctx, t, "UfsType")
	if err != nil {
		if errors.Is(err, ErrUnknownCommand) {
			return FSInfo{}, nil
		}
		return FSInfo{}, err
	}
	types, err := ufsNumbers(raw, "UfsType")
	if err != nil {
		return FSInfo{}, err
	}
	// One number: the only file system. Two: [primary, flash] with an SD
	// card as primary (xdrv_50 UFSType).
	var info FSInfo
	flash := 0
	switch len(types) {
	case 1:
		info.Type = types[0]
		if info.Type == FSSDCard {
			info.HasSD, info.Type = true, FSNone
		}
	case 2:
		info.HasSD = types[0] == FSSDCard || types[1] == FSSDCard
		if types[0] == FSSDCard {
			flash = 1
		}
		info.Type = types[flash]
	default:
		return FSInfo{}, badResponse("UfsType has %d entries", len(types))
	}
	if !info.HasFlash() {
		return FSInfo{Type: FSNone, HasSD: info.HasSD}, nil
	}
	if info.SizeKB, err = c.ufsKB(ctx, t, "UfsSize", flash); err != nil {
		return FSInfo{}, err
	}
	if info.FreeKB, err = c.ufsKB(ctx, t, "UfsFree", flash); err != nil {
		return FSInfo{}, err
	}
	return info, nil
}

func (c *httpClient) ufsKB(ctx context.Context, t Target, cmnd string, idx int) (int, error) {
	raw, err := c.Command(ctx, t, cmnd)
	if err != nil {
		return 0, err
	}
	v, err := ufsNumbers(raw, cmnd)
	if err != nil {
		return 0, err
	}
	if idx >= len(v) {
		return 0, badResponse("%s has no flash entry", cmnd)
	}
	return v[idx], nil
}

// ufsNumbers reads {"Key":n} or {"Key":[n,m]}.
func ufsNumbers(raw json.RawMessage, key string) ([]int, error) {
	m, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := classifyObject(m); err != nil {
		return nil, err
	}
	v, ok := m[key]
	if !ok {
		return nil, badResponse("missing %s", key)
	}
	var one int
	if json.Unmarshal(v, &one) == nil {
		return []int{one}, nil
	}
	var many []int
	if json.Unmarshal(v, &many) == nil && len(many) > 0 {
		return many, nil
	}
	return nil, badResponse("bad %s", key)
}

func (c *httpClient) ListFiles(ctx context.Context, t Target, dir string) ([]FileEntry, error) {
	if err := checkFolder(dir); err != nil {
		return nil, err
	}
	// UfsList reads the device's global file system selection (dfsp), which an
	// SD card or the file manager can have moved. The GET names flash (dir=2)
	// and nothing may come between it and the command.
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return nil, err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(statusTimeout+rootTimeout))
	defer cancel()
	if err := c.selectFlash(rctx, ctx, t); err != nil {
		return nil, err
	}
	raw, err := c.commandLocked(rctx, t, "UfsList "+dir)
	if err != nil {
		return nil, err
	}
	return parseUfsList(raw)
}

// selectFlash sends GET /ufsd?dir=2 (the page is discarded) inside a held slot.
func (c *httpClient) selectFlash(rctx, parent context.Context, t Target) error {
	resp, err := c.ufsGet(rctx, parent, t, url.Values{"dir": {"2"}}, ErrFolderNotFound, "file manager")
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body, nothing to recover
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.o.MaxResponse))
	return nil
}

// parseUfsList reads {"UfsList":[["name","date",size],...]} and the empty
// folder answer {"UfsList":"Done"}. Entries with a name that fails
// checkEntryName are dropped and reported in a *SkippedNamesError next to the
// valid entries.
func parseUfsList(raw json.RawMessage) ([]FileEntry, error) {
	m, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if err := classifyObject(m); err != nil {
		return nil, err
	}
	v, ok := m["UfsList"]
	if !ok {
		return nil, badResponse("missing UfsList")
	}
	var done string
	if json.Unmarshal(v, &done) == nil {
		if done == "Done" {
			return []FileEntry{}, nil
		}
		return nil, badResponse("UfsList answered %q", done)
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(stripControl(v), &rows); err != nil {
		return nil, badResponse("bad UfsList")
	}
	out := make([]FileEntry, 0, len(rows))
	var skipped []string
	for _, r := range rows {
		if len(r) != 3 {
			return nil, badResponse("UfsList entry has %d fields", len(r))
		}
		var name, date string
		var size int64
		if json.Unmarshal(r[0], &name) != nil || json.Unmarshal(r[1], &date) != nil || json.Unmarshal(r[2], &size) != nil || size < 0 {
			return nil, badResponse("bad UfsList entry")
		}
		if checkEntryName(name) != nil {
			skipped = append(skipped, name)
			continue
		}
		e := FileEntry{Name: name, Dir: date == "", Size: size}
		if !e.Dir {
			e.ModTime = parseDeviceTime(date)
		}
		out = append(out, e)
	}
	if len(skipped) > 0 {
		return out, &SkippedNamesError{Names: skipped}
	}
	return out, nil
}

// parseDeviceTime reads Tasmota's GetDT: "2026-10-31T12:34:56". Anything else
// yields the zero time.
func parseDeviceTime(s string) time.Time {
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts
		}
	}
	return time.Time{}
}

// ufsRequest performs GET /ufsd inside an acquired device slot. The caller
// closes the body.
func (c *httpClient) ufsRequest(rctx context.Context, t Target, query url.Values) (*http.Response, error) {
	u := url.URL{Scheme: "http", Host: t.hostPort(), Path: "/ufsd", RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &deviceError{ErrBadResponse, "invalid target"}
	}
	if t.user != "" || t.pass != "" {
		user := t.user
		if user == "" {
			user = "admin" // Tasmota's fixed web user
		}
		req.SetBasicAuth(user, t.pass)
	}
	setReferer(req, t)
	return c.hc.Do(req)
}

// ufsGet performs GET /ufsd inside an acquired device slot. A connection that
// closes without a byte is what the device does for a missing file or folder,
// but also what a dropped link and a refused HTTP API (SetOption128 0) look
// like. Two probes tell them apart: if the main page does not answer the link
// is gone (ErrUnreachable); if a plain /ufsd (no download argument) gets no
// answer either, the API is refused (ErrHTTPAPIRefused); otherwise the target
// is missing and the result is `missing` (ErrFileNotFound or
// ErrFolderNotFound). The caller closes the body.
func (c *httpClient) ufsGet(rctx, parent context.Context, t Target, query url.Values, missing error, what string) (*http.Response, error) {
	resp, err := c.ufsRequest(rctx, t, query)
	if err != nil {
		if errors.Is(err, io.EOF) && parent.Err() == nil {
			return nil, c.ufsNoReply(parent, t, query, missing, what)
		}
		return nil, c.mapErr(parent, t, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_ = resp.Body.Close() // error path, nothing to recover
		return nil, &deviceError{ErrAuth, "HTTP 401"}
	case resp.StatusCode != http.StatusOK:
		_ = resp.Body.Close() // error path, nothing to recover
		return nil, &deviceError{ErrBadResponse, "HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	return resp, nil
}

func (c *httpClient) ufsNoReply(parent context.Context, t Target, query url.Values, missing error, what string) error {
	if !c.rootAnswers(parent, t) {
		return c.mapErr(parent, t, io.EOF)
	}
	if _, has := query["download"]; !has {
		// The request that got no answer was the plain page itself.
		return &deviceError{ErrHTTPAPIRefused, t.redact("set SetOption128 1 on " + t.display() + " (console, or Configure Other > HTTP API)")}
	}
	pctx, cancel := context.WithTimeout(parent, c.timeout(followUpTimeout))
	defer cancel()
	probe, err := c.ufsRequest(pctx, t, url.Values{"dir": {"2"}})
	if err != nil {
		if errors.Is(err, io.EOF) {
			return &deviceError{ErrHTTPAPIRefused, t.redact("set SetOption128 1 on " + t.display() + " (console, or Configure Other > HTTP API)")}
		}
		return c.mapErr(parent, t, err)
	}
	_ = probe.Body.Close() // only the fact of an answer matters
	return &deviceError{missing, t.redact(what)}
}

func isAttachment(resp *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Disposition"))), "attachment")
}

func (c *httpClient) DownloadFile(ctx context.Context, t Target, p string, max int64) ([]byte, error) {
	if err := CheckFSPath(p); err != nil {
		return nil, err
	}
	if max <= 0 {
		max = c.o.MaxResponse
	}
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return nil, err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(fileTimeout))
	defer cancel()

	resp, err := c.ufsGet(rctx, ctx, t, url.Values{"dir": {"2"}, "download": {p}}, ErrFileNotFound, p)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body, nothing to recover
	// A folder is answered with the HTML file manager page, a file with an
	// attachment. Anything else is not file content.
	if !isAttachment(resp) {
		return nil, &deviceError{ErrNotAFile, t.redact(p)}
	}
	if resp.ContentLength < 0 {
		return nil, &deviceError{ErrBadResponse, "file reply without Content-Length"}
	}
	if resp.ContentLength > max {
		return nil, &deviceError{ErrFileTooLarge, strconv.FormatInt(resp.ContentLength, 10) + " bytes"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) && ctx.Err() == nil && rctx.Err() == nil {
			return nil, &deviceError{ErrBadResponse, "file cut short (" + strconv.Itoa(len(body)) + " of " + strconv.FormatInt(resp.ContentLength, 10) + " bytes)"}
		}
		return nil, c.mapErr(ctx, t, err)
	}
	return body, nil
}

func (c *httpClient) UploadFile(ctx context.Context, t Target, p string, data []byte) error {
	if err := CheckFSPath(p); err != nil {
		return err
	}
	folder, name := path.Split(p)
	if folder != "/" {
		folder = strings.TrimSuffix(folder, "/")
	}
	// The device formats "%s/%s" of the folder and the name into 50 bytes (49
	// characters) and cuts the rest, so a long name would silently hit another
	// file. For the root the folder is "/", which gives "//name".
	if len(folder)+1+len(name) > maxFSPath {
		return badPath("folder, slash and file name are longer than 49 bytes")
	}
	// One slot for both requests: the target folder and file system are global
	// device state set by the GET, so nothing may come between it and the POST.
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(uploadTimeout))
	defer cancel()

	// The GET sets the upload type (needed on firmware before v16), resets the
	// folder to "/" and selects flash. With a folder it also proves the folder
	// exists: for a missing one the handler sends nothing.
	q := url.Values{"dir": {"2"}}
	if folder != "/" {
		q.Set("download", folder)
	}
	gctx, gcancel := context.WithTimeout(rctx, c.timeout(rootTimeout))
	resp, err := c.ufsGet(gctx, ctx, t, q, ErrFolderNotFound, folder)
	if err == nil {
		attachment := isAttachment(resp)
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, c.o.MaxResponse)) // page body is not needed; a short read changes nothing
		_ = resp.Body.Close()                                                  // read-only body
		if attachment {
			err = &deviceError{ErrNotAFolder, t.redact(folder)}
		}
	}
	gcancel()
	if err != nil {
		return err
	}

	var head bytes.Buffer
	mw := multipart.NewWriter(&head)
	if _, err := mw.CreateFormFile("ufsu", name); err != nil {
		return &deviceError{ErrBadResponse, "invalid file name"}
	}
	headLen := head.Len()
	if err := mw.Close(); err != nil {
		return &deviceError{ErrBadResponse, "invalid multipart body"}
	}
	body := io.MultiReader(bytes.NewReader(head.Bytes()[:headLen]), bytes.NewReader(data), bytes.NewReader(head.Bytes()[headLen:]))
	total := int64(head.Len() + len(data))
	reply, err := c.fetch(rctx, ctx, t, http.MethodPost, "/ufsu", "fsz="+strconv.Itoa(len(data)), body, mw.FormDataContentType(), total)
	if err != nil {
		return err
	}
	text := string(reply)
	switch {
	case strings.Contains(text, "Failed</font>"):
		return &deviceError{ErrUploadRefused, t.redact(refusalText(text))}
	case strings.Contains(text, "Successful</font>"):
		return nil
	}
	return &deviceError{ErrBadResponse, "upload reply is neither Successful nor Failed"}
}
