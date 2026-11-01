package web

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/importer"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// The "Add devices" pages (manual form, discovery, import) show what went wrong
// at the input it is about, in a sentence, and never offer an action that
// cannot work (QA v0.9.0, package D).

// Why: a Go error chain ("tasmota: address outside device networks: 8.8.8.8")
// is useless to the person adding a device. The sentence belongs under the
// Address field, and "Add without probing" must not be offered: the inventory
// refuses an address outside the device networks anyway, so that path cannot work.
func TestManualAddOutsideNetworkExplainsAtAddressAndOffersNoSkip(t *testing.T) {
	e := setup(t)
	e.pr.err = fmt.Errorf("probe: %w: 8.8.8.8", tasmota.ErrForbiddenAddr)
	r, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"8.8.8.8"}})
	if r.StatusCode != 422 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if !strings.Contains(body, `id="err-address"`) || !strings.Contains(body, "That address is outside the configured device networks.") {
		t.Errorf("the sentence is not under the Address field: %s", body)
	}
	if tag := inputTag(t, body, "address"); !strings.Contains(tag, `aria-invalid="true"`) {
		t.Errorf("Address is not marked invalid: %s", tag)
	}
	for _, raw := range []string{"tasmota:", "Probe failed", "8.8.8.8</p>"} {
		if strings.Contains(body, raw) {
			t.Errorf("raw error text %q reached the page", raw)
		}
	}
	if strings.Contains(body, "skip_probe") {
		t.Error("skip-probe offered for an address that the inventory refuses")
	}
}

// Why: a device that is off or not there yet is the case "Add without probing"
// exists for; the message must say so, under the Address field, without the
// dial error of the Go net package ("dial 127.0.0.1:8: connect: connection refused").
func TestManualAddUnreachableKeepsSkipProbeAndHidesTheDialError(t *testing.T) {
	e := setup(t)
	e.pr.err = fmt.Errorf("dial tcp 127.0.0.1:8: connect: connection refused: %w", tasmota.ErrUnreachable)
	_, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}})
	if !strings.Contains(body, `id="err-address"`) || !strings.Contains(body, "No device answered at this address") {
		t.Errorf("no sentence at the Address field: %s", body)
	}
	if strings.Contains(body, "connection refused") || strings.Contains(body, "dial tcp") {
		t.Error("the dial error reached the page")
	}
	if !strings.Contains(body, `name="skip_probe"`) {
		t.Error("an unreachable device can still be added without probing")
	}
	// A rejected web password is about the password, not about the address.
	e.pr.err = fmt.Errorf("probe: %w", tasmota.ErrAuth)
	_, body = e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}})
	if tag := inputTag(t, body, "http_password"); !strings.Contains(tag, `aria-invalid="true"`) {
		t.Errorf("a refused web password does not mark the password input: %s", tag)
	}
	// An unknown failure says nothing technical and stays in the banner.
	e.pr.err = errors.New("something unforeseen: secret detail")
	_, body = e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}})
	if strings.Contains(body, "secret detail") || !strings.Contains(body, "The server log has the details.") {
		t.Errorf("unknown probe error: %s", body)
	}
}

// Why: the button said "Add and probe" even when the box next to it said
// "without probing". One neutral label is always true.
func TestManualAddButtonIsNeutral(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "Add and probe") || !strings.Contains(body, ">Add device</button>") {
		t.Errorf("the submit button must read Add device: %s", body)
	}
}

// noNetworkEnv is an admin environment with no device network and a discovery fake.
func noNetworkEnv(t *testing.T, fd *fakeDiscovery) *env {
	t.Helper()
	fs := &fakeSetup{}
	e, svc, _, _ := sysEnvWithDeps(t, func(d *Deps) { d.Setup = fs; d.Discovery = fd })
	fs.svc = svc
	if _, err := svc.Apply(context.Background(), sysconfig.Request{Actor: "t", Clear: []string{"DEVICE_NETWORKS"}}); err != nil {
		t.Fatal(err)
	}
	return e
}

// Why: every submit fails with the outside-network error while no device
// network exists, and the scan tab already says so by disabling its button;
// the manual tab has to agree, with the reason beside the button.
func TestManualAddIsDisabledWithoutDeviceNetworkAndSaysWhy(t *testing.T) {
	e := noNetworkEnv(t, &fakeDiscovery{})
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	btn := regexp.MustCompile(`<button type="submit" class="btn btn-primary"[^>]*>Add device</button>`).FindString(body)
	if !strings.Contains(btn, "disabled") || !strings.Contains(btn, `aria-describedby="add-off"`) || !strings.Contains(body, `id="add-off"`) {
		t.Errorf("Add device must be disabled with its reason: %q", btn)
	}
}

// Why: the same state on the scan tab. One reason, one setup link (the
// notice above every page): Scan now is off with its reason, and the tab does
// not repeat the notice in other words.
func TestDiscoveryWithoutDeviceNetworkDisablesScanAndDropsTheStartHint(t *testing.T) {
	e := noNetworkEnv(t, &fakeDiscovery{})
	_, body := e.do(t, "GET", "/discovery/scan", "admin", nil)
	btn := regexp.MustCompile(`<button type="submit" class="btn btn-primary"[^>]*>Scan now</button>`).FindString(body)
	if !strings.Contains(btn, "disabled") || !strings.Contains(btn, `aria-describedby="scan-off"`) || !strings.Contains(body, `id="scan-off"`) {
		t.Errorf("Scan now must be disabled with its reason: %q", btn)
	}
	if strings.Contains(body, "Add one in Settings") || strings.Count(body, "Open the setup") != 1 || strings.Contains(body, "settings/system?tab=network") {
		t.Error("the page must name the setup once, in the notice")
	}
}

