package restore

// Replace device: the hardware of an inventory entry died. The wizard puts the
// entry's newest backup onto fresh hardware through the same one-time token
// the restore uses, then lets the entry stand for the new MACs. See
// docs/security/restore.md (R13, R18-R21).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Errors returned by PreviewReplace and StartReplace (besides the ones of
// Start: ErrBusy, ErrOTAActive, ErrFamilyMismatch, ErrNewerBackup,
// ErrVersionUnknown, ErrNoAddress).
var (
	ErrReplaceOff        = errors.New("replace: not available")
	ErrOldReachable      = errors.New("replace: the old device still answers at its address")
	ErrCandidateBound    = errors.New("replace: the replacement device is already part of the inventory")
	ErrCandidateIsOld    = errors.New("replace: the replacement device is the device being replaced")
	ErrCandidateOffline  = errors.New("replace: the replacement device is not reachable")
	ErrCandidateOutside  = errors.New("replace: the replacement address is outside the device networks")
	ErrCandidateLocked   = errors.New("replace: the replacement device asks for a password")
	ErrCandidateUnusable = errors.New("replace: the replacement device could not be identified")
	ErrNoBackup          = errors.New("replace: the device has no config dump to restore")
)

// ReplaceRequest names the broken entry, the replacement hardware and the
// backup to put on it.
type ReplaceRequest struct {
	Device  model.DeviceID
	Address string // of the replacement hardware
	Port    int
	// Backup is the dump to restore; 0 means the newest dump of the entry
	// itself (never the backup of replacement hardware).
	Backup model.BackupID
}

// Carries is what the chosen dump would put on the new hardware, read from
// its decoded twin. Passwords and keys are never part of it.
type Carries struct {
	Known       bool // false: no decoded twin, nothing can be shown
	Hostname    string
	StaticIP    string // empty: DHCP
	MQTTTopic   string
	MQTTClient  string
	SSIDs       []string
	Module      int
	HasModule   bool
	HasTemplate bool
	Template    string // name of the user template
	TemplateIOs int    // GPIOs the user template assigns
}

// ReplacePreview is the result of the live checks.
type ReplacePreview struct {
	Old     model.Device // the broken entry
	New     model.Device // the live replacement hardware: address, MAC set, identity
	Backup  model.Backup
	Dumps   []model.Backup // dumps of the entry that could be chosen, newest first
	Carries Carries
	// Files is what the replacement does with the backup's files (#324).
	Files FilesPlan
	// DumpFiles says per dump what it holds of the device's files ("6 files", "no files (went over MQTT)").
	DumpFiles map[model.BackupID]string
	// Major and Family need the admin's tick (StartOptions), Detail says why.
	Major, Family bool
	Detail        string
}

// Replacer is the replace part of the service.
type Replacer interface {
	// Candidates keeps the endpoints that answer, are inside the device
	// networks and are not part of the inventory.
	Candidates(ctx context.Context, eps []Endpoint) []Endpoint
	PreviewReplace(ctx context.Context, req ReplaceRequest) (ReplacePreview, error)
	StartReplace(ctx context.Context, actor string, req ReplaceRequest, o StartOptions) (Job, error)
}

var _ Replacer = (*Manager)(nil)

// Endpoint is a device address as discovery reports it.
type Endpoint struct {
	Address string
	Port    int
	MAC     string // filled by Candidates
}

type replacePlan struct {
	old     model.Device
	cand    model.Device // Address is the resolved IP, Port as requested
	ip      netip.Addr
	oldIP   netip.Addr // where the entry's address resolves; invalid when it does not
	dmp     []byte
	b       model.Backup
	carries Carries
	need    OverrideError
	dumps   []model.Backup
	files   FilesPlan
}

// Candidates implements Replacer. It answers from Status 5 without
// credentials: a candidate is a fresh device, and credentials of the
// inventory never go to a host that is not in it.
func (m *Manager) Candidates(ctx context.Context, eps []Endpoint) []Endpoint {
	if m.o.Hardware == nil {
		return nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var out []Endpoint
	for _, ep := range eps {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			mac, err := m.liveMAC(cctx, model.Device{Address: ep.Address, Port: ep.Port})
			if err != nil || mac == "" {
				return
			}
			if _, err := m.o.Hardware.GetDeviceByMAC(cctx, colonMAC(mac)); !errors.Is(err, store.ErrNotFound) {
				return // bound (or the store failed: do not offer what cannot be checked)
			}
			ep.MAC = colonMAC(mac)
			mu.Lock()
			out = append(out, ep)
			mu.Unlock()
		}()
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b Endpoint) int { return strings.Compare(a.Address, b.Address) })
	return out
}

