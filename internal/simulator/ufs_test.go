package simulator

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ufsFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, "ufs", "esp32-littlefs", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reply is what the tests need of an HTTP answer; the body is already read and closed.
type reply struct {
	StatusCode    int
	Header        http.Header
	ContentLength int64
}

func send(method, url, ctype string, body io.Reader) (*reply, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return &reply{resp.StatusCode, resp.Header, resp.ContentLength}, b, err
}

func httpGet(t *testing.T, url string) (*reply, []byte) {
	t.Helper()
	r, b, err := send(http.MethodGet, url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, b
}

// capturedFS is the file system of the live ESP32 capture: one 299 byte file.
func capturedFS(t *testing.T) DeviceSpec {
	return DeviceSpec{Name: "monitor", Profile: ProfileESP32, Version: "16.0.0",
		Files: map[string]FSFile{"/_persist.json": {Data: ufsFixture(t, "download-file.body"), Modified: time.Date(2025, 11, 30, 12, 51, 29, 0, time.UTC)}}}
}

// The simulator must answer byte for byte what the live device did, or the
// client tests built on it prove nothing about the real thing.
func TestUFS_CommandsReplayLiveCapture(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), capturedFS(t))
	for cmd, file := range map[string]string{"UfsType": "ufstype.json", "Ufs": "ufs.json", "UfsSize": "ufssize.json", "UfsFree": "ufsfree.json",
		"UfsList": "list-root.json", "UfsList /nonexistent": "list-missing-dir.json"} {
		_, got := httpGet(t, "http://"+c.Addr()+"/cm?cmnd="+strings.ReplaceAll(cmd, " ", "%20"))
		if string(got) != string(ufsFixture(t, file)) {
			t.Errorf("%s: got %s, captured %s", cmd, got, ufsFixture(t, file))
		}
	}
}

func TestUFS_DownloadHeadersAndMissingFileReplayLiveCapture(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), capturedFS(t))
	resp, body := httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2&download=%2F_persist.json")
	if resp.Header.Get("Content-Disposition") != "attachment; filename=_persist.json" || resp.ContentLength != 299 || string(body) != string(ufsFixture(t, "download-file.body")) {
		t.Errorf("download differs from the capture: %v len=%d", resp.Header, resp.ContentLength)
	}
	// Why: the real handler sends nothing for a missing file (curl exit 52). A simulator that
	// answered 404 would let a client treat "missing" as an HTTP status it never gets on a device.
	if r, _, err := send(http.MethodGet, "http://"+c.Addr()+"/ufsd?dir=2&download=%2Fnope.txt", "", nil); err == nil {
		t.Fatalf("missing file answered HTTP %d, the device sends no byte", r.StatusCode)
	}
	resp, page := httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2&download=%2F")
	if resp.Header.Get("Content-Disposition") != "" || !strings.Contains(string(page), "Manage File system") {
		t.Errorf("folder download must be the HTML page, got %q", resp.Header.Get("Content-Disposition"))
	}
}

func TestUFS_HiddenFilesOnlyInList2(t *testing.T) {
	spec := capturedFS(t)
	spec.Files["/.drvset000"] = FSFile{Data: []byte("x")}
	c := add(t, newTestFleet(t, "", Extra{}), spec)
	_, l1 := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList")
	_, l2 := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList2")
	if strings.Contains(string(l1), ".drvset") || !strings.Contains(string(l2), ".drvset000") {
		t.Errorf("dot files: UfsList %s, UfsList2 %s", l1, l2)
	}
}