// Why: a refused range is a problem with the Subnet input: the sentence sits
// under it (capitalised, no "scan range refused:" chain), the input is marked
// invalid and keeps what was typed, and no page banner repeats it.
func TestScanRangeErrorSitsAtTheRangesField(t *testing.T) {
	fd := &fakeDiscovery{startErr: fmt.Errorf("%w: 10.99.0.0/24 is outside the device networks", discovery.ErrScanRange)}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	r, body := e.do(t, "POST", "/discovery/scan", "admin", url.Values{"cidrs": {"10.99.0.0/24"}})
	if r.StatusCode != 422 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if !strings.Contains(body, `id="err-cidrs"`) || !strings.Contains(body, "10.99.0.0/24 is outside the device networks.") {
		t.Errorf("no sentence under the Subnet field: %s", body)
	}
	if tag := inputTag(t, body, "cidrs"); !strings.Contains(tag, `aria-invalid="true"`) || !strings.Contains(tag, `value="10.99.0.0/24"`) {
		t.Errorf("Subnet is not marked invalid or lost the input: %s", tag)
	}
	if strings.Contains(body, "scan range refused") || strings.Contains(body, `role="alert"`) {
		t.Error("the raw chain or a duplicate banner is shown")
	}
	// Input that is no range at all is the same kind of problem.
	_, body = e.do(t, "POST", "/discovery/scan", "admin", url.Values{"cidrs": {"nonsense"}})
	if !strings.Contains(body, `id="err-cidrs"`) || !strings.Contains(body, "is not a range like 192.168.1.0/24") {
		t.Errorf("bad input: %s", body)
	}
}

// Why: "Show ignored" is a filter (decision 19): it sits above the table,
// shows how many are ignored, says whether it is on, and switches the list.
func TestShowIgnoredIsAFilterAboveTheTable(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.ignored = []discovery.Proposal{{ID: 9, Kind: discovery.KindNew, SeenAt: time.Now(), Device: model.Device{Name: "hidden-lamp", Address: "10.0.0.60", MAC: "aa:bb:cc:dd:ee:09"}}}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	sw := regexp.MustCompile(`<a class="add-showign"[^>]*>`).FindString(body)
	if !strings.Contains(sw, `aria-checked="false"`) || !strings.Contains(sw, `href="/tf/devices/new?ignored=1"`) || !strings.Contains(body, "Show ignored (1)") {
		t.Fatalf("Show ignored: %q", sw)
	}
	if strings.Index(body, sw) > strings.Index(body, "<table") || strings.Contains(body, "hidden-lamp") {
		t.Error("the switch must sit above the table, and ignored devices stay hidden")
	}
	_, body = e.do(t, "GET", "/devices/new?ignored=1", "admin", nil)
	if sw := regexp.MustCompile(`<a class="add-showign"[^>]*>`).FindString(body); !strings.Contains(sw, `aria-checked="true"`) || !strings.Contains(body, "hidden-lamp") {
		t.Error("the switch must stay on while ignored devices are listed")
	}
}

// Why: "so far" says the scan is still going; a finished scan found nothing.
func TestFinishedScanSaysNothingFoundWithoutSoFar(t *testing.T) {
	fd := &fakeDiscovery{scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 4, Done: 4, StartedAt: time.Now()}}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, body := e.do(t, "GET", "/discovery/scan/abc123", "admin", nil)
	if strings.Contains(body, "so far") || !strings.Contains(body, "<h2>Nothing found</h2>") || !strings.Contains(body, "Scan finished.") {
		t.Errorf("finished scan: %s", body)
	}
	fd.scan.State = discovery.ScanRunning
	_, body = e.do(t, "GET", "/discovery/scan/abc123", "admin", nil)
	if !strings.Contains(body, "Nothing found so far") || !strings.Contains(body, ">Stop scan</button>") {
		t.Error("a running scan has not found anything yet, and can be stopped")
	}
}

// Why: the Network scan tab shows the latest scan without a click (the tab is
// where the admin left it), and before any scan it says so instead of an empty table.
func TestScanTabShowsTheLatestScan(t *testing.T) {
	e, _ := setupDiscovery(t)
	_, body := e.do(t, "GET", "/discovery/scan", "admin", nil)
	if !strings.Contains(body, "Scan finished.") || !strings.Contains(body, "4 addresses · 1 Tasmota device: 0 already known, 1 new, 0 ignored.") {
		t.Errorf("latest scan: %s", body)
	}
	e2 := setupWith(t, func(d *Deps) { d.Discovery = &fakeDiscovery{} })
	_, body = e2.do(t, "GET", "/discovery/scan", "admin", nil)
	if !strings.Contains(body, "No scan yet") || strings.Contains(body, "<table") {
		t.Errorf("no scan yet: %s", body)
	}
}

