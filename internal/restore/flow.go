package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// step stores a state change; a failure to store is logged, the run goes on.
func (m *Manager) step(ctx context.Context, j *Job, state, detail string) {
	j.State, j.Detail = state, detail
	// A restore cancelled by a shutdown must still record how it ended: the store write outlives the run's context.
	if err := m.o.Store.UpdateRestore(context.WithoutCancel(ctx), *j); err != nil {
		m.o.Log.Error("restore: store state", "component", "restore", "job", j.ID, "err", err)
	}
}

func (m *Manager) fail(ctx context.Context, j *Job, why string) {
	m.step(ctx, j, store.RestoreFailed, why)
	m.record(ctx, j.Actor, "restore.finish", j.ID, j.DeviceID, "error: "+why, "")
}

// run is the whole restore of one job. Order matters: the safety backup comes
// first and a failure there stops everything, because without it a bad
// restore cannot be undone.
func (m *Manager) run(ctx context.Context, j Job, dev model.Device, dmp []byte, confirmedFiles int) {
	target := tasmota.TargetFor(dev)
	ctx = backup.WithRun(ctx, backup.Run{Trigger: model.TriggerRestore})
	if j.Transport == model.TransportMQTT {
		ctx = transport.WithPath(ctx, transport.MQTT) // the safety backup, the checks and the verification too
	} else {
		ctx = transport.WithPath(ctx, transport.HTTP) // a restore that began over HTTP never moves to MQTT
	}

	m.step(ctx, &j, store.RestoreBackup, "taking a backup of the current configuration")
	pre, err := m.o.Backups.Backup(ctx, j.Actor, dev.ID)
	if err != nil {
		m.fail(ctx, &j, failPreBackup+err.Error())
		return
	}
	for _, b := range pre {
		if b.Kind == model.BackupConfigDump {
			j.PreBackupID = b.ID
		}
	}

	viaMQTT := j.Transport == model.TransportMQTT
	fo, err := m.filesStep(ctx, &j, target, func(c context.Context) (string, error) { return m.liveVersion(c, dev) }, viaMQTT,
		func() error { return m.checkIdentity(ctx, dev) }, confirmedFiles)
	if err != nil {
		m.failFiles(ctx, &j, err)
		return
	}
	m.send(ctx, &j, dev, target, dmp, fo)
}

// send is the upload half of a restore: identity, the boot count before, the upload (the HTTP restore upload, or FileUpload over MQTT), wait for the restart. Over both
// paths a refused or aborted upload leaves the device unchanged: over HTTP the device answers its
// "Failed" page and keeps running, over MQTT it applies the dump only after the MD5 matched.
func (m *Manager) send(ctx context.Context, j *Job, dev model.Device, target tasmota.Target, dmp []byte, fo *filesOutcome) {
	viaMQTT := j.Transport == model.TransportMQTT
	if viaMQTT {
		m.step(ctx, j, store.RestoreSending, "uploading the configuration over MQTT")
	} else {
		m.step(ctx, j, store.RestoreSending, "uploading the configuration")
	}
	// The address may have changed hands since Start (DHCP): the dump of this device must never
	// go to another one.
	if err := m.checkIdentity(ctx, dev); err != nil {
		m.fail(ctx, j, fo.afterFiles(failUnchanged+err.Error()))
		return
	}
	before, err := m.bootCount(ctx, target)
	if err != nil {
		m.fail(ctx, j, fo.afterFiles(failUnreachable+err.Error()))
		return
	}
	began := m.o.Clock.Now()
	if viaMQTT {
		if err := m.o.Uploader.UploadConfig(ctx, target, dmp, transport.BindingOf(dev)); err != nil {
			// A lost "Done" is an unknown outcome: the device may have applied the dump and be
			// restarting. Look at the device instead of failing; the upload is never repeated.
			if !errors.Is(err, mqttcmd.ErrOutcomeUnknown) {
				m.fail(ctx, j, fo.afterFiles(failUpload+err.Error()))
				return
			}
			m.o.Log.Warn("restore: FileUpload outcome unknown, checking the device", "component", "restore", "job", j.ID)
		}
	} else if err := m.o.Client.(tasmota.ConfigRestorer).RestoreConfig(ctx, target, dmp); err != nil {
		// The "Failed" page and an upload that never left are certain; anything else (a timeout,
		// a lost or unreadable reply) may come after the device applied the dump: look at the
		// device, never send it again.
		if errors.Is(err, tasmota.ErrUploadRefused) || errors.Is(err, tasmota.ErrNotSent) {
			m.fail(ctx, j, fo.afterFiles(failRestoreUpload+err.Error()))
			return
		}
		m.o.Log.Warn("restore: upload outcome unknown, checking the device", "component", "restore", "job", j.ID, "err", err)
	}
	m.step(ctx, j, store.RestoreWaiting, "waiting for the restart")
	if err := m.waitBoot(ctx, target, before, began); err != nil {
		m.fail(ctx, j, err.Error())
		return
	}
	m.finish(ctx, *j, dev, dmp, fo)
}

