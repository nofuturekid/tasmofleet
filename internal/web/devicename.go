package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// DeviceNames records the admin's "Keep" against a device's DeviceName (store.SQLite). Optional:
// without it the notice has no Keep button.
type DeviceNames interface {
	// DismissDeviceName remembers that deviceName was declined; store.ErrConflict when the device
	// reports another name now, store.ErrNotFound for an unknown device.
	DismissDeviceName(ctx context.Context, id model.DeviceID, deviceName string) error
}

// tasmotaDefaultName is the DeviceName of a Tasmota nobody named; offering it would nag on every
// such device (the prober treats it the same way).
const tasmotaDefaultName = "Tasmota"

// nameOffer is the DeviceName the Identity card offers instead of the stored name (#125), "" when
// there is nothing to decide: the device reports none, the names agree, it is Tasmota's default,
// or the admin already answered Keep to exactly this name. A stored name that is still the
// device's own follows it on refresh and never gets here.
func nameOffer(d model.Device) string {
	n := d.DeviceName
	if n == "" || n == d.Name || n == tasmotaDefaultName || n == d.KeptDeviceName {
		return ""
	}
	return n
}

// nameVals is the hx-vals of an answer: the DeviceName the admin saw, so a newer one is never
// answered unseen.
func nameVals(name string) string {
	b, _ := json.Marshal(map[string]string{"name": name}) // a map of strings cannot fail
	return string(b)
}

const nameStale = "The device reports another name now. Check it below."

// deviceNameAdopt is POST /devices/{id}/name/adopt: "Use device name". The stored name becomes the
// DeviceName the page showed, through the inventory (validation, audit, events like any rename).
func (h *handler) deviceNameAdopt(w http.ResponseWriter, r *http.Request) {
	h.nameAnswer(w, r, func(d model.Device, seen string) string {
		if d.DeviceName != seen || nameOffer(d) == "" {
			return nameStale
		}
		cas, ok := h.d.Inventory.(interface {
			UpdateIf(ctx context.Context, actor string, expected, next model.Device) error
		})
		if !ok {
			h.log.ErrorContext(r.Context(), "adopt device name: the inventory has no compare-and-set update")
			return "The name could not be changed. The server log has the details."
		}
		next := d
		next.Name = seen
		// Written only if the device is still the one the page showed: an edit made meanwhile is not lost.
		switch err := cas.UpdateIf(r.Context(), actor(r), d, next); {
		case errors.Is(err, inventory.ErrConflict):
			return "The device changed meanwhile. Check it below."
		case err != nil:
			h.log.WarnContext(r.Context(), "adopt device name", "device", d.ID, "err", err)
			return "The name could not be changed. The server log has the details."
		}
		return ""
	})
}

// deviceNameKeep is POST /devices/{id}/name/keep: "Keep". The notice stays away until the device
// reports another DeviceName.
func (h *handler) deviceNameKeep(w http.ResponseWriter, r *http.Request) {
	h.nameAnswer(w, r, func(d model.Device, seen string) string {
		switch err := h.d.DeviceNames.DismissDeviceName(r.Context(), d.ID, seen); {
		case errors.Is(err, store.ErrConflict):
			return nameStale
		case err != nil:
			h.log.WarnContext(r.Context(), "keep device name", "device", d.ID, "err", err)
			return "The answer could not be saved. The server log has the details."
		}
		h.recordFirmware(r, "device.name_kept", "device:"+strconv.FormatInt(int64(d.ID), 10),
			map[string]any{"device_name": seen, "name": d.Name}, "ok")
		return ""
	})
}

// nameAnswer runs one answer and swaps the Identity card (and, when the name changed, the header
// with it, as after "Refresh identity").
func (h *handler) nameAnswer(w http.ResponseWriter, r *http.Request, answer func(d model.Device, seen string) string) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	failure := answer(d, r.PostFormValue("name"))
	if fresh, err := h.d.Inventory.Get(r.Context(), d.ID); err == nil {
		d = fresh
	} else {
		h.log.WarnContext(r.Context(), "name answer: reload device", "err", err)
	}
	v := h.view(r, "", "", nil)
	hl, running := h.deviceState(r, d)
	tabs := h.headOf(r.Context(), v, d, hl, running, "overview")
	c := h.identity(v, d, running != nil, tabs.offline(), h.factsOf(r.Context(), d))
	c.Error = failure
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	parts := []templ.Component{identityCard(c)}
	if failure == "" {
		parts = append(parts, deviceHeadSwap(tabs, deviceCrumbs(d)))
	}
	if err := templ.Join(parts...).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "name answer: render", "err", err)
	}
}
