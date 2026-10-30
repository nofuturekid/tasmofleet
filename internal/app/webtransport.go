package app

import (
	"context"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// webFacts is what the web UI reads: the router's own transport facts (so the device page shows what
// the router decides), plus the batch read of the device list.
type webFacts struct{ *TransportFacts }

// FailingHTTP returns the facts of those of ids whose HTTP calls fail. The decision is made from the
// router's failure memory and the health, both in memory, so a fleet without a failure costs no
// database read at all; with failures the inventory is listed once for all of them.
func (w webFacts) FailingHTTP(ctx context.Context, ids []model.DeviceID) (map[model.DeviceID]transport.Facts, error) {
	var failing []model.DeviceID
	for _, id := range ids {
		if w.HTTPState(id).State == transport.HTTPFailing {
			failing = append(failing, id)
		}
	}
	if len(failing) == 0 {
		return nil, nil
	}
	all, err := w.Devices.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[model.DeviceID]model.Device, len(all))
	for _, d := range all {
		byID[d.ID] = d
	}
	out := make(map[model.DeviceID]transport.Facts, len(failing))
	for _, id := range failing {
		d, ok := byID[id]
		if !ok {
			continue
		}
		f, err := w.factsFor(ctx, d, all)
		if err != nil {
			return nil, err
		}
		out[id] = f
	}
	return out, nil
}