// bootCount reads Status 1 (a fresh answer from the device, not the INFO3 of an earlier boot).
func (m *Manager) bootCount(ctx context.Context, t tasmota.Target) (int, error) {
	raw, err := m.o.Client.Command(ctx, t, "Status 1")
	if err != nil {
		return 0, err
	}
	var s struct {
		StatusPRM struct{ BootCount *int }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusPRM.BootCount == nil {
		return 0, tasmota.ErrBadResponse
	}
	return *s.StatusPRM.BootCount, nil
}

// waitBoot returns when Status 1 answers with a larger BootCount than before the upload and
// Status 11 shows an uptime shorter than the time since the upload began (the device really
// restarted since: a count that rose for another reason, or a stale answer, is not a restart). A
// restore does not bring the backup's BootCount back: it sits in the first 16 bytes of the
// settings, which SettingsConfigRestore does not copy (v16 settings.ino 557, tasmota_types.h 490).
// The firmware counts the boot at 10 s of uptime, so the count rises a few polls after the device
// answers again. While the device restarts the poll fails (LWT Offline, refused or timed out);
// that just means "not yet" until the deadline.
func (m *Manager) waitBoot(ctx context.Context, t tasmota.Target, before int, began time.Time) error {
	deadline := m.o.Clock.Now().Add(m.o.RebootTimeout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.o.Clock.After(m.o.PollInterval):
		}
		if n, err := m.bootCount(ctx, t); err == nil && n > before {
			if up, uerr := m.uptime(ctx, t); uerr == nil && time.Duration(up)*time.Second <= m.o.Clock.Now().Sub(began)+2*time.Second {
				return nil
			}
		}
		if m.o.Clock.Now().After(deadline) {
			return errors.New("the device did not restart in time (its boot count did not change); the configuration may or may not have been applied")
		}
	}
}

// finish takes the fresh backup and compares it with the restored one.
func (m *Manager) finish(ctx context.Context, j Job, dev model.Device, dmp []byte, fo *filesOutcome) {
	m.step(ctx, &j, store.RestoreVerifying, "taking a fresh backup to verify")
	if err := m.checkIdentity(ctx, dev); err != nil {
		m.fail(ctx, &j, failAfterRestore+err.Error())
		return
	}
	detail, verified, changes, err := m.verify(ctx, j, dmp, func() ([]model.Backup, error) {
		return m.o.Backups.Backup(ctx, j.Actor, j.DeviceID)
	}, fo.check)
	if err != nil {
		m.fail(ctx, &j, failAfterRestore+"verification failed: "+err.Error())
		return
	}
	j.Verified, j.Changes = verified, changes
	detail = fo.withSummary(detail)
	m.step(ctx, &j, store.RestoreDone, detail)
	m.record(ctx, j.Actor, "restore.finish", j.ID, j.DeviceID, "ok", detail)
}

// failFiles ends the job with the failure text of the files step.
func (m *Manager) failFiles(ctx context.Context, j *Job, err error) {
	var ff *failedFiles
	if errors.As(err, &ff) {
		m.fail(ctx, j, ff.text)
		return
	}
	m.fail(ctx, j, failFilesNone+err.Error())
}

func (m *Manager) uptime(ctx context.Context, t tasmota.Target) (int64, error) {
	raw, err := m.o.Client.Command(ctx, t, "Status 11")
	if err != nil {
		return 0, err
	}
	st, err := tasmota.ParseStatus11(raw)
	if err != nil {
		return 0, err
	}
	return st.UptimeSec, nil
}

