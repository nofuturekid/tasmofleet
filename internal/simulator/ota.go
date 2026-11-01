package simulator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
	"slices"
	"time"
)

// Nominal ESP8266 timeline in seconds (docs/research/tasmota-minimal-and-ota.md
// section 6), multiplied by the fleet's time scale.
const (
	secPending       = 3.0
	secDownloadMin   = 10.0 // blocking download of a ~0.5-1 MB gz image
	secRestartWait   = 7.0  // 2 s after the download + restart_flag 5
	secRebootGap     = 8.0  // bootloader copy + wifi join
	secRetryDelay    = 1.5
	secAutoLoader    = 6.0   // minimal continues on its own after ota_loader
	secLightSettle   = 0.004 // a light shows its old state this long after the boot (4 ms observed on the fleet), then switches itself off
	secLWTBlock      = 45.0  // keepalive 30 s * 1.5: the broker fires the will
	secRestartCmd    = 2.0
	secFWConnectFail = 66.0 // ESPhttpUpdate gives up connecting after 66 s (live, 2026-10-30)
	attemptsESP8266  = 10
	attemptsESP32    = 5
)

var errTooLarge = devErr("Not Enough space")

func (d *simDevice) armLocked(cmd string) {
	d.armed = nil
	for _, f := range d.faults {
		switch f.Kind {
		case FaultDownloadFail, FaultFWUnreachable, FaultStayOldVersion, FaultLongReboot, FaultWrongFamily, FaultNeverReturns:
			if f.matches(cmd) {
				d.armed = append(d.armed, f)
			}
		}
	}
}

func (d *simDevice) armedLocked(kind string) (Fault, bool) {
	for _, f := range d.armed {
		if f.Kind == kind {
			return f, true
		}
	}
	return Fault{}, false
}

// phase0StartLocked moves to PENDING and starts the timeline goroutine.
func (d *simDevice) phase0StartLocked() {
	d.setPhaseLocked(PhasePending)
	d.spawn(func() {
		d.runOTA(false)
	})
}

// runOTA plays pending, download, restart wait and reboot. skipPending is
// the minimal image continuing by itself after the automatic fallback.
func (d *simDevice) runOTA(skipPending bool) {
	if !skipPending {
		if !d.sleep(secPending / 3) {
			return
		}
		d.mu.Lock()
		d.otaLoader = false // flag 3->2 resets ota_loader and runs SettingsSave(1)
		d.commitLocked()
		d.mu.Unlock()
		if !d.sleep(secPending * 2 / 3) {
			return
		}
	}
	if f, ok := d.firmwareUnreachable(); ok {
		d.failUnreachable(f)
		return
	}
	d.mu.Lock()
	d.attempts = 0
	d.setPhaseLocked(PhaseDownloading)
	d.mu.Unlock()
	d.mq.setMuted(true)
	stopLWT := d.mq.startBlockTimer(d.dur(secLWTBlock))
	res, err := d.downloadWithRetries()
	stopLWT()
	if err != nil {
		d.mq.setMuted(false)
		d.mu.Lock()
		d.armed = nil
		d.setPhaseLocked(PhaseRun)
		d.mu.Unlock()
		d.mq.publish("stat/"+d.topic+"/UPGRADE", `{"Upgrade":`+jsonString("Failed "+err.Error())+`}`, false)
		return
	}
	d.mu.Lock()
	d.nextImage = res
	if res.kind != "minimal" && res.kind != "safeboot" {
		d.otaLoader = false
	}
	d.setPhaseLocked(PhaseRestartWait)
	d.mu.Unlock()
	d.mq.setMuted(false)
	d.mq.publish("stat/"+d.topic+"/UPGRADE", `{"Upgrade":"Successful. Restarting"}`, false)
	d.mq.setMuted(true)
	if !d.sleep(secRestartWait) {
		return
	}
	d.reboot(true)
}

func (d *simDevice) firmwareUnreachable() (Fault, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.armedLocked(FaultFWUnreachable)
}

// failUnreachable is a connect attempt to a firmware server the device cannot
// reach: it takes secFWConnectFail, then Upgrade reports the failure. The
// device neither reboots nor goes silent (live, 2026-10-30: MQTT stayed up
// and the next Upgrade worked at once).
func (d *simDevice) failUnreachable(f Fault) {
	wait := f.Duration
	if wait <= 0 {
		wait = d.dur(secFWConnectFail)
	}
	if !d.sleepFor(wait) {
		return
	}
	d.mu.Lock()
	d.armed = nil
	d.setPhaseLocked(PhaseRun)
	d.mu.Unlock()
	d.mq.publish("stat/"+d.topic+"/UPGRADE", `{"Upgrade":"Failed HTTP error: connection failed"}`, false)
}

