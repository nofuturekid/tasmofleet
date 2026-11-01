package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Commander is the part of tasmota.Client the reads and the apply use. The client routes over HTTP or
// MQTT by itself, so an MQTT-only device is read through the strict Template matcher (E3).
type Commander interface {
	Command(ctx context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error)
}

// reading is one live read of a device: its Module and, on a user template, the stored Template.
type reading struct {
	Module   policy.ModuleReading
	Template *policy.GPIOTemplate // nil: not read (built-in module) or unreadable
	TplErr   string               // why Template is nil on Module 0
	At       time.Time
}

// readLive reads Module and, when the device runs its user template, Template. One command at a time
// (never Status 0). A Template reply is read on Module 0 only: on a built-in module the stored template
// is inactive (T1) and is not used.
func readLive(ctx context.Context, c Commander, d model.Device, now time.Time) (reading, error) {
	t := tasmota.TargetFor(d)
	raw, err := c.Command(ctx, t, "Module")
	if err != nil {
		return reading{}, fmt.Errorf("reading Module: %w", err)
	}
	m, err := policy.ParseModule(raw)
	if err != nil {
		return reading{}, err
	}
	r := reading{Module: m, At: now}
	if m.Number != 0 {
		return r, nil
	}
	raw, err = c.Command(ctx, t, "Template")
	if err != nil {
		return reading{}, fmt.Errorf("reading Template: %w", err)
	}
	tpl, err := policy.ParseTemplate(raw)
	if err != nil {
		r.TplErr = err.Error()
		return r, nil //nolint:nilerr // an unreadable Template is a state of the device (Module known, template not), not a failed read
	}
	r.Template = &tpl
	return r, nil
}

// cache holds the newest live reading per device. Readings older than maxAge count as not live.
type cache struct {
	mu     sync.Mutex
	m      map[model.DeviceID]reading
	maxAge time.Duration
}

func newCache(maxAge time.Duration) *cache {
	return &cache{m: map[model.DeviceID]reading{}, maxAge: maxAge}
}

func (c *cache) put(id model.DeviceID, r reading) {
	c.mu.Lock()
	c.m[id] = r
	c.mu.Unlock()
}

func (c *cache) get(id model.DeviceID, now time.Time) (reading, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.m[id]
	return r, ok && now.Sub(r.At) <= c.maxAge
}

// fromSnapshot reads Module and Template out of a backup snapshot (a JSON object with one raw reply per
// command; a command that failed holds {"_error": ...}). Nothing here is ever applied (E5).
func fromSnapshot(data []byte, at time.Time) (reading, bool) {
	var doc map[string]json.RawMessage
	if json.Unmarshal(data, &doc) != nil {
		return reading{}, false
	}
	rawModule, ok := doc["Module"]
	if !ok || bytes.Contains(rawModule, []byte(`"_error"`)) {
		return reading{}, false
	}
	m, err := policy.ParseModule(rawModule)
	if err != nil {
		return reading{}, false
	}
	r := reading{Module: m, At: at}
	if m.Number != 0 {
		return r, true
	}
	if rawTpl, ok := doc["Template"]; ok && !bytes.Contains(rawTpl, []byte(`"_error"`)) {
		if tpl, err := policy.ParseTemplate(rawTpl); err == nil {
			r.Template = &tpl
			return r, true
		}
	}
	r.TplErr = "the backup has no readable template"
	return r, true
}
