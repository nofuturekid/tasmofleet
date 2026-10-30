package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// MQTTFirmwareUploader is the firmware upload through the broker (FileUpload Type 1, experimental).
// The Router implements it; the OTA engine reaches it through its DeviceAPI.
type MQTTFirmwareUploader interface {
	// UploadFirmwareMQTT sends size bytes of body as the image name. progress, if set, receives the
	// bytes acknowledged so far. Errors are mqttcmd's: mqttcmd.ErrOutcomeUnknown means every byte was
	// sent and nothing confirmed it, anything else means the device keeps its firmware.
	UploadFirmwareMQTT(ctx context.Context, t tasmota.Target, name string, size int64, body io.Reader, progress func(sent int64)) error
	// MarkUploadPending tells the channel that a firmware upload to the device ended without a
	// confirmation in an earlier process: no file transfer to it until it has booted again (commands still go out).
	MarkUploadPending(ctx context.Context, t tasmota.Target) error
}

var _ MQTTFirmwareUploader = (*Router)(nil)

// mqttUploadChannel is what the command channel offers for FileUpload (mqttcmd.Channel.Upload). The
// channel is asserted to it, so a fake MQTTDoer without it has no upload.
type mqttUploadChannel interface {
	Upload(ctx context.Context, d model.Device, typ int, name string, size int64, r io.Reader, password string, progress func(sent int64)) error
}

type mqttPendingMarker interface{ MarkFirmwarePending(topic string) }

// MarkUploadPending re-arms the channel's block after a restart of this process (see the interface).
func (r *Router) MarkUploadPending(ctx context.Context, t tasmota.Target) error {
	// Not mqttDoer(): the block must be settable while the broker connection is still coming up.
	var doer MQTTDoer
	if h := r.mqtt.Load(); h != nil {
		doer = h.d
	}
	m, ok := doer.(mqttPendingMarker)
	if !ok {
		return errors.New("the MQTT channel cannot block transfers")
	}
	d, err := r.o.Devices.GetDevice(ctx, t.DeviceID())
	if err != nil {
		return err
	}
	if d.Topic == "" {
		return errors.New("the device has no MQTT topic")
	}
	m.MarkFirmwarePending(d.Topic)
	return nil
}

// UploadFirmwareMQTT uploads a firmware image over MQTT for a job that chose MQTT (a pinned path):
// never rerouted, never a fallback, never an HTTP request. The conditions are those of a write
// (strict topic rules) plus a usable MqttPassword; when one fails, nothing is published.
func (r *Router) UploadFirmwareMQTT(ctx context.Context, t tasmota.Target, name string, size int64, body io.Reader, progress func(int64)) error {
	id := t.DeviceID()
	if pin, ok := PathFrom(ctx); id == 0 || !ok || pin != MQTT {
		return errors.New("an upload over MQTT is only made for a job that chose MQTT")
	}
	if r.mqttDoer() == nil {
		return fmt.Errorf("%w: MQTT is not connected", tasmota.ErrUnreachable)
	}
	up, ok := r.mqttDoer().(mqttUploadChannel)
	if !ok {
		return errors.New("the MQTT channel cannot upload files")
	}
	d, f, _, err := r.decide(ctx, id)
	if err != nil {
		return err
	}
	if why := transferBlocker(f, "upload firmware"); why != "" {
		return fmt.Errorf("%w: not usable for an upload over MQTT: %s", tasmota.ErrUnreachable, why)
	}
	pw, why := r.downloadPassword(d)
	if why != "" {
		return fmt.Errorf("%w: %s", tasmota.ErrUnreachable, why)
	}
	noteTrace(ctx, MQTT)
	err = up.Upload(ctx, d, mqttcmd.UploadFirmware, name, size, body, pw, progress)
	if errors.Is(err, mqttcmd.ErrPasswordRefused) && r.o.Marker != nil {
		if merr := r.o.Marker.MarkMQTTPasswordBad(context.WithoutCancel(ctx), id, time.Now()); merr != nil {
			r.o.Log.Warn("transport: could not record the refused MQTT password", "device", int64(id), "err", merr)
		}
	}
	return err
}
