package web

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/store"
)

// registerReplace adds the "Replace device" wizard. It needs the restore
// service to implement restore.Replacer.
func (h *handler) registerReplace(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if _, ok := h.d.Restore.(restore.Replacer); !ok {
		return
	}
	route("GET /devices/{id}/replace", adm, h.replaceStart)
	route("GET /devices/{id}/replace/preview", adm, h.replacePreview)
	route("POST /devices/{id}/replace", adm, h.replaceRun)
}

func (h *handler) replacer() restore.Replacer {
	r, _ := h.d.Restore.(restore.Replacer)
	return r
}

// candidate is a device the network showed that is online and not in the
// inventory.
type candidate struct {
	Address string
	Port    int
	MAC     string
}

type replacePage struct {
	Device     model.Device
	Candidates []candidate
	Address    string
	Port       string
	Error      string
	// Preview step
	Preview *restore.ReplacePreview
	Backup  model.BackupID
}

// replaceCandidates lists the proposals discovery holds for devices that are
// not in the inventory, kept to those that answer right now.
func (h *handler) replaceCandidates(r *http.Request) []candidate {
	if h.d.Discovery == nil {
		return nil
	}
	props, err := h.d.Discovery.Proposals(r.Context())
	if err != nil {
		h.log.ErrorContext(r.Context(), "replace: proposals", "err", err)
		return nil
	}
	var eps []restore.Endpoint
	for _, p := range props {
		if p.Kind == discovery.KindNew && p.Device.Address != "" && len(eps) < 30 {
			eps = append(eps, restore.Endpoint{Address: p.Device.Address, Port: p.Device.Port})
		}
	}
	var out []candidate
	for _, ep := range h.replacer().Candidates(r.Context(), eps) {
		out = append(out, candidate{Address: ep.Address, Port: ep.Port, MAC: ep.MAC})
	}
	return out
}

func (h *handler) replaceStart(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	p := replacePage{Device: d, Candidates: h.replaceCandidates(r)}
	h.renderReplace(w, r, http.StatusOK, p)
}

// replaceReq reads the replacement address, port and chosen backup.
func replaceReq(d model.Device, get func(string) string) (restore.ReplaceRequest, replacePage, string) {
	p := replacePage{Device: d, Address: strings.TrimSpace(get("address")), Port: strings.TrimSpace(get("port"))}
	// A device chosen from the list ("target", host:port) unless an address was typed.
	if t := strings.TrimSpace(get("target")); t != "" && p.Address == "" {
		if host, port, err := net.SplitHostPort(t); err == nil {
			p.Address, p.Port = host, port
		} else {
			p.Address = t
		}
	}
	req := restore.ReplaceRequest{Device: d.ID, Address: p.Address}
	if p.Address == "" || len(p.Address) > 253 || strings.ContainsAny(p.Address, " /@#?") {
		return req, p, "Enter the address of the new device."
	}
	if p.Port != "" {
		n, err := strconv.Atoi(p.Port)
		if err != nil || n < 1 || n > 65535 {
			return req, p, "The port must be a number between 1 and 65535."
		}
		req.Port = n
	}
	if b := get("backup"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return req, p, "Choose one of the listed backups."
		}
		req.Backup, p.Backup = model.BackupID(n), model.BackupID(n)
	}
	return req, p, ""
}

func (h *handler) replacePreview(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	req, p, msg := replaceReq(d, q.Get)
	h.replaceShow(w, r, d, req, p, msg)
}

// replaceShow runs the live checks and renders the preview, or the start page
// with the reason.
func (h *handler) replaceShow(w http.ResponseWriter, r *http.Request, d model.Device, req restore.ReplaceRequest, p replacePage, msg string) {
	if msg == "" {
		pv, err := h.replacer().PreviewReplace(r.Context(), req)
		if err == nil {
			p.Preview = &pv
			h.renderReplace(w, r, http.StatusOK, p)
			return
		}
		status, text := replaceRefusal(err)
		if status == http.StatusInternalServerError {
			h.fail(w, r, err)
			return
		}
		p.Candidates, p.Error = h.replaceCandidates(r), text
		h.renderReplace(w, r, status, p)
		return
	}
	p.Candidates, p.Error = h.replaceCandidates(r), msg
	h.renderReplace(w, r, http.StatusUnprocessableEntity, p)
}

