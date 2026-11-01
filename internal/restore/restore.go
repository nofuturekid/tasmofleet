// Package restore puts a stored config dump (.dmp) back onto a device.
//
// Two ways reach the device (spec §12.4). Over HTTP tasmofleet does what the web UI's "Restore
// configuration" does: GET /rs, then the multipart POST of the dump to /u2 (#430; official builds
// have no WebGetConfig, so the device cannot fetch the dump itself). Over MQTT (a device HTTP
// cannot reach) it pushes the dump with FileUpload Type 2. Neither needs the firmware listener.
//
// A restore is: automatic backup of the current config, identity check, the upload, wait for the
// restart, fresh backup, diff against the restored backup.
package restore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// Errors returned by Start.
var (
	ErrBusy           = errors.New("restore: another operation on this device is running")
	ErrNotDump        = errors.New("restore: only a config dump (.dmp) can be restored")
	ErrNewerBackup    = errors.New("restore: the backup comes from a newer firmware than the device runs")
	ErrNoAddress      = errors.New("restore: the device address does not resolve to exactly one IP")
	ErrNotConfigured  = errors.New("restore: neither the HTTP restore upload nor a restore over MQTT is available")
	ErrNoPath         = errors.New("restore: no way to reach the device")
	ErrIdentity       = errors.New("restore: the device at this address is not the inventory device")
	ErrVersionUnknown = errors.New("restore: the firmware version of the device or of the backup is unknown")
	ErrFamilyMismatch = errors.New("restore: the backup comes from another chip family")
	ErrOTAActive      = errors.New("restore: a firmware update of this device is active")
)

// OverrideError is returned when a restore needs the admin's explicit
// confirmation for risks that are not certain: StartOptions waives exactly
// the listed ones, nothing else.
type OverrideError struct {
	Major  bool // firmware major version differs
	Family bool // chip family of the backup could not be checked
	// Lockout: a restore over MQTT whose backup changes the device's MQTT settings (or cannot be
	// compared), which could cut TasmoFleet off from a device it reaches only over MQTT.
	Lockout bool
	// MAC is set when the device has no stored MAC and the admin has not yet
	// confirmed the one it reports (StartOptions.ExpectMAC).
	MAC    string
	Detail string
}

func (e *OverrideError) Error() string { return "restore: confirmation needed: " + e.Detail }

// StartOptions are the admin's explicit confirmations.
type StartOptions struct {
	AllowMajorChange   bool
	AllowUnknownFamily bool
	AllowMQTTLockout   bool // accept the MQTT lockout risk (restore over MQTT only)
	// ExpectMAC is the MAC the admin saw and confirmed for a device that has
	// no stored MAC (trust on first use). The live MAC must still be this one
	// when the restore starts, or it is refused.
	ExpectMAC string
	// ConfirmedFiles is the number of files the confirmation page said the restore writes first. Files
	// are written only when the plan at run time has exactly that many (a page that showed none, or
	// an MQTT restore, cannot become a restore that writes files); 0 restores the settings only.
	ConfirmedFiles int
}

// Assessment is what Assess found: the risks the admin has to accept.
type Assessment struct {
	Major   bool   // firmware major version differs
	Family  bool   // chip family of the backup could not be checked
	Lockout bool   // a restore over MQTT whose backup changes the device's MQTT settings
	Detail  string // sentence naming them
	// MACUnknown is true when the inventory holds no MAC for the device: the
	// start reads the live MAC and stores it.
	MACUnknown bool
	// LiveMAC is the MAC the device at the address reports when MACUnknown;
	// the page shows it and the admin's confirmation covers exactly this one.
	LiveMAC string
	// Address and Chip describe the device for that sentence (Chip is the family
	// the inventory holds, empty when unknown).
	Address, Chip string
	// ViaMQTT is true when the restore goes over MQTT (FileUpload); PathNote is the sentence
	// the page shows for the path, as transport.Choose gave it.
	ViaMQTT  bool
	PathNote string
	// Files is what the restore does with the backup's files (#324): the list, or the reason they
	// are not written.
	Files FilesPlan
}

// Job is one restore run.
type Job = store.RestoreJob

// Service starts restores and serves the one-time download.
type Service interface {
	// Start validates the request and runs the restore in the background.
	// The returned job is the pending record; follow it with Get.
	Start(ctx context.Context, actor string, backup model.BackupID) (Job, error)
	// StartWith is Start with the admin's confirmations for the risks an
	// OverrideError names.
	StartWith(ctx context.Context, actor string, backup model.BackupID, o StartOptions) (Job, error)
	// Assess runs the read-only part of the checks against the live device
	// and reports the risks StartWith would ask the admin to accept, or the
	// refusal it would give. The confirmation page shows them up front, so
	// one confirmation covers everything.
	Assess(ctx context.Context, backup model.BackupID) (Assessment, error)
	Get(ctx context.Context, id store.RestoreID) (Job, error)
	List(ctx context.Context, device model.DeviceID, limit int) ([]Job, error)
}