func TestUFS_Esp8266OneMegabyteBuildHasNoFilesystem(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "lamp", Profile: ProfileESP8266_1M, Version: "16.0.0"})
	_, got := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsType")
	if string(got) != string(mustRead(t, filepath.Join(fixtures, "ufs", "esp8266-1m", "ufstype.json"))) {
		t.Errorf("UfsType = %s", got)
	}
	if r, _ := httpGet(t, "http://"+c.Addr()+"/ufsd"); r.StatusCode != http.StatusNotFound {
		t.Errorf("/ufsd = %d, want 404", r.StatusCode)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func post(t *testing.T, addr, query, name string, data []byte) string {
	t.Helper()
	var buf strings.Builder
	buf.WriteString("--B\r\nContent-Disposition: form-data; name=\"ufsu\"; filename=\"" + name + "\"\r\nContent-Type: application/octet-stream\r\n\r\n")
	buf.Write(data)
	buf.WriteString("\r\n--B--\r\n")
	_, b, err := send(http.MethodPost, "http://"+addr+"/ufsu?"+query, "multipart/form-data; boundary=B", strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Why: ufs_path is global state. UfsList moves it too, so a POST that follows a listing (and no
// GET) lands in the listed folder on v16 -- the trap the client's GET-before-POST rule avoids.
func TestUFS_UploadGoesToTheFolderTheLastRequestLeft(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "m", Profile: ProfileESP32, Version: "16.0.0"})
	httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList%20/lib")
	if out := post(t, c.Addr(), "fsz=1", "a.txt", []byte("x")); !strings.Contains(out, "Successful") {
		t.Fatal(out)
	}
	_, lib := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList%20/lib")
	_, root := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList%20/")
	if !strings.Contains(string(lib), `"a.txt"`) || strings.Contains(string(root), `"a.txt"`) {
		t.Errorf("lib %s root %s", lib, root)
	}
	httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2") // resets to /
	post(t, c.Addr(), "fsz=1", "b.txt", []byte("x"))
	_, root = httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList")
	if !strings.Contains(string(root), `"b.txt"`) {
		t.Errorf("after GET /ufsd the root is the target: %s", root)
	}
}

// Why: before v16 only a GET /ufsd sets the upload type; a bare POST answers Successful and writes
// nothing. A client that skipped the GET would believe the file was restored.
func TestUFS_BarePostBeforeV16WritesNothingButSaysSuccessful(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "m", Profile: ProfileESP32, Version: "15.1.0"})
	if out := post(t, c.Addr(), "fsz=1", "a.txt", []byte("x")); !strings.Contains(out, "Successful") {
		t.Fatal(out)
	}
	if _, l := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList"); strings.Contains(string(l), "a.txt") {
		t.Errorf("file written without GET: %s", l)
	}
	httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2")
	post(t, c.Addr(), "fsz=1", "a.txt", []byte("x"))
	if _, l := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList"); !strings.Contains(string(l), "a.txt") {
		t.Errorf("file missing after GET+POST: %s", l)
	}
}

func TestUFS_UploadErrors(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "m", Profile: ProfileESP32, Version: "16.0.0", FSSizeKB: 200})
	httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2")
	if out := post(t, c.Addr(), "fsz=999999", "a.txt", []byte("x")); !strings.Contains(out, "File too large") {
		t.Errorf("fsz over free space: %s", out)
	}
	httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsList%20/nope") // ufs_path now names a folder that does not exist
	if out := post(t, c.Addr(), "fsz=1", "a.txt", []byte("x")); !strings.Contains(out, "Not enough space") {
		t.Errorf("missing folder: %s", out)
	}
}

func TestUFS_NoFSAndMinimalImagesHaveNone(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	c := add(t, f, DeviceSpec{Name: "m", Profile: ProfileESP32, NoFS: true})
	if _, b := httpGet(t, "http://"+c.Addr()+"/cm?cmnd=UfsType"); !strings.Contains(string(b), `"Command":"Unknown"`) {
		t.Errorf("NoFS: %s", b)
	}
	c2 := add(t, f, DeviceSpec{Name: "min", Profile: ProfileESP8266Min})
	if r, _ := httpGet(t, "http://"+c2.Addr()+"/ufsd"); r.StatusCode != http.StatusNotFound {
		t.Errorf("minimal /ufsd = %d", r.StatusCode)
	}
}

func TestUFS_RefererGuardApplies(t *testing.T) {
	c := add(t, newTestFleet(t, "", Extra{}), DeviceSpec{Name: "m", Profile: ProfileESP32, RefererCheck: true})
	if r, _, err := send(http.MethodGet, "http://"+c.Addr()+"/ufsd?dir=2", "", nil); err == nil {
		t.Errorf("a request without Referer got HTTP %d; SetOption128 0 sends nothing", r.StatusCode)
	}
}

// Why: the device cuts "<ufs_path>/<name>" at 49 characters, and the root is "//name". The simulator
// must do the same, or no test can show what a too long name does to another file.
func TestUFS_UploadNameIsCutLikeTheDevice(t *testing.T) {
	long47 := "/" + strings.Repeat("a", 47)
	spec := DeviceSpec{Name: "m", Profile: ProfileESP32, Version: "16.0.0", Files: map[string]FSFile{long47: {Data: []byte("precious")}}}
	c := add(t, newTestFleet(t, "", Extra{}), spec)
	httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2")
	name48 := strings.Repeat("a", 48)
	if out := post(t, c.Addr(), "fsz=3", name48, []byte("NEW")); !strings.Contains(out, "Successful") {
		t.Fatal(out)
	}
	_, got := httpGet(t, "http://"+c.Addr()+"/ufsd?dir=2&download="+long47)
	if string(got) != "NEW" {
		t.Errorf("the cut name should have replaced the 47 byte file, it holds %q", got)
	}
	if r, _, err := send(http.MethodGet, "http://"+c.Addr()+"/ufsd?dir=2&download=/"+name48, "", nil); err == nil {
		t.Errorf("the 48 byte name must not exist (HTTP %d)", r.StatusCode)
	}
}