// Why: a scan result is only useful with the next step on each row: a new
// device can be ignored or added (its proposal, prefilled), a known one opened,
// an ignored one restored. The filter chips count what is behind them.
func TestScanRowsOfferTheNextStep(t *testing.T) {
	now := time.Now()
	fd := &fakeDiscovery{
		props:   []discovery.Proposal{{ID: 4, Version: "v4", Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "fresh", Address: "10.0.0.50", MAC: "aa:bb:cc:dd:ee:01"}}},
		ignored: []discovery.Proposal{{ID: 5, Kind: discovery.KindNew, Via: "scan", SeenAt: now, Device: model.Device{Name: "old", Address: "10.0.0.51", MAC: "aa:bb:cc:dd:ee:05"}}},
		scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 254, Done: 254, StartedAt: now, FinishedAt: now,
			Hits: []discovery.Hit{
				{Address: "10.0.0.50", Port: 80, MAC: "aa:bb:cc:dd:ee:01", Name: "fresh", Version: "14.3.0", Outcome: "new proposal"},
				{Address: "10.0.0.51", Port: 80, MAC: "aa:bb:cc:dd:ee:05", Name: "old", Outcome: "new proposal"},
				{Address: "10.0.0.2", Port: 80, MAC: "aa:bb:cc:dd:ee:22", Name: "kitchen", Outcome: "known device"},
			}},
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	d2 := e.inv.devs[2]
	d2.MAC = "aa:bb:cc:dd:ee:22"
	e.inv.devs[2] = d2
	_, body := e.do(t, "GET", "/discovery/scan", "admin", nil)
	for _, want := range []string{
		`href="/tf/devices/new?proposal=4#card-manual"`, `formaction="/tf/discovery/proposals/4/dismiss"`, `value="4:v4"`,
		`formaction="/tf/discovery/proposals/5/undismiss"`, ">Un-ignore</button>",
		`href="/tf/devices/2"`, ">Already known</span>", ">attic<",
		`All<span class="add-chip-n">3</span>`, `New<span class="add-chip-n">1</span>`, `Known<span class="add-chip-n">1</span>`, `Ignored<span class="add-chip-n">1</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	_, body = e.do(t, "GET", "/discovery/scan?f=known", "admin", nil)
	if strings.Contains(body, ">fresh<") || !strings.Contains(body, ">attic<") {
		t.Errorf("the Known chip must list only known devices: %s", body[strings.Index(body, "add-chips"):])
	}
}

// ---- import ----

// checkingInv is a recording inventory that refuses addresses outside 10.0.0.0/24,
// like inventory.Manager does with the device networks.
type checkingInv struct {
	recInv
	failUpsert error
}

func (c *checkingInv) CheckAddress(a string) error {
	if !strings.HasPrefix(a, "10.0.0.") {
		return fmt.Errorf("%w: %w: %s", inventory.ErrInvalid, inventory.ErrOutsideNetwork, a)
	}
	return nil
}

func (c *checkingInv) Upsert(ctx context.Context, actor string, d model.Device) (model.DeviceID, bool, error) {
	if c.failUpsert != nil {
		return 0, false, c.failUpsert
	}
	return c.recInv.Upsert(ctx, actor, d)
}

func checkingImportEnv(t *testing.T) (*env, *checkingInv) {
	t.Helper()
	inv := &checkingInv{}
	e := setupWith(t, func(d *Deps) { d.Import = importer.New(importer.Options{Inventory: inv}) })
	return e, inv
}

// Why: "0 skipped" next to a rejected line is wrong, and an address outside the
// device networks used to be listed as Create and then failed on apply. The
// plan must say Skip with the reason, so nobody finds out only after Apply.
func TestImportPlanSkipsRefusedRowsAndCountsRejectedLines(t *testing.T) {
	e, inv := checkingImportEnv(t)
	csv := `1,"lamp",10.0.0.9,,,bulb_1,1,1,0,0,1,80,"lamp",0,lamp,0` + "\n" +
		`2,"outside",8.8.8.8,,,bulb_1,1,1,0,0,1,80,"outside",0,out,0` + "\n" +
		`3,"bad row",,,,bulb_1,1,1,0,0,1,80,"bad",0,bad,0` + "\n"
	c, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	if c != 200 {
		t.Fatalf("status %d: %s", c, body)
	}
	for _, want := range []string{">1 create</span>", ">0 merge</span>", ">2 reject</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary lacks %q: %s", want, body)
		}
	}
	if !strings.Contains(body, "address is outside the configured device networks") {
		t.Error("the outside row has no reason")
	}
	if len(inv.upserts) != 0 {
		t.Fatal("a plan must not write")
	}
}

// Why: a row that only carries a warning (port 99999 becomes 80, an encrypted
// password without its key) is still planned. Counting its error as a skipped
// row showed "1 to create, 1 skipped" for one row, and the same for every row
// of an encrypted export uploaded without the key file.
func TestImportPlanCountsOnlyRejectedLinesAsSkipped(t *testing.T) {
	e, _ := checkingImportEnv(t)
	csv := `1,"lamp",10.0.0.9,,,bulb_1,1,1,0,0,1,99999,"lamp",0,lamp,0` + "\n"
	_, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	if !strings.Contains(body, ">1 create</span>") || !strings.Contains(body, ">0 reject</span>") {
		t.Errorf("a warning on a planned row is not a rejected row: %s", body)
	}
	if !strings.Contains(body, "unusable port") {
		t.Errorf("the warning must still be shown: %s", body)
	}
}

// Why: a failed import line must not inherit the OTA hint "the update did not
// finish; the device keeps its old state".
func TestImportResultFailedBadgeHasNoUpdateHint(t *testing.T) {
	e, inv := checkingImportEnv(t)
	csv := `1,"lamp",10.0.0.9,,,bulb_1,1,1,0,0,1,80,"lamp",0,lamp,0` + "\n"
	_, plan := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	token := planToken(t, plan)
	inv.failUpsert = fmt.Errorf("boom")
	_, out := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", url.Values{"plan": {token}})
	if !strings.Contains(out, `<span class="bdg t-danger">Failed</span>`) || strings.Contains(out, "the update did not finish") {
		t.Errorf("failed badge: %s", out)
	}
}

// Why: a file with nothing usable gave an empty table header and an enabled
// "Apply import" that could only write nothing.
func TestImportPlanWithNothingToApplyHasNoApplyButton(t *testing.T) {
	e, _ := checkingImportEnv(t)
	csv := `1,"outside",8.8.8.8,,,bulb_1,1,1,0,0,1,80,"outside",0,out,0` + "\n"
	_, body := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	if strings.Contains(body, "Apply plan") || strings.Contains(body, `name="plan"`) {
		t.Errorf("nothing can be applied, yet the plan offers it: %s", body)
	}
	if !strings.Contains(body, "Nothing to import.") {
		t.Errorf("no explanation: %s", body)
	}
	// Only rejected lines: the line is listed as rejected, with its reason, and nothing can be applied.
	_, body = e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", `1,"bad",,,,bulb_1,1,1,0,0,1,80,"bad",0,bad,0` + "\n"})
	if strings.Contains(body, "Apply plan") || !strings.Contains(body, "Nothing to import.") || !strings.Contains(body, "devices.csv line 1") {
		t.Errorf("a file without rows: %s", body)
	}
}

// Why: the result must read like the plan: a badge per row (failed in the
// danger tone), host:port in Address (rows of one host differ by port only),
// and a short sentence instead of "inventory: invalid device: inventory: ...".
func TestImportResultUsesBadgesPortsAndSentences(t *testing.T) {
	e, inv := checkingImportEnv(t)
	csv := `1,"lamp",10.0.0.9,,,bulb_1,1,1,0,0,1,8080,"lamp",0,lamp,0` + "\n"
	_, plan := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	token := planToken(t, plan)
	inv.failUpsert = fmt.Errorf("inventory: invalid device: %w: %w: 10.0.0.9", inventory.ErrInvalid, inventory.ErrOutsideNetwork)
	_, out := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", url.Values{"plan": {token}})
	if !strings.Contains(out, `<span class="bdg t-danger">Failed</span>`) {
		t.Errorf("a failed row needs the danger badge: %s", out)
	}
	if !strings.Contains(out, "10.0.0.9") || !strings.Contains(out, ":8080") {
		t.Errorf("the port is dropped from the Address column: %s", out)
	}
	if !strings.Contains(out, "The address is outside the configured device networks.") || strings.Contains(out, "inventory:") {
		t.Errorf("detail is not a sentence: %s", out)
	}
}

// Why: the drop zone has one "Choose files" for all three TasmoAdmin files
// (the prototype's), so the server must tell them apart by name: a .csv is
// devices.csv, a .json MyConfig.json, anything else the key file; two files of
// one kind are refused instead of one silently winning.
func TestImportChooserSortsFilesByName(t *testing.T) {
	for name, want := range map[string]string{"devices.csv": "devices_csv", "DEVICES.CSV": "devices_csv", "MyConfig.json": "my_config", ".device-password.key": "key_file", "key": "key_file"} {
		if got := uploadField(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	e, _ := checkingImportEnv(t)
	_, page := e.do(t, "GET", "/devices/new", "admin", nil)
	if !regexp.MustCompile(`<input type="file" name="files" multiple`).MatchString(page) || strings.Contains(page, `name="devices_csv"`) {
		t.Errorf("the drop zone needs one multi-file chooser: %s", page)
	}
	csv := `1,"lamp",10.0.0.9,,,bulb_1,1,1,0,0,1,80,"lamp",0,lamp,0` + "\n"
	c, body := e.upload(t, "admin", part{"files", "devices.csv", "text/csv", csv})
	if c != 200 || !strings.Contains(body, ">1 create</span>") || !strings.Contains(body, "<b>devices.csv</b> uploaded from TasmoAdmin") {
		t.Errorf("a chosen devices.csv is not used: %d %s", c, body)
	}
	if c, _ := e.upload(t, "admin", part{"files", "a.csv", "text/csv", csv}, part{"files", "b.csv", "text/csv", csv}); c != 400 {
		t.Errorf("two devices.csv: %d, want 400", c)
	}
}

// Why: "Download report" hands over what the import did, for the admin's own
// records: only for the session that applied it, as CSV, with a name that a
// spreadsheet would run as a formula defused.
func TestImportReportDownload(t *testing.T) {
	e, _ := checkingImportEnv(t)
	if r, _ := e.do(t, "GET", "/import/tasmoadmin/report.csv", "admin", nil); r.StatusCode != 404 {
		t.Fatalf("no import yet: %d", r.StatusCode)
	}
	csv := `1,"=cmd|' /C calc'!A0",10.0.0.9,,,bulb_1,1,1,0,0,1,8080,"=cmd|' /C calc'!A0",0,lamp,0` + "\n"
	_, plan := e.upload(t, "admin", part{"devices_csv", "devices.csv", "text/csv", csv})
	_, out := e.do(t, "POST", "/import/tasmoadmin/apply", "admin", url.Values{"plan": {planToken(t, plan)}})
	if !strings.Contains(out, `href="/tf/import/tasmoadmin/report.csv"`) {
		t.Fatalf("the result offers no report: %s", out)
	}
	r, rep := e.do(t, "GET", "/import/tasmoadmin/report.csv", "admin", nil)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/csv") || !strings.Contains(r.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("report: %d %v", r.StatusCode, r.Header)
	}
	if !strings.Contains(rep, "name,address,port,status,detail") || !strings.Contains(rep, "'=cmd") || !strings.Contains(rep, ",8080,created,") {
		t.Errorf("report body: %q", rep)
	}
	if r, _ := e.do(t, "GET", "/import/tasmoadmin/report.csv", "operator", nil); r.StatusCode != 403 {
		t.Errorf("operators must not download it: %d", r.StatusCode)
	}
}

// Why (N20): adding a device takes the fleet default device password only when
// the form asks for it (the prefill checkbox); a typed password always wins,
// and the default never appears in a page.
func TestAddDeviceDefaultPassword(t *testing.T) {
	e := setupWith(t, func(d *Deps) { d.DefaultDevicePassword = func() string { return "fleet-fake-pw" } })
	add := func(extra url.Values) model.Device {
		t.Helper()
		form := url.Values{"address": {"10.0.0.9"}, "name": {"x"}, "skip_probe": {"1"}}
		for k, v := range extra {
			form[k] = v
		}
		if r, _ := e.do(t, "POST", "/devices/new", "admin", form); r.StatusCode != 303 {
			t.Fatalf("%v: status %d", extra, r.StatusCode)
		}
		return e.inv.last
	}
	if d := add(nil); d.HTTPPassword != "" {
		t.Fatal("without the checkbox no password may be set")
	}
	if d := add(url.Values{"use_default_password": {"1"}}); d.HTTPPassword != "fleet-fake-pw" || d.HTTPUser != "" {
		t.Fatalf("default not used (user must stay Tasmota's admin): %q %q", d.HTTPUser, d.HTTPPassword)
	}
	if d := add(url.Values{"use_default_password": {"1"}, "http_password": {"typed-pw"}}); d.HTTPPassword != "typed-pw" {
		t.Fatal("a typed password must win over the default")
	}
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "fleet-fake-pw") {
		t.Fatal("the default password must never reach the browser")
	}
}

// ---- design v2 Add devices (P6) ----

// Why: "Test connection" is how an admin checks an address and password
// before committing: it probes and says what answered, and it must never add
// the device (nor send anything but the probe).
func TestManualTestConnectionProbesWithoutAdding(t *testing.T) {
	e := setup(t)
	e.pr.describe = model.Device{Name: "shed-light", MAC: "aa:bb:cc:00:00:01"}
	before := len(e.inv.devs)
	r, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.88"}, "test": {"1"}})
	if r.StatusCode != 200 || !strings.Contains(body, "<b>Connected.</b> shed-light answered. Nothing was added yet.") {
		t.Fatalf("test result: %d %s", r.StatusCode, body)
	}
	if len(e.inv.devs) != before {
		t.Fatal("Test connection added the device")
	}
	if tag := inputTag(t, body, "address"); !strings.Contains(tag, `value="10.0.0.88"`) {
		t.Errorf("the form must keep the address for the real add: %s", tag)
	}
	e.pr.err = fmt.Errorf("probe: %w", tasmota.ErrUnreachable)
	_, body = e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.88"}, "test": {"1"}})
	if !strings.Contains(body, `id="err-address"`) || strings.Contains(body, "Connected.") || len(e.inv.devs) != before {
		t.Errorf("a failed test must say so at the address and add nothing: %s", body)
	}
}

// Why: a typed address with a space ("attic heater") is a typo, not a host that
// did not answer: the sentence says what to fix before anything is probed.
func TestManualAddressWithSpacesIsATypo(t *testing.T) {
	e := setup(t)
	e.pr.err = errors.New("must not be probed")
	r, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"attic heater"}})
	if r.StatusCode != 422 || !strings.Contains(body, "Enter an IP address or hostname without spaces, e.g. 192.168.1.80") || strings.Contains(body, "server log") {
		t.Fatalf("%d %s", r.StatusCode, body)
	}
}

// Why: the prototype's form has only the address and the password; name, port,
// tags, notes and the OTA exclusion are kept under "More options" (they are
// still saved), and the fold opens by itself when one of them needs fixing.
func TestManualMoreOptionsKeepTheOtherFields(t *testing.T) {
	e := setup(t)
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	fold := regexp.MustCompile(`(?s)<details class="add-more"[^>]*>.*?</details>`).FindString(body)
	for _, name := range []string{"name", "port", "tags", "notes", "ota_excluded"} {
		if !strings.Contains(fold, `name="`+name+`"`) {
			t.Errorf("%s is not under More options", name)
		}
	}
	if strings.Contains(fold, "<details class=\"add-more\" open") {
		t.Error("the fold starts closed")
	}
	form := url.Values{"address": {"10.0.0.9"}, "name": {"porch"}, "port": {"8080"}, "tags": {"outside, lamp"}, "notes": {"n"}, "ota_excluded": {"1"}, "skip_probe": {"1"}}
	if r, _ := e.do(t, "POST", "/devices/new", "admin", form); r.StatusCode != 303 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if d := e.inv.last; d.Name != "porch" || d.Port != 8080 || len(d.Tags) != 2 || d.Notes != "n" || !d.OTAExcluded {
		t.Errorf("More options were not saved: %+v", d)
	}
	_, body = e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}, "port": {"99999"}})
	if !strings.Contains(body, `<details class="add-more" open`) || !strings.Contains(body, `id="err-port"`) {
		t.Errorf("a port error must open the fold: %s", body)
	}
}

// Why: "Add…" on a discovered device opens the manual form filled from it, with
// a banner that says so and a way back. The fleet default password is never
// sent to a discovered address (security review 2026-10-22): the prefilled form
// does not carry it, and the server ignores it for a prefilled form.
func TestAddFromDiscoveryPrefillsWithoutTheDefaultPassword(t *testing.T) {
	_, fd := setupDiscovery(t)
	fd.props[0].Device.Name = "Shed Light"
	fd.props[0].Device.Topic = "tasmota-5F3A21-6689"
	e := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.DefaultDevicePassword = func() string { return "fleet-fake-pw" }
	})
	_, body := e.do(t, "GET", "/devices/new?proposal=1", "admin", nil)
	for _, want := range []string{"Prefilled from MQTT discovery:", "<b>Shed Light</b>", "tasmota-5F3A21-6689", `name="prefilled"`, `href="/tf/devices/new">Clear</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("prefill lacks %q", want)
		}
	}
	if tag := inputTag(t, body, "address"); !strings.Contains(tag, `value="10.0.0.50"`) {
		t.Errorf("address not prefilled: %s", tag)
	}
	manual := body[strings.Index(body, `<form class="add-manual"`):]
	manual = manual[:strings.Index(manual, "</form>")]
	if strings.Contains(manual, `name="use_default_password"`) || strings.Contains(body, "fleet-fake-pw") {
		t.Error("a prefilled form must not ask for the default password")
	}
	_, plain := e.do(t, "GET", "/devices/new", "admin", nil)
	plain = plain[strings.Index(plain, `<form class="add-manual"`):]
	if !strings.Contains(plain, `name="use_default_password" value="1"`) || !strings.Contains(plain, "Prefilled from Settings → Devices.") {
		t.Error("a typed address keeps the default password (N20)")
	}
	form := url.Values{"address": {"10.0.0.50"}, "prefilled": {"MQTT discovery"}, "use_default_password": {"1"}, "skip_probe": {"1"}}
	if r, _ := e.do(t, "POST", "/devices/new", "admin", form); r.StatusCode != 303 || e.inv.last.HTTPPassword != "" {
		t.Fatalf("the default went to a discovered address: %d %q", r.StatusCode, e.inv.last.HTTPPassword)
	}
	// A proposal that is not a new device (an address change) prefills nothing.
	_, body = e.do(t, "GET", "/devices/new?proposal=2", "admin", nil)
	if strings.Contains(body, "Prefilled from MQTT") || strings.Contains(body, "Prefilled from network") {
		t.Error("an address change must not prefill the add form")
	}
}