func (d *simDevice) downloadWithRetries() (*flashed, error) {
	attempts := attemptsESP8266
	if d.esp32 {
		attempts = attemptsESP32
	}
	override := ""
	var last error
	for i := 0; i < attempts; i++ {
		d.mu.Lock()
		d.attempts++
		url := d.runtime.otaURL
		minimal := d.minimalLocked()
		d.mu.Unlock()
		if override != "" {
			url = override
		}
		start := time.Now()
		res, err := d.download(url, minimal)
		if errors.Is(err, errTooLarge) {
			// Tasmota's automatic fallback: remember ota_loader and retry
			// with the minimal image of the same family.
			if mu, ok := minimalURL(url); ok {
				d.mu.Lock()
				d.otaLoader = true
				d.mu.Unlock()
				override = mu
				last = err
				continue
			}
		}
		if err == nil {
			if rest := d.dur(secDownloadMin) - time.Since(start); rest > 0 && !d.sleepFor(rest) {
				return nil, context.Canceled
			}
			return res, nil
		}
		last = err
		if !d.sleep(secRetryDelay) {
			return nil, context.Canceled
		}
	}
	return nil, last
}

// download fetches url like ESPhttpUpdate and checks what it got.
func (d *simDevice) download(url string, runningMinimal bool) (*flashed, error) {
	d.mu.Lock()
	_, fail := d.armedLocked(FaultDownloadFail)
	_, wrong := d.armedLocked(FaultWrongFamily)
	ver := d.version
	img := d.image
	d.mu.Unlock()
	if fail {
		return nil, devErr("File Not Found (404)")
	}
	u, err := neturl.Parse(url)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return nil, devErr("HTTP error: connection refused")
	}
	ctx, cancel := context.WithTimeout(d.ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, devErr("HTTP error: connection refused")
	}
	req.Header.Set("User-Agent", "ESP8266-http-Update")
	req.Header.Set("x-ESP8266-version", ver+"(release-"+img+")")
	resp, err := d.f.client.Do(req)
	if err != nil {
		return nil, devErr("HTTP error: connection refused")
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, devErr("File Not Found (404)")
	case http.StatusForbidden:
		return nil, devErr("Forbidden (403)")
	default:
		return nil, devErr("Wrong HTTP Code")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return nil, devErr("HTTP error: connection lost")
	}
	if len(body) > maxImage {
		return nil, devErr("Not Enough space")
	}
	raw, err := unpack(body)
	if err != nil {
		return nil, devErr("Verify Bin Header Failed")
	}
	fam, err := imageFamily(raw)
	if wrong || err != nil || d.chipFamily == "" || fam != d.chipFamily {
		return nil, devErr("Verify Bin Header Failed")
	}
	kind := imageKind(u.Path, d.esp32)
	if d.def.Chip == "esp8266" && d.def.FlashKB <= 1024 && !runningMinimal && kind != "minimal" {
		return nil, errTooLarge
	}
	version, ok := findVersion(raw)
	if !ok {
		version = d.f.x.FlashVersion
	}
	return &flashed{kind: kind, version: version}, nil
}

// restartSoon is Restart 1: respond, then reboot after ~2 s on the same image.
// The restart is itself a save event (see settings.go), and it commits when it
// happens, not when it was scheduled: a value set during the ~2 s wait (a
// Hostname change queues the restart, the next command comes right behind)
// is flashed with the rest, as on the device, instead of being lost.
func (d *simDevice) restartSoon() {
	if !d.sleep(secRestartCmd) {
		return
	}
	d.reboot(true)
}