// verify takes a fresh backup (take) and compares it with the restored one.
// The detail names paths only, never values.
func (m *Manager) verify(ctx context.Context, j Job, dmp []byte, take func() ([]model.Backup, error), fc *filesCheck) (detail string, verified bool, changes int, err error) {
	fresh, err := take()
	if err != nil {
		return "", false, 0, err
	}
	detail, verified, changes, err = m.verifySettings(ctx, j, dmp, fresh)
	if err != nil || fc == nil {
		return detail, verified, changes, err
	}
	if fd, ok := m.verifyFiles(ctx, fresh, fc); fd != "" {
		detail, verified = detail+"; "+fd, verified && ok
	}
	return detail, verified, changes, nil
}

// verifySettings compares the fresh dump with the restored one.
func (m *Manager) verifySettings(ctx context.Context, j Job, dmp []byte, fresh []model.Backup) (detail string, verified bool, changes int, err error) {
	var freshDmp, freshDec []byte
	for _, b := range fresh {
		switch b.Kind {
		case model.BackupConfigDump:
			freshDmp, _, err = m.o.Backups.Open(ctx, b.ID)
		case model.BackupDecoded:
			freshDec, _, err = m.o.Backups.Open(ctx, b.ID)
		case model.BackupSnapshot, model.BackupFiles:
			// not needed for the comparison
		}
		if err != nil {
			return "", false, 0, err
		}
	}
	if bytes.Equal(freshDmp, dmp) {
		return "restored; the device's configuration dump is identical to the backup", true, 0, nil
	}
	want, err := m.decodedOf(ctx, j.BackupID)
	if err != nil || want == nil || freshDec == nil {
		return "restored; the dumps differ in bytes and no decoded backup is available to compare (decoder missing)", false, 0, nil //nolint:nilerr // not having a decoded twin is a result, not a failure
	}
	diff, err := backup.Diff(want, freshDec)
	if err != nil {
		return "", false, 0, err
	}
	diff = backup.FilterVolatile(diff)
	if len(diff) == 0 {
		return "restored and verified: no differences except counters and checksums", true, 0, nil
	}
	paths := make([]string, 0, 5)
	for i, c := range diff {
		if i == 5 {
			paths = append(paths, "...")
			break
		}
		paths = append(paths, c.Path)
	}
	return fmt.Sprintf("restored, but %d setting(s) differ from the backup: %s", len(diff), strings.Join(paths, ", ")), false, len(diff), nil
}

// decodedOf returns the decoded twin (same device and run) of a dump backup.
func (m *Manager) decodedOf(ctx context.Context, dump model.BackupID) ([]byte, error) {
	_, b, err := m.o.Backups.Open(ctx, dump)
	if err != nil {
		return nil, err
	}
	all, err := m.o.Backups.List(ctx, b.DeviceID)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.Kind == model.BackupDecoded && c.TakenAt.Equal(b.TakenAt) {
			data, _, err := m.o.Backups.Open(ctx, c.ID)
			return data, err
		}
	}
	return nil, nil
}

// normMAC lowers a MAC and removes separators; "" stays "".
func normMAC(s string) string {
	return strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

// colonMAC writes a normalised MAC (12 hex digits) the way the inventory stores
// it, lower case with colons; anything else is returned as it is.
func colonMAC(n string) string {
	if len(n) != 12 {
		return n
	}
	return n[0:2] + ":" + n[2:4] + ":" + n[4:6] + ":" + n[6:8] + ":" + n[8:10] + ":" + n[10:12]
}

// liveMAC reads Status 5. It asks without credentials first, so a host that
// is not the device never sees the stored password; a device with a web
// password answers 401 to that, and only then the stored credentials are
// used (the same host is about to receive them with every command anyway).
func (m *Manager) liveMAC(ctx context.Context, dev model.Device) (string, error) {
	if pin, ok := transport.PathFrom(ctx); ok && pin == transport.MQTT {
		// Over MQTT there is no address to leak a password to, and the router needs the device ID
		// in the target to find its topic (a target without it is a raw HTTP call).
		raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(dev), "Status 5")
		if err != nil {
			return "", err
		}
		return macOf(raw)
	}
	var lastErr error
	for _, withCreds := range []bool{false, true} {
		t := model.Device{Address: dev.Address, Port: dev.Port}
		if withCreds {
			t = dev
		}
		raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(t), "Status 5")
		if errors.Is(err, tasmota.ErrAuth) {
			lastErr = err
			continue
		}
		if err != nil {
			return "", err
		}
		return macOf(raw)
	}
	return "", lastErr
}

// macOf reads the MAC out of a Status 5 reply.
func macOf(raw json.RawMessage) (string, error) {
	var s struct {
		StatusNET struct{ Mac string }
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
		return "", tasmota.ErrBadResponse
	}
	return normMAC(s.StatusNET.Mac), nil
}