// Why: "Add N selected" and "Ignore selected" act on the ticked rows only, each
// accept still tied to the version the admin saw; a failure is reported with
// the device's name and the others are still added.
func TestDiscoveredSelectionAddsAndIgnores(t *testing.T) {
	e, fd := setupDiscovery(t)
	fd.props = append(fd.props, discovery.Proposal{ID: 7, Version: "v7", Kind: discovery.KindNew, Via: "scan", SeenAt: time.Now(), Device: model.Device{Name: "porch", Address: "10.0.0.57", MAC: "aa:bb:cc:dd:ee:07"}})
	r, _ := e.do(t, "POST", "/discovery/proposals/accept", "admin", url.Values{"id": {"1:v1", "7:v7"}})
	if r.StatusCode != 303 || len(fd.accepted) != 2 || fd.versions[0] != "v1" || fd.versions[1] != "v7" {
		t.Fatalf("add selected: %d accepted %v versions %v", r.StatusCode, fd.accepted, fd.versions)
	}
	// An address change cannot be added as a new device through the selection.
	r, body := e.do(t, "POST", "/discovery/proposals/accept", "admin", url.Values{"id": {"2:v2"}})
	if r.StatusCode != 409 || len(fd.accepted) != 2 || !strings.Contains(body, "0 devices added, 1 not") {
		t.Fatalf("address change via selection: %d %v", r.StatusCode, fd.accepted)
	}
	if r, _ := e.do(t, "POST", "/discovery/proposals/accept", "admin", url.Values{}); r.StatusCode != 422 {
		t.Errorf("nothing selected: %d", r.StatusCode)
	}
	r, _ = e.do(t, "POST", "/discovery/proposals/dismiss", "admin", url.Values{"id": {"1:v1", "7:v7"}})
	if r.StatusCode != 303 || len(fd.dismissed) != 2 {
		t.Fatalf("ignore selected: %d %v", r.StatusCode, fd.dismissed)
	}
	for _, path := range []string{"/discovery/proposals/accept", "/discovery/proposals/dismiss"} {
		if r, _ := e.do(t, "POST", path, "operator", url.Values{"id": {"1:v1"}}); r.StatusCode != 403 {
			t.Errorf("%s as operator: %d", path, r.StatusCode)
		}
	}
}

