package app

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// countingDevices counts the reads that cost a database round trip.
type countingDevices struct {
	fakeFactsDevices
	gets, lists int
}

func (c *countingDevices) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	c.gets++
	return c.fakeFactsDevices.GetDevice(ctx, id)
}

func (c *countingDevices) ListDevices(ctx context.Context) ([]model.Device, error) {
	c.lists++
	return c.fakeFactsDevices.ListDevices(ctx)
}

type memoryFor map[model.DeviceID]bool

func (m memoryFor) HTTPFailure(id model.DeviceID) (transport.HTTPFailKind, time.Time, bool) {
	return transport.HTTPFailUnreachable, factsNow.Add(-time.Hour), m[id]
}

// Why (review H2): the device list asks for the MQTT marker on every render; with 500 devices a
// Facts read each (GetDevice plus a full ListDevices) cost seconds. Facts are read only for a device
// whose HTTP is failing, and a fleet without failures (or without the router's memory) reads nothing.
func TestFailingHTTPReadsOnlyFailingDevices(t *testing.T) {
	var devs []model.Device
	for i := 1; i <= 200; i++ {
		devs = append(devs, model.Device{ID: model.DeviceID(i), Topic: "t" + strconv.Itoa(i), MQTT: model.MQTTOurs})
	}
	ids := make([]model.DeviceID, len(devs))
	for i, d := range devs {
		ids[i] = d.ID
	}
	h := fakeFactsHealth{}

	count := &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f := newFacts(devs, h, nil)
	f.Devices = count
	if m, err := (webFacts{f}).FailingHTTP(t.Context(), ids); err != nil || len(m) != 0 || count.gets+count.lists != 0 {
		t.Fatalf("no memory: %d facts, %d gets, %d lists, err %v", len(m), count.gets, count.lists, err)
	}

	count = &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f = newFacts(devs, h, memoryFor{}) // memory, no failure
	f.Devices = count
	if m, _ := (webFacts{f}).FailingHTTP(t.Context(), ids); len(m) != 0 || count.gets+count.lists != 0 {
		t.Fatalf("no failure: %d facts, %d gets, %d lists", len(m), count.gets, count.lists)
	}

	count = &countingDevices{fakeFactsDevices: fakeFactsDevices{devs}}
	f = newFacts(devs, h, memoryFor{7: true})
	f.Devices = count
	m, err := (webFacts{f}).FailingHTTP(t.Context(), ids)
	if err != nil || len(m) != 1 || m[7].HTTP.State != transport.HTTPFailing {
		t.Fatalf("one failure: %v, err %v", m, err)
	}
	if count.gets != 0 || count.lists != 1 {
		t.Fatalf("failing devices must cost one ListDevices in all, got %d gets and %d lists", count.gets, count.lists)
	}
}