// PreviewReplace implements Replacer: every check a start does, nothing
// changed on any device.
func (m *Manager) PreviewReplace(ctx context.Context, req ReplaceRequest) (ReplacePreview, error) {
	if m.o.Hardware == nil || m.o.Describe == nil || m.o.BaseURL == "" { // a replacement is restored through the firmware listener
		return ReplacePreview{}, ErrReplaceOff
	}
	p, err := m.planReplace(ctx, req)
	if err != nil {
		return ReplacePreview{}, err
	}
	return ReplacePreview{Old: p.old, New: p.cand, Backup: p.b, Dumps: p.dumps, Carries: p.carries, Files: p.files, DumpFiles: m.dumpFiles(ctx, p.dumps),
		Major: p.need.Major, Family: p.need.Family, Detail: p.need.Detail}, nil
}

// planReplace does all read-only checks, in the order that keeps device
// traffic smallest: inventory facts first, then the replacement hardware,
// then the old address.
func (m *Manager) planReplace(ctx context.Context, req ReplaceRequest) (*replacePlan, error) {
	old, err := m.o.Devices.GetDevice(ctx, req.Device)
	if err != nil {
		return nil, err
	}
	p := &replacePlan{old: old}
	if err := m.chooseBackup(ctx, p, req.Backup); err != nil {
		return nil, err
	}
	ips, err := m.o.Resolve(ctx, req.Address)
	if err != nil || len(distinct(ips)) != 1 {
		return nil, ErrNoAddress
	}
	p.ip = distinct(ips)[0]
	cand, err := m.describeCandidate(ctx, p.ip, req.Port)
	if err != nil {
		return nil, err
	}
	p.cand = cand
	if err := m.checkUnbound(ctx, p); err != nil {
		return nil, err
	}
	if err := m.checkOldSilent(ctx, p); err != nil {
		return nil, err
	}
	if err := m.checkFit(ctx, p); err != nil {
		return nil, err
	}
	p.carries = m.carries(ctx, p.b)
	p.files = m.replaceFilesPlan(ctx, p)
	return p, nil
}

// chooseBackup picks the dump: the requested one, or the newest dump of the
// entry itself. Backups of replacement hardware (earlier, aborted replaces)
// are not the entry's configuration and are never used.
func (m *Manager) chooseBackup(ctx context.Context, p *replacePlan, want model.BackupID) error {
	all, err := m.o.Backups.List(ctx, p.old.ID)
	if err != nil {
		return err
	}
	for _, b := range all {
		if b.Kind == model.BackupConfigDump && !backup.IsReplacementHardware(b) {
			p.dumps = append(p.dumps, b)
		}
	}
	if len(p.dumps) == 0 {
		return ErrNoBackup
	}
	id := p.dumps[0].ID
	if want != 0 {
		if !slices.ContainsFunc(p.dumps, func(b model.Backup) bool { return b.ID == want }) {
			return ErrNotDump // another device's backup, a snapshot, or replacement hardware
		}
		id = want
	}
	p.dmp, p.b, err = m.o.Backups.Open(ctx, id)
	return err
}

// describeCandidate reads the live replacement hardware and maps the ways it
// can be unusable to refusals the page can word.
func (m *Manager) describeCandidate(ctx context.Context, ip netip.Addr, port int) (model.Device, error) {
	d, err := m.o.Describe(ctx, model.Device{Address: ip.String(), Port: port})
	switch {
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return d, ErrCandidateOutside
	case errors.Is(err, tasmota.ErrAuth):
		return d, ErrCandidateLocked
	case errors.Is(err, tasmota.ErrUnreachable), errors.Is(err, tasmota.ErrTimeout):
		return d, ErrCandidateOffline
	case err != nil:
		return d, fmt.Errorf("%w: %w", ErrCandidateUnusable, err)
	}
	d.Address, d.Port = ip.String(), port
	if normMAC(d.MAC) == "" || d.Identity.Family == model.ChipUnknown || d.Identity.Version == "" {
		return d, ErrCandidateUnusable
	}
	return d, nil
}