// Why: "Add N selected" must only add what the admin saw. An id without its
// version ("7" or "7:") made AcceptVersion skip the stale-proposal check, so a
// forged or outdated form could add a device whose proposal had changed since
// (for example a spoofed MQTT announcement that took over the address). Like a
// single accept, the selection is refused as out of date and nothing is added.
func TestDiscoveredSelectionRefusesAnIDWithoutVersion(t *testing.T) {
	e, fd := setupDiscovery(t)
	for _, ids := range [][]string{{"1"}, {"1:"}, {"1:v1", "7"}} {
		r, body := e.do(t, "POST", "/discovery/proposals/accept", "admin", url.Values{"id": ids})
		if r.StatusCode != 422 || !strings.Contains(body, "The form is out of date.") {
			t.Errorf("ids %v: %d, want 422 out of date", ids, r.StatusCode)
		}
	}
	if len(fd.accepted) != 0 {
		t.Fatalf("accepted without a version: %v (versions %q)", fd.accepted, fd.versions)
	}
}

// Why: with MQTT discovery off only scans find devices; the tab must say so
// (and link the setting) instead of waiting for announcements that never come.
func TestDiscoveredSaysWhenMQTTDiscoveryIsOff(t *testing.T) {
	_, fd := setupDiscovery(t)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.MQTT = func() string { return "disabled" }
	})
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	if !strings.Contains(body, "<b>MQTT discovery is switched off.</b> Only network scans find new devices.") {
		t.Errorf("no discovery-off notice: %s", body)
	}
	e2 := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.MQTT = func() string { return "connected" }
	})
	_, body = e2.do(t, "GET", "/devices/new", "admin", nil)
	if strings.Contains(body, "MQTT discovery is switched off") {
		t.Error("the notice shows while discovery listens")
	}
}

