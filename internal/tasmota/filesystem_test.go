package tasmota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

// fsSim starts one simulated device and a client for it. The FileSystem
// interface is what backup and restore code against, so the tests go through it.
func fsSim(t *testing.T, spec simulator.DeviceSpec, o Options) (FileSystem, Target, simulator.Controller) {
	t.Helper()
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if spec.Name == "" {
		spec.Name = "monitor"
	}
	if spec.Profile == "" {
		spec.Profile = simulator.ProfileESP32
	}
	spec.Version = "16.0.0"
	d, err := f.Add(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	fs, ok := newClient(o).(FileSystem)
	if !ok {
		t.Fatal("the production client does not implement FileSystem")
	}
	return fs, simTarget(t, d), c
}

func ctxS(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func names(es []FileEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestFSInfo_ESP32ReportsFlashSizes(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{}, Options{})
	got, err := fs.FSInfo(ctxS(t), tg)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != FSLittleFS || !got.HasFlash() || got.HasSD || got.SizeKB != 320 || got.FreeKB <= 0 || got.FreeKB >= 320 {
		t.Errorf("FSInfo = %+v", got)
	}
}

// Why: the live ESP8266 1 MB build answers {"Command":"Unknown"}, not 0. That is the normal
// "no filesystem" answer of a build, so the backup must not fail on it.
func TestFSInfo_BuildWithoutFilesystemIsNotAnError(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{Profile: simulator.ProfileESP8266_1M}, Options{})
	got, err := fs.FSInfo(ctxS(t), tg)
	if err != nil {
		t.Fatalf("Unknown command must mean no filesystem, got %v", err)
	}
	if got.HasFlash() || got.Type != FSNone {
		t.Errorf("FSInfo = %+v", got)
	}
}

// Why: with an SD card the device lists the SD card first ({"UfsType":[1,3]}) and its Ufs
// reply is not valid JSON. The flash numbers must come from the second entry, never the SD's.
func TestFSInfo_SDCardPrimaryUsesFlashEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.ToLower(r.URL.Query().Get("cmnd")) {
		case "ufstype":
			_, _ = w.Write([]byte(`{"UfsType":[1,3]}`))
		case "ufssize":
			_, _ = w.Write([]byte(`{"UfsSize":[30000,320]}`))
		case "ufsfree":
			_, _ = w.Write([]byte(`{"UfsFree":[29000,200]}`))
		default:
			t.Errorf("unexpected command %q (Ufs is invalid JSON with an SD card)", r.URL.Query().Get("cmnd"))
		}
	}))
	defer srv.Close()
	got, err := newClient(Options{}).(FileSystem).FSInfo(ctxS(t), target(t, srv, 1, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got != (FSInfo{Type: FSLittleFS, HasSD: true, SizeKB: 320, FreeKB: 200}) {
		t.Errorf("FSInfo = %+v", got)
	}
}

func TestFSInfo_OnlySDCardIsNoFlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"UfsType":1}`))
	}))
	defer srv.Close()
	got, err := newClient(Options{}).(FileSystem).FSInfo(ctxS(t), target(t, srv, 1, "", ""))
	if err != nil || got.HasFlash() || !got.HasSD {
		t.Errorf("FSInfo = %+v, %v: an SD card is never backed up", got, err)
	}
}

func TestListFiles_FilesFoldersEmptyAndMissing(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{
		Files: map[string]simulator.FSFile{
			"/autoexec.be":      {Data: []byte("a"), Modified: time.Date(2026, 10, 31, 12, 34, 56, 0, time.UTC)},
			"/lib/x.be":         {Data: []byte("xyz")},
			`/we"ird.txt`:       {Data: []byte("q")},
			"/.drvset000":       {Data: []byte("hidden")},
			"/deep/er/file.txt": {Data: []byte("1")},
		},
		Dirs: []string{"/empty"}}, Options{})
	root, err := fs.ListFiles(ctxS(t), tg, "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(root), ","); got != `autoexec.be,deep,empty,lib,we"ird.txt` {
		t.Errorf("root = %s (dot files must stay hidden, JSON escapes must survive)", got)
	}
	by := map[string]FileEntry{}
	for _, e := range root {
		by[e.Name] = e
	}
	if e := by["autoexec.be"]; e.Dir || e.Size != 1 || !e.ModTime.Equal(time.Date(2026, 10, 31, 12, 34, 56, 0, time.UTC)) {
		t.Errorf("autoexec.be = %+v", e)
	}
	if e := by["lib"]; !e.Dir || !e.ModTime.IsZero() {
		t.Errorf("lib = %+v: a folder has an empty date", e)
	}
	sub, err := fs.ListFiles(ctxS(t), tg, "/lib")
	if err != nil || len(sub) != 1 || sub[0].Name != "x.be" || sub[0].Size != 3 {
		t.Errorf("/lib = %+v, %v", sub, err)
	}
	// Why: {"UfsList":"Done"} is a string where a list is expected; an empty folder is not an error.
	for _, dir := range []string{"/empty", "/nonexistent"} {
		got, err := fs.ListFiles(ctxS(t), tg, dir)
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("%s = %#v, %v; want empty non-nil slice", dir, got, err)
		}
	}
}

func TestListFiles_RejectsMalformedDeviceReplies(t *testing.T) {
	for name, reply := range map[string]string{
		"two-field rows of firmware before 14.6": `{"UfsList":[["a",12]]}`,
		"name with a slash":                      `{"UfsList":[["../x","2026-01-01T00:00:00",1]]}`,
		"name that is dot dot":                   `{"UfsList":[["..","",0]]}`,
		"dot file name":                          `{"UfsList":[[".settings","2026-01-01T00:00:00",1]]}`,
		"negative size":                          `{"UfsList":[["a","2026-01-01T00:00:00",-1]]}`,
		"unknown string answer":                  `{"UfsList":"Nope"}`,
		"missing key":                            `{"Other":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(reply)) }))
			defer srv.Close()
			got, err := newClient(Options{}).(FileSystem).ListFiles(ctxS(t), target(t, srv, 1, "", ""), "/")
			if err == nil {
				t.Errorf("accepted %s as %+v", reply, got)
			}
		})
	}
}