// checkIdentity refuses unless the host at the address reports the MAC the
// inventory holds. A device without a stored MAC cannot be checked and is
// refused (probe it first).
func (m *Manager) checkIdentity(ctx context.Context, dev model.Device) error {
	want := normMAC(dev.MAC)
	if want == "" {
		return fmt.Errorf("%w: the inventory has no MAC for this device (probe it first)", ErrIdentity)
	}
	got, err := m.liveMAC(ctx, dev)
	if err != nil {
		return fmt.Errorf("device not reachable: %w", err)
	}
	if got != want && !slices.ContainsFunc(dev.MACs, func(m string) bool { return normMAC(m) == got }) {
		return ErrIdentity // not the MAC of any interface of the stored device
	}
	return nil
}

// liveVersion reads the firmware version from Status 2.
func (m *Manager) liveVersion(ctx context.Context, dev model.Device) (string, error) {
	raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(dev), "Status 2")
	if err != nil {
		return "", err
	}
	fw, err := tasmota.ParseStatus2(raw)
	if err != nil {
		return "", err
	}
	return fw.Version, nil
}

var majorRe = regexp.MustCompile(`^\s*v?(\d+)\.`)

func major(v string) (int, bool) {
	m := majorRe.FindStringSubmatch(v)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// preflight checks, against the live device, everything that decides whether
// this dump may be applied. It runs before any state is changed. With adopt,
// a device without a stored MAC gets the live one stored (the start of a
// restore); without it (the read-only assessment) that step is skipped.
func (m *Manager) preflight(ctx context.Context, actor string, dev *model.Device, b model.Backup, opt StartOptions, adopt bool) error {
	if normMAC(dev.MAC) == "" {
		if err := m.adoptMAC(ctx, actor, dev, adopt, opt.ExpectMAC); err != nil {
			return err
		}
	} else if err := m.checkIdentity(ctx, *dev); err != nil {
		return err
	}
	live, err := m.liveVersion(ctx, *dev)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrVersionUnknown, err)
	}
	mj, okD := major(live)
	bm, okB := major(b.Version)
	if !okD || !okB {
		return ErrVersionUnknown
	}
	if backup.CompareVersions(b.Version, live) > 0 {
		return fmt.Errorf("%w (backup %s, device %s)", ErrNewerBackup, b.Version, live)
	}
	fam, err := m.backupFamily(ctx, b)
	if err != nil {
		return err
	}
	var need OverrideError
	var why []string
	if mj != bm && !opt.AllowMajorChange {
		need.Major = true
		why = append(why, fmt.Sprintf("the backup is from firmware %s, the device runs %s", b.Version, live))
	}
	switch {
	case fam == familyUnknown || dev.Identity.Family == model.ChipUnknown:
		if !opt.AllowUnknownFamily {
			need.Family = true
			why = append(why, "the chip family of the backup could not be checked (no decoded backup)")
		}
	case (fam == familyESP8266) != (dev.Identity.Family == model.ChipESP8266):
		return ErrFamilyMismatch
	}
	if pin, ok := transport.PathFrom(ctx); ok && pin == transport.MQTT && !opt.AllowMQTTLockout {
		if risk := m.mqttLockout(ctx, *dev, b); risk != "" {
			need.Lockout = true
			why = append(why, risk)
		}
	}
	if need.Major || need.Family || need.Lockout {
		need.Detail = strings.Join(why, "; ")
		return &need
	}
	return nil
}

// adoptMAC reads the live MAC of a device that has none and, with record,
// stores it. A MAC that belongs to another entry means the host at this
// address is not this device.
func (m *Manager) adoptMAC(ctx context.Context, actor string, dev *model.Device, record bool, expect string) error {
	if m.o.Hardware == nil {
		return fmt.Errorf("%w: the inventory has no MAC for this device (probe it first)", ErrIdentity)
	}
	mac, err := m.liveMAC(ctx, *dev)
	if err != nil {
		return fmt.Errorf("device not reachable: %w", err)
	}
	mac = colonMAC(mac)
	dev.MAC = mac // the assessment reports it; a start stores it below
	if !record {
		return nil
	}
	// Trust on first use: the identity that gets recorded is exactly the one
	// the admin was shown and confirmed, not whatever answers now.
	if normMAC(expect) == "" {
		return &OverrideError{MAC: mac, Detail: fmt.Sprintf("the device at %s reports MAC %s; confirm it as the identity of %s", dev.Address, mac, dev.Name)}
	}
	if normMAC(expect) != normMAC(mac) {
		return fmt.Errorf("%w: the device at the address no longer reports the MAC you confirmed", ErrIdentity)
	}
	if err := m.o.Hardware.AdoptMAC(ctx, dev.ID, mac); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: the MAC of the device at this address belongs to another inventory entry", ErrIdentity)
		}
		return err
	}
	if m.o.Audit != nil {
		m.o.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "device.mac_adopted", Target: fmt.Sprintf("device:%d", dev.ID),
			Result: "ok", Detail: fmt.Sprintf(`{"mac":%q,"via":"restore","info":"identity recorded on first restore"}`, mac)})
	}
	return nil
}