// Why: a device with SetOption128 0 that is reached by an address that is not
// its hostname or IP answers nothing to /cm. "No device answered" would send
// the person to the network; the page must name the setting (issue #63).
func TestManualAddRefusedHTTPAPINamesSetOption128(t *testing.T) {
	e := setup(t)
	e.pr.err = fmt.Errorf("probe: %w", tasmota.ErrHTTPAPIRefused)
	_, body := e.do(t, "POST", "/devices/new", "admin", url.Values{"address": {"10.0.0.9"}})
	if !strings.Contains(body, `id="err-address"`) || !strings.Contains(body, "SetOption128 1") {
		t.Errorf("no SetOption128 hint under the Address field: %s", body)
	}
	if strings.Contains(body, "No device answered") {
		t.Error("the generic unreachable sentence hides the cause")
	}
}

// ---- the Add page (#410) ----

// Why: #410 replaced the four tabs by one page, so there are fewer clicks: all four ways to add
// devices must be on /devices/new, Discovered and Network scan in the wide column and Add manually
// and Import in the narrow one, and on a phone the reading order is Discovered, Add manually,
// Network scan, Import (rank classes). A box of a service that is off is left out.
func TestAddPageHoldsFourBoxesInTwoStacks(t *testing.T) {
	_, fd := setupDiscovery(t)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.Import = importer.New(importer.Options{Inventory: &checkingInv{}})
	})
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	wide := strings.Index(body, `<div class="cstack cstack-w2">`)
	narrow := strings.Index(body[wide+1:], `<div class="cstack">`) + wide + 1
	if wide < 0 || narrow <= wide {
		t.Fatalf("no wide stack before a narrow one: %d %d", wide, narrow)
	}
	at := func(id string) int { return strings.Index(body, `id="card-`+id+`"`) }
	for _, c := range []struct {
		id   string
		rank string
		in   [2]int
	}{{"discovered", "r1", [2]int{wide, narrow}}, {"scan", "r3", [2]int{wide, narrow}}, {"manual", "r2", [2]int{narrow, len(body)}}, {"import", "r4", [2]int{narrow, len(body)}}} {
		i := at(c.id)
		if i < c.in[0] || i > c.in[1] {
			t.Errorf("box %s is not in its stack (%d not in %v)", c.id, i, c.in)
		}
		if !strings.Contains(body[i-60:i], c.rank) {
			t.Errorf("box %s lacks its phone rank %s: %s", c.id, c.rank, body[i-60:i])
		}
	}
	if strings.Contains(body, `aria-label="Add devices"`) || strings.Contains(body, `class="utabs"`) {
		t.Error("the Add page still has tabs")
	}
	// without discovery and import only the manual form is left
	_, plain := setup(t).do(t, "GET", "/devices/new", "admin", nil)
	for _, gone := range []string{`id="card-discovered"`, `id="card-scan"`, `id="card-import"`} {
		if strings.Contains(plain, gone) {
			t.Errorf("a box of a service that is off: %s", gone)
		}
	}
	if !strings.Contains(plain, `id="card-manual"`) {
		t.Error("no manual box")
	}
}