// checkUnbound refuses hardware that is already an inventory entry: by any of
// its MACs, or by the address (an entry that sits there would clash).
func (m *Manager) checkUnbound(ctx context.Context, p *replacePlan) error {
	for _, mac := range append([]string{p.cand.MAC}, p.cand.MACs...) {
		d, err := m.o.Hardware.GetDeviceByMAC(ctx, mac)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case d.ID == p.old.ID:
			return ErrCandidateIsOld
		default:
			return ErrCandidateBound
		}
	}
	all, err := m.o.Hardware.ListDevices(ctx)
	if err != nil {
		return err
	}
	for _, d := range all {
		if d.ID != p.old.ID && d.Address == p.cand.Address && portOf(d.Port) == portOf(p.cand.Port) {
			return ErrCandidateBound
		}
	}
	return nil
}

func portOf(p int) int {
	if p == 0 {
		return 80
	}
	return p
}

// silent lists the errors that mean "nothing is there".
func silent(err error) bool {
	return errors.Is(err, tasmota.ErrUnreachable) || errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrForbiddenAddr)
}

// checkOldSilent refuses while the old device can still be reached: two
// devices with the same configuration (hostname, MQTT topic, static IP) would
// clash on the network. Something answering that is not the old device (the
// replacement itself on the old address, or a stranger) does not count.
func (m *Manager) checkOldSilent(ctx context.Context, p *replacePlan) error {
	ips, err := m.o.Resolve(ctx, p.old.Address)
	if err != nil {
		return nil //nolint:nilerr // an address that no longer resolves is a device that is gone
	}
	oldMACs := append([]string{p.old.MAC}, p.old.MACs...)
	for _, ip := range distinct(ips) {
		if ip == p.ip && portOf(p.old.Port) == portOf(p.cand.Port) {
			continue // that is the replacement hardware, checked above
		}
		probe := p.old
		probe.Address = ip.String()
		mac, err := m.liveMAC(ctx, probe)
		switch {
		case err == nil:
			if !slices.ContainsFunc(oldMACs, func(s string) bool { return normMAC(s) != "" }) ||
				slices.ContainsFunc(oldMACs, func(s string) bool { return normMAC(s) == mac }) {
				return ErrOldReachable
			}
		case silent(err):
		default:
			return ErrOldReachable // it answered, just not clearly (password, garbage)
		}
		if !p.oldIP.IsValid() {
			p.oldIP = ip
		}
	}
	return nil
}

// checkFit compares the dump with the replacement hardware: chip family
// (final) and firmware (newer dump refused, other major version needs a tick).
func (m *Manager) checkFit(ctx context.Context, p *replacePlan) error {
	cf := p.cand.Identity.Family
	bf, err := m.backupFamily(ctx, p.b)
	if err != nil {
		return err
	}
	if of := p.old.Identity.Family; of != model.ChipUnknown && of != cf {
		return ErrFamilyMismatch
	}
	if bf != familyUnknown && (bf == familyESP8266) != (cf == model.ChipESP8266) {
		return ErrFamilyMismatch
	}
	var why []string
	if bf == familyUnknown && p.old.Identity.Family == model.ChipUnknown {
		p.need.Family = true
		why = append(why, "the chip family of the old device could not be checked (no decoded backup, no detected family)")
	}
	live := p.cand.Identity.Version
	mj, okD := major(live)
	bm, okB := major(p.b.Version)
	if !okD || !okB {
		return ErrVersionUnknown
	}
	if backup.CompareVersions(p.b.Version, live) > 0 {
		return fmt.Errorf("%w (backup %s, new device %s)", ErrNewerBackup, p.b.Version, live)
	}
	if mj != bm {
		p.need.Major = true
		why = append(why, fmt.Sprintf("the backup is from firmware %s, the new device runs %s", p.b.Version, live))
	}
	p.need.Detail = strings.Join(why, "; ")
	return nil
}

