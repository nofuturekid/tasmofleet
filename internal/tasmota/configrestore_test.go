package tasmota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/simulator"
)

func restorer(t *testing.T) ConfigRestorer {
	t.Helper()
	r, ok := newClient(Options{}).(ConfigRestorer)
	if !ok {
		t.Fatal("the production client must implement ConfigRestorer")
	}
	return r
}

func restoreSim(t *testing.T, spec simulator.DeviceSpec) (simulator.Controller, Target) {
	t.Helper()
	f, err := simulator.NewFleetWith(simulator.FleetOptions{FixturesDir: simFixtures}, simulator.Extra{TimeScale: 0.02})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if spec.Name == "" {
		spec.Name = "plug"
	}
	if spec.Profile == "" {
		spec.Profile = simulator.ProfileESP32
	}
	if spec.Version == "" && spec.Profile != simulator.ProfileESP8266Min {
		spec.Version = "16.0.0"
	}
	d, err := f.Add(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := simulator.Control(d)
	tg := simTarget(t, d)
	tg.user, tg.pass = spec.User, spec.Password
	return c, tg
}

func restoreDump(fill byte) []byte { return bytes.Repeat([]byte{fill}, 4096) }

func download(t *testing.T, tg Target) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := newClient(Options{}).DownloadConfig(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func restoreCfg(r ConfigRestorer, tg Target, dump []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return r.RestoreConfig(ctx, tg, dump)
}

// Why: a restore over HTTP is what the web UI's "Restore configuration" does (#430): GET /rs sets
// the settings upload type, the multipart POST to /u2 carries the dump. The device applies it and
// restarts; nothing else works on official builds (no WebGetConfig).
func TestRestoreConfigAppliesTheDumpAndTheDeviceRestarts(t *testing.T) {
	c, tg := restoreSim(t, simulator.DeviceSpec{RefererCheck: true})
	want := restoreDump(0x5A)
	if err := restoreCfg(restorer(t), tg, want); err != nil {
		t.Fatal(err)
	}
	waitBoots(t, c, 1)
	if !bytes.Equal(download(t, tg), want) {
		t.Error("/dl after the restore is not the dump")
	}
	cmds := strings.Join(c.Commands(), "\n")
	if !strings.Contains(cmds, "POST /u2 Config.dmp 4096 (settings)") {
		t.Errorf("commands:\n%s", cmds)
	}
}

// Why: the device's web password goes as Basic auth to its own target on both requests; a wrong
// one is ErrAuth and nothing is applied.
func TestRestoreConfigAuth(t *testing.T) {
	c, tg := restoreSim(t, simulator.DeviceSpec{Password: testPassword})
	before := download(t, tg)
	bad := tg
	bad.pass = "wrong"
	if err := restoreCfg(restorer(t), bad, restoreDump(1)); !errors.Is(err, ErrAuth) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("wrong password: %v, want ErrAuth and ErrNotSent", err)
	}
	time.Sleep(100 * time.Millisecond)
	if c.Boots() != 0 || !bytes.Equal(download(t, tg), before) {
		t.Error("a refused login changed the device")
	}
}

// Why: a "Failed" page is certain: the device applied nothing and keeps running, so the restore
// can fail at once instead of waiting for a restart that never comes.
func TestRestoreConfigRefusedDumpIsCertain(t *testing.T) {
	c, tg := restoreSim(t, simulator.DeviceSpec{})
	before := download(t, tg)
	err := restoreCfg(restorer(t), tg, []byte("short"))
	if !errors.Is(err, ErrUploadRefused) || !strings.Contains(err.Error(), "File invalid") {
		t.Fatalf("err %v, want ErrUploadRefused with the device's reason", err)
	}
	time.Sleep(100 * time.Millisecond)
	if c.Boots() != 0 || !bytes.Equal(download(t, tg), before) {
		t.Error("a refused dump changed the device")
	}
}

// Why: the minimal firmware has no /rs. Without the page the POST would hit an unset upload type,
// say "Successful" and restart the device with nothing applied; so nothing may be posted.
func TestRestoreConfigWithoutRestorePageSendsNothing(t *testing.T) {
	c, tg := restoreSim(t, simulator.DeviceSpec{Profile: simulator.ProfileESP8266Min})
	err := restoreCfg(restorer(t), tg, restoreDump(1))
	if !errors.Is(err, ErrUploadUnsupported) || !errors.Is(err, ErrNotSent) {
		t.Fatalf("err %v, want ErrUploadUnsupported and ErrNotSent", err)
	}
	for _, cmd := range c.Commands() {
		if strings.HasPrefix(cmd, "POST") {
			t.Errorf("posted: %s", cmd)
		}
	}
}

// Why: a reply that is lost after the body went out leaves it open whether the device applied the
// dump; it must not read as "refused" (the caller then looks at the device, never resends).
func TestRestoreConfigLostReplyIsNotARefusal(t *testing.T) {
	_, tg := restoreSim(t, simulator.DeviceSpec{Faults: []simulator.Fault{{Kind: simulator.FaultUploadLostReply, After: "u2"}}})
	err := restoreCfg(restorer(t), tg, restoreDump(2))
	if err == nil || errors.Is(err, ErrUploadRefused) || errors.Is(err, ErrNotSent) {
		t.Fatalf("err %v, want an unknown outcome", err)
	}
}

// The language builds translate "Successful" and "Failed"; the reload script that only the success
// page carries is the same in every build.
func TestRestoreReplyIsReadWithoutTheLanguage(t *testing.T) {
	ok := `<script>setTimeout(function(){location.href='.';},10000);</script><b>Hochladen <font color='#008000'>Erfolgreich</font></b>`
	bad := `<b>Hochladen <font color='#ff5661'>Fehlgeschlagen</font></b><br><br>Datei ungültig</div>`
	if err := restoreReply(ok); err != nil {
		t.Errorf("German success page: %v", err)
	}
	if err := restoreReply(bad); !errors.Is(err, ErrUploadRefused) {
		t.Errorf("German failure page: %v", err)
	}
	if err := restoreReply("<html>something else</html>"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("other page: %v", err)
	}
}

// postU2Raw is a POST /u2 as another tool (or a dropped TasmoFleet request) could leave it.
func postU2Raw(t *testing.T, tg Target, referer bool, data []byte) error {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("u2", "Config.dmp")
	_, _ = fw.Write(data)
	_ = mw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+tg.hostPort()+"/u2?fsz="+strconv.Itoa(len(data)), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if referer {
		req.Header.Set("Referer", "http://"+tg.display()+"/")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func getRaw(t *testing.T, tg Target, p string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+tg.hostPort()+p, nil)
	req.Header.Set("Referer", "http://"+tg.display()+"/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", p, err)
	}
	_ = resp.Body.Close()
}

// Why: the upload type is one global on the device, and a POST the Referer check drops leaves the
// settings type of a GET /rs armed (live, 2026-11-01). A firmware push that relied on it would
// write the image into the settings, or a restore would flash the dump. Every upload therefore
// sends its own type-setting GET first, in the same device slot: an armed settings type never
// reaches a firmware push, and an armed firmware type never reaches a restore.
func TestArmedUploadTypeNeverLeaksIntoTheNextUpload(t *testing.T) {
	t.Run("settings armed, then a firmware push", func(t *testing.T) {
		c, tg := uploadSim(t, simulator.DeviceSpec{RefererCheck: true})
		getRaw(t, tg, "/rs")
		if err := postU2Raw(t, tg, false, restoreDump(3)); err == nil {
			t.Fatal("the POST without Referer got a reply")
		}
		if err := upload(uploader(t, Options{}), tg, simImage(t, "tasmota32.bin", "16.1.0")); err != nil {
			t.Fatal(err)
		}
		waitBoots(t, c, 1)
		if c.Version() != "16.1.0" {
			t.Errorf("version %s: the firmware was not flashed", c.Version())
		}
		if bytes.Equal(download(t, tg), restoreDump(3)) {
			t.Error("the firmware push applied the dump as settings")
		}
	})
	t.Run("firmware armed, then a restore", func(t *testing.T) {
		c, tg := uploadSim(t, simulator.DeviceSpec{RefererCheck: true})
		getRaw(t, tg, "/up")
		want := restoreDump(4)
		if err := restoreCfg(restorer(t), tg, want); err != nil {
			t.Fatal(err)
		}
		waitBoots(t, c, 1)
		if c.Version() != "16.0.0" {
			t.Errorf("the restore flashed something: version %s", c.Version())
		}
		if !bytes.Equal(download(t, tg), want) {
			t.Error("the dump was not applied")
		}
	})
}