// DeviceGetter is the part of the inventory the service needs.
type DeviceGetter interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
}

// Backups is the part of backup.Service the restore flow uses.
type Backups interface {
	Backup(ctx context.Context, actor string, id model.DeviceID) ([]model.Backup, error)
	// BackupOf backs up src (hardware that is not yet the inventory entry
	// owner) and files the result under owner, marking the dump with note.
	BackupOf(ctx context.Context, actor string, owner model.DeviceID, src model.Device, note string) ([]model.Backup, error)
	List(ctx context.Context, id model.DeviceID) ([]model.Backup, error)
	Open(ctx context.Context, id model.BackupID) ([]byte, model.Backup, error)
}

// Options configure the service.
type Options struct {
	Devices DeviceGetter
	Backups Backups
	Store   store.RestoreStore
	Client  tasmota.Client
	// Client also carries the HTTP restore upload (tasmota.ConfigRestorer, the Router in
	// production); a client without it has no restore over HTTP.
	//
	// Uploader sends the dump with FileUpload (the Router); nil: no restore over MQTT.
	Uploader transport.ConfigUploader
	// PathFor says how a restore reaches the device (transport.Choose on the live facts): HTTP
	// (restore upload), MQTT (FileUpload) or None with the sentence why. The second result is the
	// path's reason, shown on the confirmation page. nil: always HTTP.
	PathFor func(ctx context.Context, d model.Device) (transport.Path, string)
	// Files reads the files archive of a backup (backup.Manager). Optional: nil restores settings only.
	Files FileArchives
	Audit audit.Recorder // optional
	Clock clock.Clock    // default clock.Real
	Log   *slog.Logger
	// Context bounds the background runs; default context.Background.
	Context context.Context
	// PollInterval is how often the device is polled for the reboot (default 2 s).
	PollInterval time.Duration
	// RebootTimeout bounds the wait for the restart (default 3 minutes).
	RebootTimeout time.Duration
	// Resolve turns a device address into IPs (default: IP literal or DNS).
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// Locks is the per-device operation lock shared with OTA, settings and
	// console; default: a private set.
	Locks *devlock.Locks
	// Jobs lists active OTA jobs (store.JobStore); optional.
	Jobs ActiveJobs
	// BrokerPassword is TasmoFleet's live broker password, which a device in broker mode uses as its
	// MqttPassword; the restore compares it with the backup's. Optional.
	BrokerPassword func() string
	// Hardware stores a MAC that was missing (restore) and swaps the entry
	// onto new hardware (replace). Optional: without it a device without a
	// stored MAC is refused and Replace is off.
	Hardware Hardware
	// Describe reads the identity (MAC set, chip family, firmware) of a
	// device that is not in the inventory (app.Prober.Describe). Optional:
	// without it Replace is off.
	Describe func(ctx context.Context, d model.Device) (model.Device, error)
}

// Hardware is the part of the store that records which MACs an entry stands
// for (store.HardwareStore plus the lookups the replacement checks need).
type Hardware interface {
	store.HardwareStore
	GetDeviceByMAC(ctx context.Context, mac string) (model.Device, error)
	ListDevices(ctx context.Context) ([]model.Device, error)
}

// ActiveJobs is the part of store.JobStore the restore uses.
type ActiveJobs interface {
	ActiveJobs(ctx context.Context) ([]model.Job, error)
}

// Manager implements Service.
type Manager struct {
	o  Options
	wg sync.WaitGroup

	mu        sync.Mutex
	replacing map[string]bool // MACs of replacement hardware a replace is working on

	progress      map[store.RestoreID]*progress // per-file progress of the latest jobs (files.go)
	progressOrder []store.RestoreID
}

var _ Service = (*Manager)(nil)