// carries reads what the dump sets on the device. Secrets are not read.
func (m *Manager) carries(ctx context.Context, b model.Backup) Carries {
	dec, err := m.decodedOf(ctx, b.ID)
	if err != nil || dec == nil {
		return Carries{}
	}
	var d struct {
		Hostname   string   `json:"hostname"`
		IPAddress  []string `json:"ip_address"`
		MQTTTopic  string   `json:"mqtt_topic"`
		MQTTClient string   `json:"mqtt_client"`
		SSID       []string `json:"sta_ssid"`
		Module     *int     `json:"module"`
		Template   *struct {
			Name string `json:"name"`
			GPIO []int  `json:"gpio"`
		} `json:"user_template"`
	}
	if json.Unmarshal(dec, &d) != nil {
		return Carries{}
	}
	c := Carries{Known: true, Hostname: d.Hostname, MQTTTopic: d.MQTTTopic, MQTTClient: d.MQTTClient}
	if len(d.IPAddress) > 0 && d.IPAddress[0] != "" && d.IPAddress[0] != "0.0.0.0" {
		c.StaticIP = d.IPAddress[0]
	}
	for _, s := range d.SSID {
		if s != "" {
			c.SSIDs = append(c.SSIDs, s)
		}
	}
	if d.Module != nil {
		c.Module, c.HasModule = *d.Module, true
	}
	if d.Template != nil {
		c.HasTemplate, c.Template = true, d.Template.Name
		for _, g := range d.Template.GPIO {
			if g != 0 {
				c.TemplateIOs++
			}
		}
	}
	return c
}

// StartReplace implements Replacer.
func (m *Manager) StartReplace(ctx context.Context, actor string, req ReplaceRequest, opt StartOptions) (Job, error) {
	if m.o.Hardware == nil || m.o.Describe == nil || m.o.BaseURL == "" { // a replacement is restored through the firmware listener
		return Job{}, ErrReplaceOff
	}
	release, holder, ok := m.o.Locks.TryAcquire(req.Device, "replace")
	if !ok {
		return Job{}, fmt.Errorf("%w (%s)", ErrBusy, holder)
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	if err := m.noActiveOTA(ctx, req.Device); err != nil {
		return Job{}, err
	}
	p, err := m.planReplace(ctx, req)
	if err != nil {
		return Job{}, err
	}
	if need := (OverrideError{Major: p.need.Major && !opt.AllowMajorChange, Family: p.need.Family && !opt.AllowUnknownFamily, Detail: p.need.Detail}); need.Major || need.Family {
		return Job{}, &need
	}
	mac := normMAC(p.cand.MAC)
	if !m.claim(mac) {
		return Job{}, ErrBusy
	}
	unclaim := func() { m.unclaim(mac) }
	defer func() {
		if !started {
			unclaim()
		}
	}()
	id, err := m.o.Store.CreateRestore(ctx, Job{DeviceID: p.old.ID, BackupID: p.b.ID, Actor: actor, Kind: store.KindReplace,
		NewMAC: p.cand.MAC, NewAddress: p.cand.Address, NewPort: p.cand.Port})
	if errors.Is(err, store.ErrConflict) {
		return Job{}, ErrBusy
	}
	if err != nil {
		return Job{}, err
	}
	job, err := m.o.Store.GetRestore(ctx, id)
	if err != nil {
		return Job{}, err
	}
	m.recordReplace(ctx, actor, "replace.start", job, "ok", map[string]any{"backup": p.b.ID, "new_mac": p.cand.MAC, "address": p.cand.Address})
	started = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer release()
		defer unclaim()
		m.runReplace(m.o.Context, job, p, opt.ConfirmedFiles)
	}()
	return job, nil
}

func (m *Manager) claim(mac string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.replacing[mac] {
		return false
	}
	m.replacing[mac] = true
	return true
}

func (m *Manager) unclaim(mac string) {
	m.mu.Lock()
	delete(m.replacing, mac)
	m.mu.Unlock()
}

// recordReplace audits a replace step. The detail is JSON built here; it
// carries MACs, addresses and IDs, never credentials or the token.
func (m *Manager) recordReplace(ctx context.Context, actor, action string, j Job, result string, detail map[string]any) {
	if m.o.Audit == nil {
		return
	}
	detail["restore"] = j.ID
	e := model.AuditEvent{Actor: actor, Action: action, Target: fmt.Sprintf("device:%d", j.DeviceID), Result: result}
	if b, err := json.Marshal(detail); err == nil {
		e.Detail = string(b)
	}
	m.o.Audit.Record(ctx, e)
}

func (m *Manager) failReplace(ctx context.Context, j *Job, why string) {
	m.step(ctx, j, store.RestoreFailed, why)
	m.recordReplace(ctx, j.Actor, "replace.finish", *j, "error: "+why, map[string]any{"new_mac": j.NewMAC})
}