func TestDownloadFile_ReturnsContent(t *testing.T) {
	fs, tg, c := fsSim(t, simulator.DeviceSpec{Files: map[string]simulator.FSFile{"/lib/x.be": {Data: []byte("print(1)\r\n\x00\xff")}}}, Options{})
	got, err := fs.DownloadFile(ctxS(t), tg, "/lib/x.be", 0)
	if err != nil || string(got) != "print(1)\r\n\x00\xff" {
		t.Fatalf("got %q, %v (binary and CR/LF must arrive untouched)", got, err)
	}
	if cmds := c.Commands(); len(cmds) != 1 || cmds[0] != "GET /ufsd dir=2 download=/lib/x.be" {
		t.Errorf("requests = %v: flash (dir=2) must be named so an SD card is never read", cmds)
	}
}

func TestDownloadFile_EmptyFileIsAFileNotAMissingOne(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{Files: map[string]simulator.FSFile{"/empty.txt": {}}}, Options{})
	got, err := fs.DownloadFile(ctxS(t), tg, "/empty.txt", 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("got %#v, %v", got, err)
	}
}

// Why: the device sends nothing for a missing file. The caller must get a distinct error and
// never an empty []byte it could store in a backup as an empty file.
func TestDownloadFile_MissingFileIsAnErrorNotAnEmptyFile(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{}, Options{})
	got, err := fs.DownloadFile(ctxS(t), tg, "/nope.txt", 0)
	if !errors.Is(err, ErrFileUnavailable) {
		t.Fatalf("err = %v, want ErrFileUnavailable", err)
	}
	if got != nil {
		t.Errorf("got %q alongside the error", got)
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrAuth) {
		t.Errorf("must not be confused with %v", err)
	}
}

// Why: a folder is answered with the HTML file manager (HTTP 200). Without the attachment
// check that page would be stored as the file's content.
func TestDownloadFile_FolderHTMLPageIsRefused(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{}, Options{})
	got, err := fs.DownloadFile(ctxS(t), tg, "/lib", 0)
	if !errors.Is(err, ErrNotAFile) || got != nil {
		t.Errorf("got %d bytes, %v; want ErrNotAFile", len(got), err)
	}
}

