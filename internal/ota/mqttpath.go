package ota

import (
	"context"
	"fmt"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// UpdatePaths chooses how an update reaches a device (transport.Choose over the device's facts). The
// engine asks it once per device in the preview, which is also the preflight of Submit: the answer is
// the job's Transport for its whole life.
type UpdatePaths interface {
	UpdatePath(ctx context.Context, d model.Device, q PathQuery) (transport.Paths, error)
}

// PathQuery is the request part of the facts: what the plan decided for a device HTTP reaches.
type PathQuery struct {
	Delivery  model.Delivery // the plan's delivery (a push that fell back is already pull)
	ImageSize int64          // the image the update sends first
	// HTTPPush is today's HTTP decision: PushHTTP, PushSafeboot or DownloadHTTP (pull, or the fallback
	// of a push, with HTTPPushNote saying why).
	HTTPPush     transport.Path
	HTTPPushNote string
	// AllowMQTTUpload: the transaction allows the experimental upload over MQTT. The engine passes it only
	// with the delivery the person asked for (push), and never when the upload is closed.
	AllowMQTTUpload bool
	// Identity is what the preview's probe just read; the upload fit is judged on it, not on the stored one.
	Identity model.Identity
	// Layout is what is known of the ESP32 upload layout for the fit rule.
	Layout fwselect.UpLayoutHint
}

// pinned marks every device call of a job with the path the job chose (D5): a job that started over
// MQTT is never rerouted to HTTP and the other way round. Policy and command jobs choose nothing.
func pinned(ctx context.Context, j model.Job) context.Context {
	if !j.IsUpdate() {
		return ctx
	}
	if j.Transport == model.TransportMQTT {
		return transport.WithPath(ctx, transport.MQTT)
	}
	return transport.WithPath(ctx, transport.HTTP)
}

// httpPushDecision is the HTTP side of the path choice for a planned update.
func httpPushDecision(p planResult, requested model.Delivery) (transport.Path, string) {
	switch {
	case p.delivery == model.DeliveryPush && p.viaSafeboot:
		return transport.PushSafeboot, ""
	case p.delivery == model.DeliveryPush:
		return transport.PushHTTP, ""
	case requested == model.DeliveryPush:
		return transport.DownloadHTTP, p.fallback
	}
	return transport.DownloadHTTP, ""
}

// choosePath asks the path chooser about a planned device and folds the answer into the preview
// item: the path and its sentence, the job's transport, and a refusal when there is no path. Without
// a chooser every update is an HTTP one, exactly as before MQTT transport.
func (e *Engine) choosePath(ctx context.Context, d model.Device, r Request, p planResult, it *PreviewItem) {
	if e.paths == nil {
		return
	}
	hp, note := httpPushDecision(p, r.Delivery)
	q := PathQuery{Delivery: p.delivery, ImageSize: p.target.Size, HTTPPush: hp, HTTPPushNote: note, Layout: layoutOf(d), Identity: p.id}
	if r.AllowMQTTUpload && r.Delivery == model.DeliveryPush && e.mqttUpload {
		// The upload over HTTP may have fallen back to the download, but over MQTT the fit rule of D19 decides.
		q.AllowMQTTUpload, q.Delivery = true, r.Delivery
	}
	paths, err := e.paths.UpdatePath(ctx, d, q)
	if err != nil {
		it.Decision, it.Reason = decisionRefuse, "path: "+err.Error()
		return
	}
	it.Path, it.PathText = paths.Update, paths.Reason(transport.ActUpdate)
	switch paths.Update {
	case transport.None:
		it.Decision, it.Reason = decisionRefuse, it.PathText
	case transport.DownloadMQTT, transport.PushMQTT:
		it.Transport = model.TransportMQTT
		if !r.SkipBackup && paths.Backup != transport.MQTT {
			// The update backs the device up first; a job whose backup has no path would fail only after
			// the preflight, so say it now, with the chooser's reason.
			it.Decision, it.Reason = decisionRefuse, paths.Reason(transport.ActBackup)+"; or turn off \"Back up each device\" for this device"
			return
		}
		if paths.Update == transport.PushMQTT && (!r.AllowMQTTUpload || r.Delivery != model.DeliveryPush || !e.mqttUpload) {
			// The chooser must not offer it unasked; if it ever does, the engine does not take it.
			it.Decision, it.Reason = decisionRefuse, "upload over MQTT was not allowed for this update"
			return
		}
		if paths.Update == transport.PushMQTT {
			// The image goes through the broker (experimental): the engine's own checks on the file and the
			// plan come on top of the fit rule Choose applied.
			if err := e.mqttPushRefusal(d.ID, p.id, layoutOf(d), p.plan, p.target); err != nil {
				it.Decision, it.Reason = decisionRefuse, "upload over MQTT: "+err.Error()
				return
			}
			it.Delivery, it.ViaSafeboot, it.DeliveryNote = model.DeliveryPush, false, ""
		} else if it.Delivery == model.DeliveryPush {
			// The upload through the device's web page (or safeboot) needs HTTP; over MQTT the device
			// downloads. Never switched unseen: the row says so and the job stores pull.
			it.Delivery, it.ViaSafeboot = model.DeliveryPull, false
			it.DeliveryNote = joinSentences(it.DeliveryNote, "no upload over HTTP: the device is reached over MQTT, it downloads the file")
		}
	case transport.HTTP, transport.MQTT, transport.DownloadHTTP, transport.PushHTTP, transport.PushSafeboot:
	}
}

func joinSentences(a, b string) string {
	if a == "" {
		return b
	}
	return strings.TrimRight(a, ". ") + "; " + b
}

// declaredLanguage normalises a language the person chose for a device whose language build cannot be
// read (D10): "en" or "EN" is the English build (""), any other two letters are the upstream suffix.
func declaredLanguage(s string) (string, error) {
	l := strings.ToUpper(strings.TrimSpace(s))
	switch {
	case l == fwselect.LanguageEnglish:
		return "", nil
	case len(l) == 2 && l[0] >= 'A' && l[0] <= 'Z' && l[1] >= 'A' && l[1] <= 'Z':
		return l, nil
	}
	return "", fmt.Errorf("unknown language %q", s)
}