func (h *handler) replaceRun(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	if err := r.ParseForm(); err != nil {
		h.fail(w, r, err)
		return
	}
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	req, p, msg := replaceReq(d, r.PostFormValue)
	if msg != "" {
		h.replaceShow(w, r, d, req, p, msg)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		pv, err := h.replacer().PreviewReplace(r.Context(), req)
		if err != nil {
			h.replaceShow(w, r, d, req, p, replaceErrText(err))
			return
		}
		p.Preview, p.Error = &pv, needConfirm
		h.renderReplace(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	opt := restore.StartOptions{AllowMajorChange: r.PostFormValue("major_ok") == "1", AllowUnknownFamily: r.PostFormValue("family_ok") == "1", ConfirmedFiles: formCount(r.PostFormValue("files"))}
	job, err := h.replacer().StartReplace(r.Context(), actor(r), req, opt)
	var need *restore.OverrideError
	switch {
	case err == nil:
		redirect(w, r, "/restores/"+strconv.FormatInt(int64(job.ID), 10))
	case errors.As(err, &need):
		if pv, perr := h.replacer().PreviewReplace(r.Context(), req); perr == nil {
			p.Preview = &pv
		}
		p.Error = "Something changed since the page was shown: confirm the risk named below."
		h.renderReplace(w, r, http.StatusUnprocessableEntity, p)
	default:
		h.replaceShow(w, r, d, req, p, replaceErrText(err))
	}
}

// replaceErrText words a start refusal; an unexpected error is logged by the
// caller path and shown generically.
func replaceErrText(err error) string {
	_, text := replaceRefusal(err)
	if text == "" {
		return "The replacement could not be started."
	}
	return text
}

// oldAnswers is the refusal for an old device that still answers. The start page
// carries a standing warning with the same message and leaves it out while this
// one is shown, so the page never says it twice.
const oldAnswers = "The old device still answers at its address. Two identical devices on one network would clash (same hostname, MQTT topic and static IP). Switch it off or disconnect it, then try again."

// replaceRefusal maps a refusal to a status and a fixed sentence. Errors that
// are not refusals map to 500 with no text: device error text never reaches
// the page.
func replaceRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, restore.ErrReplaceOff):
		return http.StatusNotFound, "Replace is not available on this installation."
	case errors.Is(err, restore.ErrBusy), errors.Is(err, restore.ErrOTAActive):
		return http.StatusConflict, "Another operation on this device is running. Try again when it has finished."
	case errors.Is(err, restore.ErrOldReachable):
		return http.StatusUnprocessableEntity, oldAnswers
	case errors.Is(err, restore.ErrCandidateIsOld):
		return http.StatusUnprocessableEntity, "That is the device being replaced itself. To put a backup back onto it, use Restore."
	case errors.Is(err, restore.ErrCandidateBound):
		return http.StatusUnprocessableEntity, "That device (or its address) already belongs to an entry in the inventory. Only a device that is not in the inventory can take over."
	case errors.Is(err, restore.ErrCandidateOutside):
		return http.StatusUnprocessableEntity, "That address is outside the configured device networks."
	case errors.Is(err, restore.ErrCandidateOffline):
		return http.StatusUnprocessableEntity, "The new device did not answer. Check that it is powered, on the network and that the address is right."
	case errors.Is(err, restore.ErrCandidateLocked):
		return http.StatusUnprocessableEntity, "The new device asks for a password. Use a device with a factory configuration (no web password)."
	case errors.Is(err, restore.ErrCandidateUnusable):
		return http.StatusUnprocessableEntity, "The new device could not be identified (MAC, chip family or firmware). Check that it runs Tasmota."
	case errors.Is(err, restore.ErrNoBackup):
		return http.StatusUnprocessableEntity, "This device has no config dump to restore. Without a backup there is nothing to put on the new device."
	case errors.Is(err, restore.ErrNotDump):
		return http.StatusUnprocessableEntity, "Choose one of this device's own config dumps."
	case errors.Is(err, restore.ErrFamilyMismatch):
		return http.StatusUnprocessableEntity, "The new device has another chip family than the old one (or than the backup). A configuration cannot move between chip families."
	case errors.Is(err, restore.ErrVersionUnknown):
		return http.StatusUnprocessableEntity, "The firmware version of the new device or of the backup is unknown."
	case errors.Is(err, restore.ErrNewerBackup):
		return http.StatusUnprocessableEntity, "The backup comes from newer firmware than the new device runs. Update the new device first, or choose an older config dump of this device on its Backups page."
	case errors.Is(err, restore.ErrNoAddress):
		return http.StatusUnprocessableEntity, "The address does not resolve to exactly one IP address."
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "Not found."
	}
	return http.StatusInternalServerError, ""
}