// runReplace is the whole replacement of one job: 1) back up the new
// hardware, 2) restore the old device's backup onto it, expecting the NEW
// hardware's MAC, 3) wait for the reboot and verify, 4) let the entry take
// over the new MAC set in one transaction.
func (m *Manager) runReplace(ctx context.Context, j Job, p *replacePlan, confirmedFiles int) {
	ctx = backup.WithRun(ctx, backup.Run{Trigger: model.TriggerReplace})
	bare := model.Device{Address: p.cand.Address, Port: p.cand.Port}
	target := tasmota.TargetFor(bare)

	m.step(ctx, &j, store.RestoreBackup, "taking a backup of the replacement device")
	pre, err := m.o.Backups.BackupOf(ctx, j.Actor, p.old.ID, bare, backup.NoteReplacementHardware)
	if err != nil {
		m.failReplace(ctx, &j, failPreBackupReplace+err.Error())
		return
	}
	for _, b := range pre {
		if b.Kind == model.BackupConfigDump {
			j.PreBackupID = b.ID
		}
	}

	stillThere := func() error {
		// The address may have changed hands since the preview (DHCP).
		if got, err := m.liveMAC(ctx, bare); err != nil || got != normMAC(p.cand.MAC) {
			return errors.New("the device at the address is no longer the replacement device you chose")
		}
		return nil
	}
	fo, ferr := m.filesStep(ctx, &j, target, func(context.Context) (string, error) { return p.cand.Identity.Version, nil }, false, stillThere, confirmedFiles)
	if ferr != nil {
		var ff *failedFiles
		text := failFilesNone + ferr.Error()
		if errors.As(ferr, &ff) {
			text = ff.text
		}
		m.failReplace(ctx, &j, text)
		return
	}
	m.step(ctx, &j, store.RestoreSending, "asking the replacement device to fetch the configuration")
	if err := stillThere(); err != nil {
		m.failReplace(ctx, &j, fo.afterFiles(failUnchanged+err.Error()))
		return
	}
	before, err := m.uptime(ctx, target)
	if err != nil {
		m.failReplace(ctx, &j, fo.afterFiles("replacement "+failUnreachable+err.Error()))
		return
	}
	tok, err := m.issue(j, p.dmp, []netip.Addr{p.ip})
	if err != nil {
		m.failReplace(ctx, &j, fo.afterFiles(failToken+err.Error()))
		return
	}
	defer m.revoke(tok)
	if _, err := m.o.Client.Command(ctx, target, "WebGetConfig "+m.o.BaseURL+"/restore/"+tok); err != nil {
		unknown := m.consumed(tok) || errors.Is(err, tasmota.ErrTimeout) || errors.Is(err, tasmota.ErrBadResponse)
		if !unknown {
			m.failReplace(ctx, &j, fo.afterFiles(failWebGetConfig+redactToken(err.Error(), tok)))
			return
		}
		m.o.Log.Warn("replace: WebGetConfig outcome unknown, checking the device", "component", "restore", "job", j.ID, "err", redactToken(err.Error(), tok))
	}

	m.step(ctx, &j, store.RestoreWaiting, "waiting for the download and the reboot")
	found, err := m.waitReplaced(ctx, p, tok, before)
	if err != nil {
		m.failReplace(ctx, &j, err.Error())
		return
	}

	m.step(ctx, &j, store.RestoreVerifying, "taking a fresh backup to verify")
	detail, verified, changes, verr := m.verify(ctx, j, p.dmp, func() ([]model.Backup, error) {
		return m.o.Backups.BackupOf(ctx, j.Actor, p.old.ID, found, "")
	}, fo.check)
	if verr != nil {
		// The new hardware answered with its own MAC after the reboot: it is
		// there and holds the configuration. Leaving the entry on the dead
		// hardware because the comparison failed would be worse.
		detail, verified, changes = "restored; the verification could not complete: "+verr.Error(), false, 0
	}
	j.Verified, j.Changes = verified, changes

	// The entry keeps the address it has when the new hardware came up there
	// (an unchanged inventory address stays as the admin wrote it, name or IP).
	addr, port := found.Address, found.Port
	if p.oldIP.IsValid() && found.Address == p.oldIP.String() && portOf(found.Port) == portOf(p.old.Port) {
		addr, port = p.old.Address, p.old.Port
	}
	oldMACs, err := m.o.Hardware.SwapHardware(ctx, p.old.ID, store.HardwareSwap{
		ExpectMACs: append([]string{p.old.MAC}, p.old.MACs...),
		MAC:        p.cand.MAC, MACs: p.cand.MACs, Address: addr, Port: port, Identity: p.cand.Identity,
	})
	if err != nil {
		m.failReplace(ctx, &j, failTakeOver+"the inventory entry could not take it over (it still points to the old hardware): "+err.Error())
		return
	}
	j.Detail = fo.withSummary("replaced; " + detail)
	m.step(ctx, &j, store.RestoreDone, j.Detail)
	m.recordReplace(ctx, j.Actor, "replace.finish", j, "ok", map[string]any{
		"old_macs": oldMACs, "new_mac": p.cand.MAC, "address": addr, "verified": verified})
}

