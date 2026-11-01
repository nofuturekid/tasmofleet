package simulator

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// getRs is the web UI's "Restore configuration" page: it sets the settings upload type.
func getRs(t *testing.T, d Device) {
	t.Helper()
	b, code, err := cmWithPath(d, "/rs", "")
	if err != nil || code != 200 || !strings.Contains(b, "action='u2?fsz='") || !strings.Contains(b, "name='u2'") {
		t.Fatalf("GET /rs: %d %v %s", code, err, b)
	}
}

// simDump is a settings dump in the simulator's format with the given boot counter, padded to the
// dump size like the real one.
func simDump(bootCount int, marker string) []byte {
	out := []byte(dumpMagic + `{"bootcount":` + strconv.Itoa(bootCount) + `,"hostname":"` + marker + `"}`)
	return append(out, make([]byte, configDumpSize-len(out))...)
}

// Why: official builds have no WebGetConfig (USE_WEBGETCONFIG is in no build of the v16 catalogue,
// and the live ESP32 answered "Unknown", #430). A simulator that still executed it would let a
// restore path pass every test that cannot work on any real device.
func TestWebGetConfigIsUnknownLikeOfficialBuilds(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	before := dl(t, d)
	got := cm(t, d, "WebGetConfig http://127.0.0.1:1/restore/tok")
	if !strings.Contains(got, `"Command":"Unknown"`) {
		t.Fatalf("reply %s, want Command Unknown", got)
	}
	time.Sleep(200 * time.Millisecond)
	if !bytes.Equal(dl(t, d), before) || d.Boots() != 0 {
		t.Errorf("WebGetConfig changed the device (boots %d)", d.Boots())
	}
}

// Why: this is how a restore over HTTP reaches the device (#430): GET /rs, then the multipart POST
// of the dump to /u2. The device applies it, says Successful and restarts; /dl then returns it.
func TestRestoreUploadAppliesTheDumpAndRestarts(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	want := simDump(5, "restored")
	getRs(t, d)
	page, err := postU2(d, "Config.dmp", want)
	if err != nil || !strings.Contains(page, "Successful</font>") || !strings.Contains(page, "location.href='.'") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	if !bytes.Equal(dl(t, d), want) {
		t.Error("/dl after the restore is not the uploaded dump")
	}
	if !slicesContainsPrefix(d.Commands(), "POST /u2 Config.dmp 4096 (settings)") {
		t.Errorf("commands %v", d.Commands())
	}
}

// Why: a restore must not reset the boot counter: BootCount is in the first 16 bytes of the
// settings, which SettingsConfigRestore skips, so it keeps counting (live, 2026-11-01: a backup
// with 145 restored onto a device at 147 gave 148). The restore's restart proof relies on it.
func TestRestoreUploadKeepsCountingBoots(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	before := d.BootCount()
	getRs(t, d)
	if page, err := postU2(d, "Config.dmp", simDump(2, "old")); err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	eq(t, "BootCount after the restore", d.BootCount(), before+1)
}

// Why (#432 review L3): an ESP32 with a file system dumps more than 4096 bytes: a 16-byte
// "/.settings" header with the total size, the settings and the driver settings files (4224 bytes
// on the live test device). The device takes exactly the size its header names.
func TestRestoreUploadTakesAnESP32DumpWithItsSettingsHeader(t *testing.T) {
	esp32Dump := func(size, total int) []byte {
		b := make([]byte, total)
		copy(b, "/.settings")
		b[14], b[15] = byte(size), byte(size>>8)
		return b
	}
	for _, c := range []struct {
		name       string
		body       []byte
		ok         bool
		wantReason string
	}{
		{"header names its size", esp32Dump(4224, 4224), true, ""},
		{"longer than its header", esp32Dump(4224, 4240), false, "File too large"},
		{"shorter than its header", esp32Dump(4224, 4200), false, "File invalid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestFleet(t, "", Extra{})
			d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
			getRs(t, d)
			page, err := postU2(d, "Config.dmp", c.body)
			if err != nil {
				t.Fatal(err)
			}
			if c.ok {
				if !strings.Contains(page, "Successful") {
					t.Fatal(page)
				}
				settled(t, d, 1)
				if !bytes.Equal(dl(t, d), c.body) {
					t.Error("/dl is not the uploaded dump")
				}
				return
			}
			if !strings.Contains(page, "Failed</font>") || !strings.Contains(page, c.wantReason) {
				t.Errorf("page %s, want %q", page, c.wantReason)
			}
		})
	}
}

