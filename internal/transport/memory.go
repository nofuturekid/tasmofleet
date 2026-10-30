package transport

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Memory remembers, in memory only, which devices' HTTP calls failed and why. It implements
// app.HTTPMemory: the facts read it to decide that HTTP is failing, which is what routes commands
// to MQTT. A failure counts for ttl (D4: 10 minutes) and is cleared by the next HTTP call that
// gets an answer, a health poll included (the observer sees it too).
type Memory struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[model.DeviceID]failure
}

type failure struct {
	kind  HTTPFailKind
	since time.Time
	until time.Time
}

// NewMemory returns a memory whose entries last ttl.
func NewMemory(ttl time.Duration, now func() time.Time) *Memory {
	return &Memory{ttl: ttl, now: now, m: map[model.DeviceID]failure{}}
}

// HTTPFailure reports why and since when the last HTTP calls to id failed.
func (m *Memory) HTTPFailure(id model.DeviceID) (HTTPFailKind, time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.m[id]
	if !ok {
		return "", time.Time{}, false
	}
	if !m.now().Before(f.until) {
		delete(m.m, id)
		return "", time.Time{}, false
	}
	return f.kind, f.since, true
}

// Fail records a failing call; "since" keeps the start of an unbroken series.
func (m *Memory) Fail(id model.DeviceID, kind HTTPFailKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	f, ok := m.m[id]
	if !ok || !now.Before(f.until) {
		f = failure{since: now}
	}
	f.kind, f.until = kind, now.Add(m.ttl)
	m.m[id] = f
}

// Clear forgets the failure: an HTTP call got an answer.
func (m *Memory) Clear(id model.DeviceID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, id)
}

// failKind maps an HTTP error to what it says about reachability: failing (with the kind), answered
// (HTTP works), or nothing (a timeout says too little either way).
func failKind(err error) (kind HTTPFailKind, failing, answered bool) {
	switch {
	case err == nil:
		return "", false, true
	case errors.Is(err, tasmota.ErrForbiddenAddr):
		return HTTPFailOutside, true, false
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return HTTPFailAPIRefused, true, false
	case errors.Is(err, tasmota.ErrAuth):
		return HTTPFailAuth, true, false
	case errors.Is(err, tasmota.ErrUnreachable):
		return HTTPFailUnreachable, true, false
	case errors.Is(err, tasmota.ErrTimeout), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", false, false
	}
	// A parsed answer of any kind (unknown command, bad reply, upload refused): the device is there.
	return "", false, true
}

// observer wraps the raw HTTP client and tells the memory what each call showed. It routes nothing.
type observer struct {
	c   tasmota.Client
	mem *Memory
}

func (o *observer) see(t tasmota.Target, err error) {
	id := t.DeviceID()
	if id == 0 {
		return // an address check says nothing about an inventory device
	}
	kind, failing, answered := failKind(err)
	switch {
	case failing:
		o.mem.Fail(id, kind)
	case answered:
		o.mem.Clear(id)
	}
}

func (o *observer) Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	raw, err := o.c.Command(ctx, t, cmnd)
	o.see(t, err)
	return raw, err
}

func (o *observer) DownloadConfig(ctx context.Context, t tasmota.Target) ([]byte, error) {
	b, err := o.c.DownloadConfig(ctx, t)
	o.see(t, err)
	return b, err
}

func (o *observer) RootPage(ctx context.Context, t tasmota.Target) (tasmota.RootInfo, error) {
	ri, err := o.c.RootPage(ctx, t)
	o.see(t, err)
	return ri, err
}