// Why: the tab routes are gone for good (no redirects before 1.0, maintainer's rule); a request to
// one answers the app's own not-found page, so a stale bookmark is visibly stale.
func TestOldAddTabRoutesAreGone(t *testing.T) {
	_, fd := setupDiscovery(t)
	e := setupWith(t, func(d *Deps) {
		d.Discovery = fd
		d.Import = importer.New(importer.Options{Inventory: &checkingInv{}})
	})
	for _, p := range []string{"/discovery", "/import/tasmoadmin"} {
		if r, _ := e.do(t, "GET", p, "admin", nil); r.StatusCode != 404 {
			t.Errorf("GET %s: %d, want 404", p, r.StatusCode)
		}
	}
}

// Why: the Network scan box is the short way: a range, Scan now and one line about the last scan
// that says WHEN it ran (a day stamp: the last scan can be days old, a bare clock time would lie)
// with a link to all results, where the table and the live stream are.
func TestScanBoxSummarisesTheLastScanAndLinksItsResults(t *testing.T) {
	now := time.Now()
	fd := &fakeDiscovery{
		scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanDone, Total: 254, Done: 254,
			StartedAt: now.Add(-72 * time.Hour), FinishedAt: now.Add(-72*time.Hour + 6*time.Second),
			Hits: []discovery.Hit{{Address: "10.0.0.50", Port: 80, MAC: "aa:bb:cc:dd:ee:01", Name: "n", Outcome: "new proposal"}}},
	}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	box := body[strings.Index(body, `id="card-scan"`):]
	box = box[:strings.Index(box, "</section>")]
	for _, want := range []string{`name="cidrs"`, ">Scan now</button>", `data-ts="day"`, "took 6s",
		"254 addresses · 1 Tasmota device: 0 already known, 0 new (in Discovered), 0 ignored.", `href="/tf/discovery/scan/abc123">All results</a>`} {
		if !strings.Contains(box, want) {
			t.Errorf("scan box lacks %q: %s", want, box)
		}
	}
	if regexp.MustCompile(`Last scan [^<]*\d\d:\d\d:\d\d`).MatchString(box) {
		t.Error("the moment of the last scan is a bare clock time")
	}
	if strings.Contains(box, "<table") {
		t.Error("the results table belongs on the results page")
	}
}