// Assess implements Service.
func (m *Manager) Assess(ctx context.Context, bid model.BackupID) (Assessment, error) {
	_, b, err := m.o.Backups.Open(ctx, bid)
	if err != nil {
		return Assessment{}, err
	}
	if b.Kind != model.BackupConfigDump {
		return Assessment{}, ErrNotDump
	}
	dev, err := m.o.Devices.GetDevice(ctx, b.DeviceID)
	if err != nil {
		return Assessment{}, err
	}
	path, note, err := m.route(ctx, dev)
	if err != nil {
		return Assessment{}, err
	}
	ctx = transport.WithPath(ctx, path)
	if path != transport.MQTT {
		ips, err := m.o.Resolve(ctx, dev.Address)
		if err != nil || len(distinct(ips)) != 1 {
			return Assessment{}, ErrNoAddress
		}
		dev.Address = distinct(ips)[0].String()
	}
	a := Assessment{MACUnknown: normMAC(dev.MAC) == "", ViaMQTT: path == transport.MQTT, PathNote: note}
	err = m.preflight(ctx, "", &dev, b, StartOptions{}, false)
	if err == nil || errors.As(err, new(*OverrideError)) {
		a.Files = m.assessFiles(ctx, b, dev, path == transport.MQTT)
	}
	if a.MACUnknown {
		a.LiveMAC, a.Address, a.Chip = dev.MAC, dev.Address, string(dev.Identity.Family)
	}
	var need *OverrideError
	switch {
	case errors.As(err, &need):
		a.Major, a.Family, a.Lockout, a.Detail = need.Major, need.Family, need.Lockout, need.Detail
		return a, nil
	case err != nil:
		return a, err
	}
	return a, nil
}

type family int

const (
	familyUnknown family = iota
	familyESP8266
	familyESP32
)

// backupFamily reads the chip family decode-config recorded in the header of
// the decoded twin of a dump ("ESP82xx" or "ESP32..."). No twin, or no header:
// unknown.
func (m *Manager) backupFamily(ctx context.Context, b model.Backup) (family, error) {
	dec, err := m.decodedOf(ctx, b.ID)
	if err != nil || dec == nil {
		return familyUnknown, nil //nolint:nilerr // a missing twin means "unknown", which needs the admin's confirmation
	}
	var d struct {
		Header struct {
			Data struct {
				Hardware string `json:"hardware"`
			} `json:"data"`
		} `json:"header"`
	}
	if uerr := json.Unmarshal(dec, &d); uerr != nil {
		return familyUnknown, nil //nolint:nilerr // unreadable header means "unknown", which needs the admin's confirmation
	}
	switch hw := strings.ToUpper(d.Header.Data.Hardware); {
	case strings.HasPrefix(hw, "ESP82"):
		return familyESP8266, nil
	case strings.HasPrefix(hw, "ESP32"):
		return familyESP32, nil
	}
	return familyUnknown, nil
}

// assessFiles is the files part of the assessment. A device that cannot be asked now is not a
// refusal: the page says the files could not be checked, the run asks again and fails loudly.
func (m *Manager) assessFiles(ctx context.Context, b model.Backup, dev model.Device, viaMQTT bool) FilesPlan {
	plan, err := m.planFiles(ctx, b.ID, tasmota.TargetFor(dev), func(c context.Context) (string, error) { return m.liveVersion(c, dev) }, viaMQTT)
	if err != nil {
		m.o.Log.Warn("restore: assess files", "component", "restore", "err", err)
		return FilesPlan{Files: plan.Files, Why: "the device's file system could not be read right now, so the files could not be checked"}
	}
	return plan
}