func TestDownloadFile_LimitIsCheckedBeforeTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment; filename=big")
		w.Header().Set("Content-Length", "2000000")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()
	fs := newClient(Options{}).(FileSystem)
	start := time.Now()
	_, err := fs.DownloadFile(ctxS(t), target(t, srv, 1, "", ""), "/big", 1<<20)
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("took %v: the limit must be applied on Content-Length, not after reading", time.Since(start))
	}
	// the explicit max wins over the client default
	if _, err := fs.DownloadFile(ctxS(t), target(t, srv, 1, "", ""), "/big", 10); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("max 10: %v", err)
	}
}

func TestDownloadFile_DefaultLimitIsMaxResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", "attachment; filename=f")
		_, _ = w.Write(make([]byte, 100))
	}))
	defer srv.Close()
	fs := newClient(Options{MaxResponse: 50}).(FileSystem)
	if _, err := fs.DownloadFile(ctxS(t), target(t, srv, 1, "", ""), "/f", 0); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("err = %v", err)
	}
}

func TestDownloadFile_BrokenTransfersAreErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"cut short": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", "attachment; filename=f")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			_, _ = w.Write(make([]byte, 40))
			http.NewResponseController(w).Flush() //nolint:errcheck // test
			c, _, _ := http.NewResponseController(w).Hijack()
			_ = c.Close()
		},
		"no Content-Length": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", "attachment; filename=f")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("abc"))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("def"))
		},
		"no attachment header": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "3")
			_, _ = w.Write([]byte("abc"))
		},
		"HTTP 500": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		"HTTP 401": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			got, err := newClient(Options{}).(FileSystem).DownloadFile(ctxS(t), target(t, srv, 1, "", ""), "/f", 0)
			if err == nil || got != nil {
				t.Errorf("got %q, %v", got, err)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer srv.Close()
	if _, err := newClient(Options{}).(FileSystem).DownloadFile(ctxS(t), target(t, srv, 1, "", ""), "/f", 0); !errors.Is(err, ErrAuth) {
		t.Errorf("401 = %v, want ErrAuth", err)
	}
}

func TestDownloadFile_StalledDeviceTimesOut(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{
		Files:  map[string]simulator.FSFile{"/big.bin": {Data: make([]byte, 4096)}},
		Faults: []simulator.Fault{{Kind: simulator.FaultUfsStall, After: "ufsd"}}}, Options{Timeout: 300 * time.Millisecond})
	got, err := fs.DownloadFile(ctxS(t), tg, "/big.bin", 0)
	if !errors.Is(err, ErrTimeout) || got != nil {
		t.Errorf("got %d bytes, %v; want ErrTimeout and no partial content", len(got), err)
	}
}

func TestDownloadFile_CredentialsGoInTheHeaderOnly(t *testing.T) {
	var sawAuth, sawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawQuery = r.URL.RawQuery
		if _, p, ok := r.BasicAuth(); ok {
			sawAuth = p
		}
		w.Header().Set("Content-Disposition", "attachment; filename=f")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	if _, err := newClient(Options{}).(FileSystem).DownloadFile(ctxS(t), target(t, srv, 1, "", testPassword), "/f", 0); err != nil {
		t.Fatal(err)
	}
	if sawAuth != testPassword || strings.Contains(sawQuery, testPassword) {
		t.Errorf("auth header %q, query %q", sawAuth, sawQuery)
	}
}

func TestUploadFile_WritesAndReadsBack(t *testing.T) {
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{}, Options{})
	data := []byte("print('new')\r\n\x00\xff")
	if err := fs.UploadFile(ctxS(t), tg, "/lib/new.be", data); err != nil {
		t.Fatal(err)
	}
	got, err := fs.DownloadFile(ctxS(t), tg, "/lib/new.be", 0)
	if err != nil || string(got) != string(data) {
		t.Errorf("read back %q, %v", got, err)
	}
	if err := fs.UploadFile(ctxS(t), tg, "/autoexec.be", []byte("replaced")); err != nil {
		t.Fatal(err)
	}
	list, _ := fs.ListFiles(ctxS(t), tg, "/")
	n := 0
	for _, e := range list {
		if e.Name == "autoexec.be" {
			n++
			if e.Size != 8 {
				t.Errorf("autoexec.be size %d, want the new 8", e.Size)
			}
		}
	}
	if n != 1 {
		t.Errorf("autoexec.be listed %d times", n)
	}
}