// Why (#432 review L3): HandleUploadUFSDone does not clear the upload type, so after a file upload
// the type stays "file". A bare POST /u2 then applies no settings: the restore's own GET /rs is
// what makes its upload a settings restore.
func TestFileUploadLeavesTheFileTypeSet(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	before := dl(t, d)
	if _, b := httpGet(t, "http://"+d.Addr()+"/ufsd?dir=2"); len(b) == 0 {
		t.Fatal("no file manager page")
	}
	if got := post(t, d.Addr(), "fsz=1", "x.txt", []byte("x")); !strings.Contains(got, "Successful") {
		t.Fatal(got)
	}
	if page, err := postU2(d, "Config.dmp", simDump(5, "x")); err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	if !bytes.Equal(dl(t, d), before) {
		t.Error("a POST /u2 after a file upload applied settings")
	}
	getRs(t, d)
	want := simDump(6, "y")
	if page, err := postU2(d, "Config.dmp", want); err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 2)
	if !bytes.Equal(dl(t, d), want) {
		t.Error("GET /rs did not make the next upload a settings restore")
	}
}

// Why: the device refuses a file that is not a settings dump with its own messages and neither
// restarts nor changes anything; the client must see "Failed".
func TestRestoreUploadRefusesBadDumps(t *testing.T) {
	for _, c := range []struct {
		name string
		body []byte
		want string
	}{
		{"short", []byte("short"), "File invalid"},
		{"too large", make([]byte, configDumpSize+16), "File too large"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newTestFleet(t, "", Extra{})
			d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
			before := dl(t, d)
			getRs(t, d)
			page, err := postU2(d, "Config.dmp", c.body)
			if err != nil || !strings.Contains(page, "Failed</font>") || !strings.Contains(page, c.want) || strings.Contains(page, "location.href") {
				t.Fatalf("%v %s", err, page)
			}
			time.Sleep(200 * time.Millisecond)
			if !bytes.Equal(dl(t, d), before) || d.Boots() != 0 {
				t.Errorf("refused dump changed the device (boots %d)", d.Boots())
			}
		})
	}
}

// Why: the trap seen live on 2026-11-01: a POST /u2 without the GET /rs before it answers
// "Successful" and restarts, yet applies nothing. Only the verification after the restart can
// tell, so the simulator keeps the trap.
func TestRestoreUploadWithoutRsLooksSuccessfulButAppliesNothing(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0"})
	before := dl(t, d)
	page, err := postU2(d, "Config.dmp", simDump(5, "restored"))
	if err != nil || !strings.Contains(page, "Successful") {
		t.Fatalf("%v %s", err, page)
	}
	settled(t, d, 1)
	if !bytes.Equal(dl(t, d), before) {
		t.Error("a POST without GET /rs applied the dump")
	}
}

// Why: the minimal firmware (FIRMWARE_MINIMAL) registers no /rs, so there is no restore over HTTP.
func TestRestorePageMissingOnMinimal(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "min", Profile: ProfileESP8266Min})
	if _, code, err := cmWithPath(d, "/rs", ""); err != nil || code != 404 {
		t.Errorf("minimal /rs = %d %v, want 404", code, err)
	}
}

// refPost sends a multipart /u2 POST with or without the Referer the device's check wants.
func refPost(d Device, referer bool, filename string, data []byte) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("u2", filename)
	_, _ = fw.Write(data)
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+d.Addr()+"/u2?fsz="+strconv.Itoa(len(data)), &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if referer {
		req.Header.Set("Referer", "http://"+d.Addr()+"/")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

// Why: with SetOption128 0 a POST without the Referer is dropped before the upload handler runs,
// which leaves the settings upload type of an earlier GET /rs set (seen live 2026-11-01). The
// simulator must keep it armed, so a test can prove the next upload sets its own type first.
func TestRefusedPostLeavesTheSettingsTypeArmed(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "plug", Profile: ProfileESP32, Version: "16.0.0", RefererCheck: true})
	get := func(p string) []byte {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+d.Addr()+p, nil)
		req.Header.Set("Referer", "http://"+d.Addr()+"/")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	get("/rs")
	if err := refPost(d, false, "Config.dmp", simDump(5, "x")); err == nil {
		t.Fatal("a POST without Referer got a reply; SetOption128 0 sends nothing")
	}
	want := simDump(6, "armed")
	if err := refPost(d, true, "Config.dmp", want); err != nil {
		t.Fatal(err)
	}
	settled(t, d, 1)
	if !bytes.Equal(get("/dl"), want) {
		t.Error("the type set by GET /rs did not survive the dropped POST")
	}
}