// reboot takes the device through the offline gap and brings it back with
// reloaded settings. commit is false for PowerCycle (flash is not written).
func (d *simDevice) reboot(commit bool) {
	d.mu.Lock()
	if commit {
		d.commitLocked()
	}
	hang := d.hasFaultLocked(FaultRebootHang)
	srv := d.srv
	d.mu.Unlock()
	// The port closes before the phase says "rebooting": whoever sees that phase
	// must find the port closed, not a connection that is accepted and then cut.
	// Requests already in flight finish meanwhile, as on the device; stopHTTP
	// below cuts what is still open.
	if !hang && srv != nil {
		_ = srv.l.Close() // a second close in stopHTTP is harmless
	}
	d.mu.Lock()
	d.setPhaseLocked(PhaseRebooting)
	_, never := d.armedLocked(FaultNeverReturns)
	gap := d.dur(secRebootGap)
	if d.esp32 {
		gap = d.dur(secRebootGap - 1)
	}
	if d.spec.RebootGap > 0 {
		gap = d.spec.RebootGap
	}
	if f, ok := d.armedLocked(FaultLongReboot); ok {
		if f.Duration > 0 {
			gap += f.Duration
		} else {
			gap *= 10
		}
	}
	d.mu.Unlock()
	d.mq.dropUnclean() // EspRestart -> WifiShutdown -> MqttDisconnect: no DISCONNECT packet, the broker fires the will
	if !hang {
		d.stopHTTP()
	}
	if never {
		<-d.ctx.Done()
		return
	}
	if !d.sleepFor(gap) {
		return
	}
	d.mu.Lock()
	_, stay := d.armedLocked(FaultStayOldVersion)
	// A device stuck in safeboot keeps booting its factory partition whatever was flashed or switched.
	stuck := d.isSafebootLocked() && len(d.faultsForLocked("", FaultStuckInSafeboot)) > 0
	if d.nextImage != nil && !stay && !stuck {
		d.image, d.version = d.nextImage.kind, d.nextImage.version
	}
	// Observed on the fleet (HA history, 2026-10-19): a relay keeps its state
	// across the two-step update through the minimal image (the ESP8266 RTC
	// restore works there), an ESP32 relay without SetOption0 does not, and a
	// PWM light comes back ON for a few milliseconds and then switches itself
	// OFF (the light driver applies PowerOnState 0 after the boot).
	// SetOption0 1 saves the state to flash on every change and restores it
	// at every boot.
	lightLoses := false
	if d.settingFrom(d.persisted.kv, "setoption0") != "1" {
		switch {
		case d.spec.Light:
			lightLoses = true
		case d.esp32:
			clear(d.relays)
		}
	}
	d.nextImage, d.armed, d.restartQueued, d.uploadType = nil, nil, false, uploadNone
	d.runtime = d.persisted.clone() // RAM-only changes (OtaUrl set in minimal) are lost here
	d.boots++
	if !d.minimalLocked() {
		d.persisted.bootCount++ // the normal firmware saves its boot counter
		d.runtime = d.persisted.clone()
	}
	d.bootAt, d.uptimeBase, d.mqttCount = time.Now(), 0, 1
	d.reason = "Software/System restart"
	if d.esp32 {
		d.reason = "Software reset CPU"
	}
	if d.nextReason != "" {
		d.reason, d.nextReason = d.nextReason, ""
	}
	loader := d.minimalLocked() && d.otaLoader
	d.mu.Unlock()
	if !hang {
		d.startHTTP()
	}
	d.setPhase(PhaseRun)
	d.mq.connect()
	if lightLoses {
		d.spawn(func() {
			if d.sleep(secLightSettle) {
				d.mu.Lock()
				clear(d.relays)
				d.mu.Unlock()
			}
		})
	}
	if loader {
		d.spawn(func() {
			if d.sleep(secAutoLoader) {
				d.runOTA(true)
			}
		})
	}
}

// loopbackOnly is the dial guard of the device's download client: a
// simulated device never reaches beyond the loopback interface unless the
// fleet was told otherwise. The check happens before any packet is sent: IP
// literals are tested directly, names are resolved and only loopback
// results are dialled.
func loopbackOnly(allow []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var dialer net.Dialer
		if slices.Contains(allow, host) {
			return dialer.DialContext(ctx, network, addr)
		}
		var ips []netip.Addr
		if ip, perr := netip.ParseAddr(host); perr == nil {
			ips = []netip.Addr{ip}
		} else {
			found, lerr := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if lerr != nil {
				return nil, lerr
			}
			ips = found
		}
		for _, ip := range ips {
			if ip.IsLoopback() {
				return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
		}
		return nil, fmt.Errorf("simulator: refusing non-loopback download target %s", addr)
	}
}

// deviceError carries a firmware error string verbatim (they are capitalised
// in ESPhttpUpdate and published as-is in {"Upgrade":"Failed ..."}).
type deviceError string

func (e deviceError) Error() string { return string(e) }

func devErr(s string) error { return deviceError(s) }