// Why: the target folder and file system are global state set by the GET, and firmware before v16 writes
// nothing on a bare POST. Each upload therefore sends GET /ufsd first, naming the folder, then POSTs.
func TestUploadFile_GETBeforePOSTOnEveryUpload(t *testing.T) {
	fs, tg, c := fsSim(t, simulator.DeviceSpec{}, Options{})
	for _, p := range []string{"/one.txt", "/lib/two.txt", "/lib/three.txt"} {
		if err := fs.UploadFile(ctxS(t), tg, p, []byte("abc")); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"GET /ufsd dir=2", "POST /ufsu /one.txt 3 fsz=3",
		"GET /ufsd dir=2 download=/lib", "POST /ufsu /lib/two.txt 3 fsz=3",
		"GET /ufsd dir=2 download=/lib", "POST /ufsu /lib/three.txt 3 fsz=3",
	}
	if got := strings.Join(c.Commands(), "|"); got != strings.Join(want, "|") {
		t.Errorf("requests:\n got  %s\n want %s", got, strings.Join(want, "|"))
	}
}

// Why: another request between GET and POST (a poll, a Status 1) would move the global folder
// state on the device and the file would land in the wrong place. Both requests hold one lock.
func TestUploadFile_NoRequestSlipsBetweenGetAndPost(t *testing.T) {
	var mu sync.Mutex
	var order []string
	getSeen := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		order = append(order, r.Method+" "+r.URL.Path+" "+r.URL.Query().Get("cmnd"))
		mu.Unlock()
		switch r.URL.Path {
		case "/ufsd":
			close(getSeen)
			time.Sleep(200 * time.Millisecond) // a window for a concurrent request
			_, _ = w.Write([]byte("<html>Manage File system</html>"))
		case "/ufsu":
			_, _ = w.Write([]byte("Upload <font color='#008000'>Successful</font>"))
		default:
			_, _ = w.Write([]byte(`{"Status":{}}`))
		}
	}))
	defer srv.Close()
	cl := newClient(Options{})
	tg := target(t, srv, 1, "", "")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-getSeen
		_, _ = cl.Command(ctxS(t), tg, "Status 1")
	}()
	if err := cl.(FileSystem).UploadFile(ctxS(t), tg, "/a.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "GET /ufsd " || order[1] != "POST /ufsu " || order[2] != "GET /cm Status 1" {
		t.Errorf("order = %q", order)
	}
}

// Why: no binary-safe folder creation exists, and the GET proves the folder exists. A missing
// folder must fail before any byte is written, and must not create the folder.
func TestUploadFile_MissingFolderFailsWithoutWriting(t *testing.T) {
	fs, tg, c := fsSim(t, simulator.DeviceSpec{}, Options{})
	err := fs.UploadFile(ctxS(t), tg, "/newdir/x.be", []byte("abc"))
	if !errors.Is(err, ErrFileUnavailable) {
		t.Fatalf("err = %v", err)
	}
	for _, cmd := range c.Commands() {
		if strings.HasPrefix(cmd, "POST") {
			t.Errorf("a POST was sent: %v", c.Commands())
		}
	}
	if l, _ := fs.ListFiles(ctxS(t), tg, "/newdir"); len(l) != 0 {
		t.Errorf("/newdir has %v", l)
	}
	root, _ := fs.ListFiles(ctxS(t), tg, "/")
	for _, e := range root {
		if e.Name == "newdir" {
			t.Error("the folder was created")
		}
	}
}

func TestUploadFile_FolderThatIsAFileIsRefused(t *testing.T) {
	fs, tg, c := fsSim(t, simulator.DeviceSpec{}, Options{})
	if err := fs.UploadFile(ctxS(t), tg, "/autoexec.be/x", []byte("a")); !errors.Is(err, ErrNotAFolder) {
		t.Errorf("err = %v", err)
	}
	for _, cmd := range c.Commands() {
		if strings.HasPrefix(cmd, "POST") {
			t.Errorf("POST sent: %v", c.Commands())
		}
	}
}

func TestUploadFile_DeviceRefusalsAreErrors(t *testing.T) {
	// Why: "File too large" (error 9) is what a full file system answers; it must surface, with the password redacted.
	fs, tg, _ := fsSim(t, simulator.DeviceSpec{FSSizeKB: 125}, Options{})
	err := fs.UploadFile(ctxS(t), tg, "/big.bin", make([]byte, 50*1024))
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "File too large") {
		t.Errorf("err = %v", err)
	}
	fs, tg, _ = fsSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultUfsUploadError}}}, Options{})
	if err := fs.UploadFile(ctxS(t), tg, "/a.txt", []byte("x")); !errors.Is(err, ErrUploadRefused) {
		t.Errorf("fault: %v", err)
	}
}

