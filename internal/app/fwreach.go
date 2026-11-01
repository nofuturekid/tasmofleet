package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// FW reach sources, as audited in device.fwreach.
const (
	FWReachSourceFirmwareServer = "firmware_server" // a GET 200 /fw/... from the device's IP
	FWReachSourceOTADownload    = "ota_download"    // a download update succeeded
	FWReachSourceOTAFailure     = "ota_failure"     // the device could not download (WP-D2)
	FWReachSourceAdminForget    = "admin_forget"    // an administrator reset it to unknown
)

// fwReachStore is what the learner needs of the store.
type fwReachStore interface {
	GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error)
	ListDevices(ctx context.Context) ([]model.Device, error)
	SetFWReach(ctx context.Context, id model.DeviceID, r model.FWReach) error
}

// FWReachLearner records what a device reaches (D11): whether it can fetch from tasmofleet's
// firmware server. Every source writes through here so each change is audited the same way.
// Unknown means "try", so a wrong "yes" costs nothing and a wrong "no" expires after 24 h.
type FWReachLearner struct {
	st    fwReachStore
	audit audit.Recorder
	clk   clock.Clock
	log   *slog.Logger
	mu    sync.Mutex // one write at a time: a staged update's two GETs must not both audit a "yes"
}

func newFWReachLearner(st fwReachStore, rec audit.Recorder, clk clock.Clock, log *slog.Logger) *FWReachLearner {
	return &FWReachLearner{st: st, audit: rec, clk: clk, log: log}
}

// NoteYes records that the device reaches the firmware server. A device already known to reach
// it is not written again (every firmware download would otherwise write and audit).
func (l *FWReachLearner) NoteYes(ctx context.Context, id model.DeviceID, source string) error {
	return l.set(ctx, "system", id, model.FWReachYes, "", source)
}

// NoteFWReachNo records that the device cannot reach the firmware server (the UPGRADE connection
// failure, D9). The OTA machine calls it. detail is a short English note shown in the UI; it must
// carry no secrets.
func (l *FWReachLearner) NoteFWReachNo(ctx context.Context, id model.DeviceID, detail string) error {
	return l.set(ctx, "system", id, model.FWReachNo, detail, FWReachSourceOTAFailure)
}

// Forget resets the device to unknown, on behalf of actor.
func (l *FWReachLearner) Forget(ctx context.Context, actor string, id model.DeviceID) error {
	return l.set(ctx, actor, id, model.FWReachUnknown, "", FWReachSourceAdminForget)
}

// NoteYesFromIP is NoteYes for the device that owns ip. An address that belongs to no device or to
// several (NAT, a shared gateway, port forwards) learns nothing: the request cannot be attributed.
func (l *FWReachLearner) NoteYesFromIP(ctx context.Context, ip string, source string) error {
	addr, ok := parseIP(ip)
	if !ok {
		return nil // not an address: nothing to attribute
	}
	all, err := l.st.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("fwreach: %w", err)
	}
	var owner model.DeviceID
	n, consistent := 0, false
	for _, d := range all {
		if has, same := deviceIP(d, addr); has {
			owner, consistent = d.ID, same
			n++
		}
	}
	if n != 1 || !consistent {
		return nil
	}
	return l.NoteYes(ctx, owner, source)
}

func parseIP(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	return a.Unmap(), err == nil
}

// deviceIP reports whether ip is one of the device's comparable addresses (the stored one, or the
// one it reported itself; a hostname cannot be compared) and whether every comparable address
// equals ip. Two that disagree mean one is stale: the device counts as a match but is not trusted.
func deviceIP(d model.Device, ip netip.Addr) (has, same bool) {
	same = true
	for _, s := range []string{d.Address, d.Net.IP} {
		a, ok := parseIP(s)
		if !ok {
			continue
		}
		if a == ip {
			has = true
		} else {
			same = false
		}
	}
	return has, same
}

func (l *FWReachLearner) set(ctx context.Context, actor string, id model.DeviceID, state model.FWReachState, detail, source string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.st.GetDevice(ctx, id)
	if err != nil {
		return fmt.Errorf("fwreach: %w", err)
	}
	if state == model.FWReachYes && d.FWReach.State == model.FWReachYes {
		return nil
	}
	r := model.FWReach{}
	if state != model.FWReachUnknown {
		r = model.FWReach{State: state, At: l.clk.Now(), Detail: detail}
	}
	if err := l.st.SetFWReach(ctx, id, r); err != nil {
		return fmt.Errorf("fwreach: %w", err)
	}
	if l.audit != nil {
		shown := string(state)
		if state == model.FWReachUnknown {
			shown = "unknown"
		}
		b, _ := json.Marshal(map[string]string{"state": shown, "source": source}) // a map of strings cannot fail
		l.audit.Record(ctx, model.AuditEvent{At: l.clk.Now(), Actor: actor, Action: "device.fwreach",
			Target: "device:" + strconv.FormatInt(int64(id), 10), Detail: string(b), Result: "ok"})
	}
	return nil
}

// fwReachTimeout bounds a background write triggered by a device request.
const fwReachTimeout = 5 * time.Second