// New returns the restore service.
func New(o Options) (*Manager, error) {
	if o.Devices == nil || o.Backups == nil || o.Store == nil || o.Client == nil {
		return nil, errors.New("restore: Devices, Backups, Store and Client are required")
	}
	if _, ok := o.Client.(tasmota.ConfigRestorer); !ok && o.Uploader == nil {
		return nil, ErrNotConfigured
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Context == nil {
		o.Context = context.Background()
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.RebootTimeout <= 0 {
		o.RebootTimeout = 3 * time.Minute
	}
	if o.Resolve == nil {
		o.Resolve = resolveHost
	}
	if o.Locks == nil {
		o.Locks = devlock.New()
	}
	return &Manager{o: o, replacing: map[string]bool{}}, nil
}

// Wait blocks until all background restores have finished (shutdown, tests).
func (m *Manager) Wait() { m.wg.Wait() }

func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{a.Unmap()}, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i := range ips {
		ips[i] = ips[i].Unmap()
	}
	return ips, nil
}

func (m *Manager) record(ctx context.Context, actor, action string, id store.RestoreID, dev model.DeviceID, result, detail string) {
	if m.o.Audit == nil {
		return
	}
	m.o.Audit.Record(ctx, model.AuditEvent{Actor: actor, Action: action, Target: fmt.Sprintf("device:%d", dev),
		Result: result, Detail: fmt.Sprintf(`{"restore":%d,"info":%q}`, id, detail)})
}

// Start implements Service.
func (m *Manager) Start(ctx context.Context, actor string, bid model.BackupID) (Job, error) {
	return m.StartWith(ctx, actor, bid, StartOptions{})
}

// StartWith implements Service. Everything that can be checked before the
// device is touched is checked here, synchronously, so the admin gets the
// refusal in the response. The address is resolved once; the identity checks and the upload all
// use that one IP.
func (m *Manager) StartWith(ctx context.Context, actor string, bid model.BackupID, opt StartOptions) (Job, error) {
	dmp, b, err := m.o.Backups.Open(ctx, bid)
	if err != nil {
		return Job{}, err
	}
	if b.Kind != model.BackupConfigDump {
		return Job{}, ErrNotDump
	}
	dev, err := m.o.Devices.GetDevice(ctx, b.DeviceID)
	if err != nil {
		return Job{}, err
	}
	path, _, err := m.route(ctx, dev)
	if err != nil {
		return Job{}, err
	}
	ctx = transport.WithPath(ctx, path) // identity and version checks go over the same path
	if path != transport.MQTT {
		ips, err := m.o.Resolve(ctx, dev.Address)
		if err != nil || len(distinct(ips)) != 1 {
			return Job{}, ErrNoAddress
		}
		dev.Address = distinct(ips)[0].String() // from here on the name is never used again
	}

	release, holder, ok := m.o.Locks.TryAcquire(dev.ID, "restore")
	if !ok {
		return Job{}, fmt.Errorf("%w (%s)", ErrBusy, holder)
	}
	started := false
	defer func() {
		if !started {
			release()
		}
	}()
	if err := m.noActiveOTA(ctx, dev.ID); err != nil {
		return Job{}, err
	}
	if err := m.preflight(ctx, actor, &dev, b, opt, true); err != nil {
		return Job{}, err
	}

	job0 := Job{DeviceID: dev.ID, BackupID: bid, Actor: actor}
	if path == transport.MQTT {
		job0.Transport = model.TransportMQTT
	}
	id, err := m.o.Store.CreateRestore(ctx, job0)
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
	m.record(ctx, actor, "restore.start", id, dev.ID, "ok", fmt.Sprintf("backup %d (%s)", bid, b.Version))
	started = true
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer release()
		m.run(m.o.Context, job, dev, dmp, opt.ConfirmedFiles)
	}()
	return job, nil
}

// route decides how this restore reaches the device and checks that the service can do it. The
// second result is the path's reason for the confirmation page.
func (m *Manager) route(ctx context.Context, dev model.Device) (transport.Path, string, error) {
	p, why := transport.HTTP, "over the device's HTTP API (restore upload)"
	if m.o.PathFor != nil {
		p, why = m.o.PathFor(ctx, dev)
	}
	if p == transport.MQTT {
		if m.o.Uploader == nil {
			return p, why, ErrNotConfigured
		}
		return transport.MQTT, why, nil
	}
	if p == transport.HTTP {
		if _, ok := m.o.Client.(tasmota.ConfigRestorer); !ok {
			return p, why, ErrNotConfigured
		}
		return transport.HTTP, why, nil
	}
	return transport.None, why, fmt.Errorf("%w: %s", ErrNoPath, why)
}

func distinct(in []netip.Addr) []netip.Addr {
	var out []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, a := range in {
		a = a.Unmap()
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func (m *Manager) noActiveOTA(ctx context.Context, dev model.DeviceID) error {
	if m.o.Jobs == nil {
		return nil
	}
	jobs, err := m.o.Jobs.ActiveJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.DeviceID == dev {
			return ErrOTAActive
		}
	}
	return nil
}

// Get implements Service.
func (m *Manager) Get(ctx context.Context, id store.RestoreID) (Job, error) {
	return m.o.Store.GetRestore(ctx, id)
}

// List implements Service.
func (m *Manager) List(ctx context.Context, device model.DeviceID, limit int) ([]Job, error) {
	return m.o.Store.ListRestores(ctx, device, limit)
}