func TestUploadFile_ReplyWithoutSuccessIsNotSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>hello</html>")) }))
	defer srv.Close()
	if err := newClient(Options{}).(FileSystem).UploadFile(ctxS(t), target(t, srv, 1, "", ""), "/a.txt", []byte("x")); !errors.Is(err, ErrBadResponse) {
		t.Errorf("err = %v", err)
	}
}

func TestPathRules(t *testing.T) {
	ok := []string{"/a", "/lib/x.be", "/a b.txt", "/" + strings.Repeat("a", 48), "/ü.txt"}
	bad := map[string]string{
		"":                            "empty",
		"a":                           "no leading slash",
		"/":                           "root is not a file",
		"/a/":                         "trailing slash (empty segment)",
		"//a":                         "empty segment",
		"/a//b":                       "empty segment",
		"/..":                         "dot dot",
		"/a/../b":                     "dot dot inside",
		"/.":                          "dot",
		"/.settings":                  "hidden settings file",
		"/lib/.drvset000":             "hidden file in folder",
		"/" + strings.Repeat("a", 49): "50 bytes",
		"/a\x00b":                     "NUL",
		"/a\nb":                       "newline",
		"/a\x7fb":                     "DEL",
		"/a\\b":                       "backslash",
		"/\xff":                       "invalid UTF-8",
		"/" + strings.Repeat("ü", 25): "multi-byte over 49 bytes",
	}
	for _, p := range ok {
		if err := CheckFSPath(p); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for p, why := range bad {
		if err := CheckFSPath(p); !errors.Is(err, ErrBadPath) {
			t.Errorf("%q (%s) accepted: %v", p, why, err)
		}
	}
}

// Why: a path that breaks the rules must never reach the device, whatever the method.
func TestBadPathsSendNothing(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	fs := newClient(Options{}).(FileSystem)
	tg := target(t, srv, 1, "", "")
	for _, p := range []string{"", "x", "/../etc", "/.settings", "/a//b"} {
		if _, err := fs.ListFiles(ctxS(t), tg, p); !errors.Is(err, ErrBadPath) {
			t.Errorf("ListFiles(%q) = %v", p, err)
		}
		if _, err := fs.DownloadFile(ctxS(t), tg, p, 0); !errors.Is(err, ErrBadPath) {
			t.Errorf("DownloadFile(%q) = %v", p, err)
		}
		if err := fs.UploadFile(ctxS(t), tg, p, nil); !errors.Is(err, ErrBadPath) {
			t.Errorf("UploadFile(%q) = %v", p, err)
		}
	}
	if _, err := fs.ListFiles(ctxS(t), tg, "/"); err != nil && errors.Is(err, ErrBadPath) {
		t.Error("the root is a valid folder to list")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("%d requests; only the valid ListFiles(/) may be sent", got)
	}
}

func TestFilesSupported(t *testing.T) {
	lfs := FSInfo{Type: FSLittleFS}
	for _, tc := range []struct {
		version string
		info    FSInfo
		want    bool
	}{
		{"16.0.0", lfs, true},
		{"16.0.0(release-tasmota32)", lfs, true},
		{"14.6.0", lfs, true},
		{"14.6.0.2", lfs, true},
		{"15.1.0-dev", lfs, true},
		{"14.5.0", lfs, false}, // UfsList has no date before 14.6.0
		{"14.5.9", lfs, false},
		{"13.9.0", lfs, false},
		{"9.5.0", lfs, false}, // 9 < 14 although "9" > "14" as text
		{"16.0.0", FSInfo{Type: FSNone}, false},
		{"16.0.0", FSInfo{Type: FSSDCard, HasSD: true}, false},
		{"16.0.0", FSInfo{Type: FSFAT}, true},
		{"", lfs, false},
		{"garbage", lfs, false},
	} {
		if got := FilesSupported(tc.version, tc.info); got != tc.want {
			t.Errorf("FilesSupported(%q, %+v) = %v, want %v", tc.version, tc.info, got, tc.want)
		}
	}
}