// Why: a scan that runs is watched on the results page (live stream); the box must not offer a second
// start and says where to watch.
func TestScanBoxOfARunningScanLinksTheLiveView(t *testing.T) {
	fd := &fakeDiscovery{scan: discovery.ScanStatus{ID: "abc123", State: discovery.ScanRunning, Total: 254, Done: 40, StartedAt: time.Now()}}
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	_, body := e.do(t, "GET", "/devices/new", "admin", nil)
	box := body[strings.Index(body, `id="card-scan"`):]
	box = box[:strings.Index(box, "</section>")]
	if !strings.Contains(box, "40 of 254 addresses") || !strings.Contains(box, `href="/tf/discovery/scan/abc123">Watch the scan</a>`) {
		t.Errorf("no way to watch the running scan: %s", box)
	}
	if !regexp.MustCompile(`<button type="submit" class="btn btn-primary" disabled[^>]*>Scan now</button>`).MatchString(box) {
		t.Error("Scan now is offered while a scan runs")
	}
}

// Why: the trail of the Add page and of the results page: Devices / Add, and Devices / Add / Network
// scan, so the way back is one click; the list page keeps its heading "Devices" but the menu and the
// trail call it List.
func TestTrailsOfListAddAndScanResults(t *testing.T) {
	_, fd := setupDiscovery(t)
	e := setupWith(t, func(d *Deps) { d.Discovery = fd })
	crumbs := func(path string) string {
		_, body := e.do(t, "GET", path, "admin", nil)
		i := strings.Index(body, `<nav class="crumbs"`)
		return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(body[i:i+strings.Index(body[i:], "</nav>")], "")
	}
	for path, want := range map[string]string{
		"/devices":               "Devices/List",
		"/devices/new":           "Devices/Add",
		"/discovery/scan":        "Devices/Add/Network scan",
		"/discovery/scan/abc123": "Devices/Add/Network scan",
	} {
		if got := strings.Join(strings.Fields(crumbs(path)), ""); got != strings.ReplaceAll(want, " ", "") {
			t.Errorf("%s: trail %q, want %q", path, got, want)
		}
	}
	_, body := e.do(t, "GET", "/devices", "admin", nil)
	if !strings.Contains(body, "<h1>Devices</h1>") {
		t.Error("the list page lost its heading")
	}
}