// seen reads MAC and uptime of the host at ep. The entry's credentials are
// used only after a 401: the restored configuration brings the old web
// password back.
func (m *Manager) seen(ctx context.Context, ep model.Device) (string, int64, error) {
	bare := model.Device{Address: ep.Address, Port: ep.Port}
	tries := []model.Device{bare}
	if ep.HasCredentials() {
		tries = append(tries, ep)
	}
	var lastErr error
	for _, t := range tries {
		raw, err := m.o.Client.Command(ctx, tasmota.TargetFor(t), "Status 5")
		if errors.Is(err, tasmota.ErrAuth) {
			lastErr = err
			continue
		}
		if err != nil {
			return "", 0, err
		}
		var s struct {
			StatusNET struct{ Mac string }
		}
		if err := json.Unmarshal(raw, &s); err != nil || s.StatusNET.Mac == "" {
			return "", 0, tasmota.ErrBadResponse
		}
		up, err := m.uptime(ctx, tasmota.TargetFor(t))
		return normMAC(s.StatusNET.Mac), up, err
	}
	return "", 0, lastErr
}

// waitReplaced returns the endpoint where the replacement hardware (its own
// MAC) is back after the configuration was served. At its old address it must
// also have restarted (down in between, or a smaller uptime); at any other
// address it can only be the restarted device.
func (m *Manager) waitReplaced(ctx context.Context, p *replacePlan, tok string, before int64) (model.Device, error) {
	creds := func(addr string, port int) model.Device {
		return model.Device{Address: addr, Port: port, HTTPUser: p.old.HTTPUser, HTTPPassword: p.old.HTTPPassword}
	}
	here := creds(p.cand.Address, p.cand.Port)
	others := []model.Device{}
	if p.oldIP.IsValid() && (p.oldIP.String() != here.Address || portOf(p.old.Port) != portOf(here.Port)) {
		others = append(others, creds(p.oldIP.String(), p.old.Port))
	}
	want := normMAC(p.cand.MAC)
	deadline := m.o.Clock.Now().Add(m.o.RebootTimeout)
	sawDown := false
	for {
		select {
		case <-ctx.Done():
			return model.Device{}, ctx.Err()
		case <-m.o.Clock.After(m.o.PollInterval):
		}
		served := m.consumed(tok)
		mac, up, err := m.seen(ctx, here)
		switch {
		case err != nil:
			sawDown = true
		case served && mac == want && (sawDown || up < before):
			return here, nil
		}
		if served {
			for _, o := range others {
				if mac, _, err := m.seen(ctx, o); err == nil && mac == want {
					return o, nil
				}
			}
		}
		if !served && m.o.Clock.Now().After(deadline) {
			return model.Device{}, errors.New("the replacement device never downloaded the configuration (WebGetConfig unsupported or the firmware listener is unreachable for it)")
		}
		if served && m.o.Clock.Now().After(deadline) {
			return model.Device{}, errors.New("the replacement device downloaded the configuration but was not found again in time (it may have joined the network at another address; the inventory entry was not changed)")
		}
	}
}

// replaceFilesPlan is the files part of the preview: what the replacement hardware would get.
// Its file system is read without credentials, like everything else on a device not yet in the inventory.
func (m *Manager) replaceFilesPlan(ctx context.Context, p *replacePlan) FilesPlan {
	t := tasmota.TargetFor(model.Device{Address: p.cand.Address, Port: p.cand.Port})
	plan, err := m.planFiles(ctx, p.b.ID, t, func(context.Context) (string, error) { return p.cand.Identity.Version, nil }, false)
	if err != nil {
		return FilesPlan{Files: plan.Files, Why: "the replacement device's file system could not be read right now, so the files are not copied (only the settings)"}
	}
	return plan
}
