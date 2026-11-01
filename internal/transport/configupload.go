package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ConfigUploader sends a settings dump to a device with FileUpload Type 2. It is MQTT only: the
// HTTP restore is the web UI's restore upload (tasmota.ConfigRestorer, Router.RestoreConfig).
// Router implements it.
type ConfigUploader interface {
	UploadConfig(ctx context.Context, t tasmota.Target, dump []byte, bind UploadBinding) error
}

// UploadBinding is the identity the restore checked before it asked for the upload: the MQTT
// address of the device and the MACs it reported. UploadConfig sends nothing unless the device is
// still bound to exactly this and answers Status 5 with one of these MACs on that topic, so a
// check and the upload cannot end up on different devices (a topic or broker change, or another
// device taking over the topic, between the two).
type UploadBinding struct {
	Name                         string // for the error text
	Topic, FullTopic, GroupTopic string
	MQTT                         model.MQTTCapability
	MACs                         []string // the stored MAC and MAC set; Status 5 must report one of them
}

// BindingOf takes the binding from a device snapshot.
func BindingOf(d model.Device) UploadBinding {
	return UploadBinding{Name: d.Name, Topic: d.Topic, FullTopic: d.FullTopic, GroupTopic: d.GroupTopic, MQTT: d.MQTT,
		MACs: append([]string{d.MAC}, d.MACs...)}
}

// ErrIdentityChanged: the device behind the topic is not the one the restore checked (nothing was sent).
var ErrIdentityChanged = errors.New("transport: the device behind this topic changed")

// MQTTUploader is what the command channel offers for FileUpload (mqttcmd.Channel.Upload). The
// channel is asserted to it, so a fake MQTTDoer without it simply has no restore over MQTT.
type MQTTUploader interface {
	Upload(ctx context.Context, d model.Device, typ int, name string, size int64, r io.Reader, password string, progress func(sent int64)) error
}

var _ ConfigUploader = (*Router)(nil)

// UploadConfig restores dump (a Tasmota .dmp) over MQTT. It never uses HTTP and never fails over:
// the caller (the restore service) chose this path from the same facts, and a device that got a
// FileUpload start is never sent a second one. The check is repeated here with the strict write
// conditions (MQTTBlocker), because the upload replaces the device's whole configuration.
func (r *Router) UploadConfig(ctx context.Context, t tasmota.Target, dump []byte, bind UploadBinding) error {
	id := t.DeviceID()
	if id == 0 {
		return fmt.Errorf("%w: a restore over MQTT needs an inventory device", tasmota.ErrNotImplemented)
	}
	m := r.mqttDoer()
	up, ok := m.(MQTTUploader)
	if m == nil || !ok {
		return fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	d, err := r.o.Devices.GetDevice(ctx, id)
	if err != nil {
		return err
	}
	if d.Topic != bind.Topic || d.FullTopic != bind.FullTopic || d.GroupTopic != bind.GroupTopic || d.MQTT != bind.MQTT || !macAmong(d.MAC, bind.MACs) {
		noteTrace(ctx, None)
		return fmt.Errorf("%w: the MQTT settings or the MAC of %s changed since it was checked; nothing was sent", ErrIdentityChanged, bind.Name)
	}
	f, err := r.o.Facts.FactsFor(ctx, d)
	if err != nil {
		return err
	}
	if why := transferBlocker(f, "restore"); why != "" {
		noteTrace(ctx, None)
		return fmt.Errorf("%w: a restore over MQTT is not possible: %s", tasmota.ErrUnreachable, why)
	}
	pw, why := r.downloadPassword(d)
	if why != "" {
		noteTrace(ctx, None)
		return fmt.Errorf("%w: a restore over MQTT is not possible: %s", tasmota.ErrUnreachable, why)
	}
	// Right before the start: the device that answers on this topic is the one that was checked.
	replies, err := r.statusFive(ctx, m, d)
	if err != nil {
		return err
	}
	// Exactly one device answers, and it is the one that was checked. A second reply means another
	// device (outside the inventory) uses the same topic.
	same := len(replies) == 1
	for _, raw := range replies {
		var s5 struct{ StatusNET struct{ Mac string } }
		if json.Unmarshal(raw, &s5) != nil || !macAmong(s5.StatusNET.Mac, bind.MACs) {
			same = false
		}
	}
	if !same {
		noteTrace(ctx, MQTT)
		return fmt.Errorf("%w: the device on this topic is not %s any more (or a second device answers on it); nothing was uploaded", ErrIdentityChanged, bind.Name)
	}
	err = up.Upload(ctx, d, mqttcmd.UploadSettings, "Config.dmp", int64(len(dump)), bytes.NewReader(dump), pw, nil)
	if err == nil || !notPublished(err) {
		noteTrace(ctx, MQTT)
	}
	if errors.Is(err, mqttcmd.ErrPasswordRefused) && r.o.Marker != nil {
		// The caller may be cancelled by now; the mark must still be written.
		if merr := r.o.Marker.MarkMQTTPasswordBad(context.WithoutCancel(ctx), d.ID, time.Now()); merr != nil {
			r.o.Log.Warn("transport: could not record the refused MQTT password", "device", int64(d.ID), "err", merr)
		}
	}
	return err
}

func normMAC(s string) string {
	return strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.ToLower(strings.TrimSpace(s)))
}

// macAmong reports whether mac is one of set (separators and case do not matter); an empty mac never matches.
func macAmong(mac string, set []string) bool {
	n := normMAC(mac)
	if n == "" {
		return false
	}
	for _, m := range set {
		if normMAC(m) == n {
			return true
		}
	}
	return false
}

// identityWindow is how long UploadConfig keeps listening after the first Status 5 reply.
var identityWindow = 1500 * time.Millisecond

// MQTTMultiReader is what the command channel offers to hear every reply to a command within a
// window (mqttcmd.Channel.DoAll).
type MQTTMultiReader interface {
	DoAll(ctx context.Context, d model.Device, cmd string, window time.Duration) ([]json.RawMessage, error)
}

// statusFive asks Status 5 and returns every reply that came within identityWindow of the first.
func (r *Router) statusFive(ctx context.Context, m MQTTDoer, d model.Device) ([]json.RawMessage, error) {
	if mr, ok := m.(MQTTMultiReader); ok {
		return mr.DoAll(ctx, d, "Status 5", identityWindow)
	}
	raw, err := m.Do(ctx, d, "Status 5")
	if err != nil {
		return nil, err
	}
	return []json.RawMessage{raw}, nil
}
