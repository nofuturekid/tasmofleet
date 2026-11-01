package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// deviceForm is the device edit form (the Add manually tab is addManualView).
type deviceForm struct {
	Device      model.Device
	HasPassword bool
	TagsText    string
	Error       string
	ErrField    string // input the Error belongs to; empty: a page-level error
	// Unverified is set when the new address could not prove the device's MAC
	// and the stored password would be sent there: the stronger confirmation.
	Unverified *unverifiedMove
	// Stored is the device's name as saved: the edit page's subtitle and trail while the form
	// carries the name being typed.
	Stored string
	// MQTTMode is the MQTT password mode shown selected: the stored one, or the one just posted.
	MQTTMode model.MQTTPasswordMode
	// MQTTSection says whether the form has the MQTT password section (admin with the store wired).
	MQTTSection bool
	// HasMQTTPassword: an own MQTT password is stored (its value is never rendered).
	HasMQTTPassword bool
	MQTTRefused     bool // the device answered Error 1 to the stored password
	// MQTTRetype: an own password was typed but the page is shown again, so it must be typed again.
	MQTTRetype bool
}

// unverifiedMove is the stronger confirmation for sending a stored password to
// an address that did not prove the device's MAC. The token is held on the
// server and tied to this target, like the console confirmation.
type unverifiedMove struct {
	Target, Token string
	ProposalID    int64
	Version       string
	Name          string
}

func (h *handler) renderForm(w http.ResponseWriter, r *http.Request, status int, f deviceForm) {
	f.HasMQTTPassword = f.Device.MQTTPasswordMode == model.MQTTPasswordOwn && f.Device.MQTTPassword != ""
	f.MQTTRefused = !f.Device.MQTTPasswordBadAt.IsZero()
	f.MQTTSection = h.d.MQTTPasswords != nil
	f.Device.HTTPPassword, f.Device.MQTTPassword = "", "" // never rendered
	f.TagsText = strings.Join(f.Device.Tags, ", ")
	v := h.view(r, "Edit "+f.Stored, "devices", nil)
	v.Crumbs = []ui.Crumb{{Label: "Devices", Path: "/devices"}, {Label: f.Stored, Path: devPath(f.Device.ID, "")}, {Label: "Edit"}}
	h.show(w, r, status, "device_form", v, editPage(f, v.Base, v.CSRF))
}

var (
	errBadPort   = errors.New("web: bad port")
	errAddrSpace = errors.New("web: address with spaces")
)

// formProblem says which input of the device form a rejected save is about and
// what to tell the person. Errors that are about no single input come back
// with an empty field (they stay in the page banner).
func formProblem(err error) (field, msg string) {
	switch {
	case errors.Is(err, errBadPort):
		return "port", "Use a port number between 1 and 65535, or leave it empty for 80."
	case errors.Is(err, errAddrSpace):
		return "address", "Enter an IP address or hostname without spaces, e.g. 192.168.1.80"
	case errors.Is(err, inventory.ErrOutsideNetwork):
		return "address", "That address is outside the configured device networks."
	case errors.Is(err, inventory.ErrInvalid):
		detail := strings.TrimPrefix(err.Error(), inventory.ErrInvalid.Error()+": ")
		switch {
		case detail == "address is empty":
			return "address", "Enter the IP address or DNS name of the device."
		case detail == "address too long":
			return "address", "That address is too long."
		case strings.HasPrefix(detail, "address "):
			return "address", "Use an IP address or DNS name, without http://, a port or a path."
		case strings.HasPrefix(detail, "name "):
			return "name", "Use a name of 1 to 100 printable characters."
		case strings.HasPrefix(detail, "port "):
			return "port", "Use a port number between 1 and 65535, or leave it empty for 80."
		case strings.HasPrefix(detail, "notes "):
			return "notes", "The notes are too long."
		case strings.HasPrefix(detail, "tag "), strings.Contains(detail, " tags"):
			return "tags", "Use at most 32 tags of up to 50 characters each, without control characters."
		}
	}
	return "", errText(err)
}

// probeProblem says which input a failed probe of a new device is about, what to
// tell the person, and whether "add without probing" can work: it cannot for an
// address outside the device networks (the inventory refuses it too). The Go
// error chain goes to the log, never to the browser.
func (h *handler) probeProblem(r *http.Request, err error) (field, msg string, fallback bool) {
	switch {
	case errors.Is(err, tasmota.ErrForbiddenAddr), errors.Is(err, inventory.ErrOutsideNetwork):
		return "address", "That address is outside the configured device networks.", false
	case errors.Is(err, tasmota.ErrAuth):
		return "http_password", "The device asked for a web password. Enter the web user and password of the device.", true
	case errors.Is(err, tasmota.ErrHTTPAPIRefused):
		return "address", "The device answers, but refuses HTTP API commands from this address. Set SetOption128 1 on the device, or use its own IP address or hostname.", true
	case errors.Is(err, tasmota.ErrUnreachable):
		return "address", "No device answered at this address. Check that it is on and on the network, or add it without probing.", true
	case errors.Is(err, tasmota.ErrTimeout):
		return "address", "The device took too long to answer. Try again, or add it without probing.", true
	case errors.Is(err, tasmota.ErrBadResponse):
		return "address", "Something answered, but it does not look like a Tasmota device.", true
	}
	h.log.WarnContext(r.Context(), "probe of a new device failed", "err", err)
	return "", "The device could not be probed. The server log has the details.", true
}

// applyForm copies the editable fields of the posted form onto d. The stored
// password is kept unless a new one is typed or "remove" is ticked.
func applyForm(r *http.Request, d model.Device) (model.Device, error) {
	d.Name = strings.TrimSpace(r.PostFormValue("name"))
	d.Address = strings.TrimSpace(r.PostFormValue("address"))
	d.Port = 0
	if p := strings.TrimSpace(r.PostFormValue("port")); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 65535 {
			return d, errBadPort
		}
		d.Port = n
	}
	d.HTTPUser = strings.TrimSpace(r.PostFormValue("http_user"))
	switch pw := r.PostFormValue("http_password"); {
	case pw != "":
		d.HTTPPassword = pw
	case r.PostFormValue("clear_password") == "1":
		d.HTTPPassword = ""
	}
	d.Tags = nil
	for t := range strings.SplitSeq(r.PostFormValue("tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			d.Tags = append(d.Tags, t)
		}
	}
	d.Notes = strings.TrimSpace(r.PostFormValue("notes"))
	d.OTAExcluded = r.PostFormValue("ota_excluded") == "1"
	return d, nil
}

// deviceCreate adds a device from the Add manually tab. With test=1 ("Test connection") it only
// probes the address and shows what answered; nothing is stored.
func (h *handler) deviceCreate(w http.ResponseWriter, r *http.Request) {
	limitBody(w, r)
	d, err := applyForm(r, model.Device{})
	if err == nil && strings.ContainsAny(d.Address, " \t") {
		err = errAddrSpace // caught before the probe, which would only say that nothing answered
	}
	// The fleet default password (N20) only goes to an address the admin typed: a form filled
	// from a discovered device does not use it (security review 2026-10-22).
	if pw := h.defaultPasswordFor(r); pw != "" && d.HTTPPassword == "" && r.PostFormValue("prefilled") == "" {
		d.HTTPPassword = pw // a typed password wins; the user stays empty (Tasmota's admin)
	}
	form := addManualView{Device: d}
	if r.PostFormValue("prefilled") != "" {
		form.Prefill = &addPrefill{Source: r.PostFormValue("prefilled"), Address: netaddr.Display(d.Address, d.Port, "http")}
	}
	if err != nil {
		form.ErrField, form.Error = formProblem(err)
		h.renderAdd(w, r, http.StatusUnprocessableEntity, form, nil)
		return
	}
	if r.PostFormValue("test") == "1" || r.PostFormValue("skip_probe") != "1" {
		probed, perr := h.d.Prober.Describe(r.Context(), d)
		if perr != nil {
			form.ErrField, form.Error, form.Fallback = h.probeProblem(r, perr)
			h.renderAdd(w, r, http.StatusUnprocessableEntity, form, nil)
			return
		}
		if r.PostFormValue("test") == "1" {
			probed.HTTPPassword = ""
			form.Tested = &probed
			h.renderAdd(w, r, http.StatusOK, form, nil)
			return
		}
		d = probed
	}
	id, err := h.d.Inventory.Create(r.Context(), actor(r), d)
	if err != nil {
		form.ErrField, form.Error = formProblem(err)
		h.renderAdd(w, r, http.StatusUnprocessableEntity, form, nil)
		return
	}
	redirect(w, r, "/devices/"+strconv.FormatInt(int64(id), 10)+"?ok=added")
}

func (h *handler) deviceEditForm(w http.ResponseWriter, r *http.Request) {
	d, ok := h.device(w, r)
	if !ok {
		return
	}
	h.renderForm(w, r, http.StatusOK, deviceForm{Device: d, HasPassword: d.HTTPPassword != "", Stored: d.Name, MQTTMode: d.MQTTPasswordMode})
}

func (h *handler) deviceUpdate(w http.ResponseWriter, r *http.Request) {
	old, ok := h.device(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	d, err := applyForm(r, old) // starts from the stored record, so the password and identity survive
	form := deviceForm{Device: d, HasPassword: d.HTTPPassword != "", Stored: old.Name, MQTTMode: old.MQTTPasswordMode}
	var mq mqttChange
	if h.d.MQTTPasswords != nil {
		var msg string
		mq, form.MQTTMode, msg = mqttFormChange(r, old)
		form.MQTTRetype = r.PostFormValue(fieldMQTTPassword) != "" // never echoed: a re-render drops it
		if err == nil && msg != "" {                               // nothing is saved: the other fields stay as typed
			form.ErrField, form.Error = fieldMQTTPassword, msg
			h.renderForm(w, r, http.StatusUnprocessableEntity, form)
			return
		}
	}
	if err == nil {
		ctx := r.Context()
		if token := r.PostFormValue("token"); token != "" {
			pc, ok := h.cmds.take(h.d.Clock.Now(), token, h.bindingOf(r))
			if want := "edit:" + strconv.FormatInt(int64(old.ID), 10) + ":"; ok && strings.HasPrefix(pc.cmd, want) {
				ctx = inventory.WithUnverifiedMoveConfirmed(ctx, strings.TrimPrefix(pc.cmd, want))
			}
		}
		err = h.d.Inventory.Update(ctx, actor(r), d)
		var um *inventory.UnverifiedMoveError
		if errors.As(err, &um) {
			token := h.cmds.put(h.d.Clock.Now(), h.bindingOf(r), "edit:"+strconv.FormatInt(int64(old.ID), 10)+":"+um.Target, nil)
			form.Unverified = &unverifiedMove{Target: um.Target, Token: token}
			h.renderForm(w, r, http.StatusConflict, form)
			return
		}
	}
	if err != nil {
		form.ErrField, form.Error = formProblem(err)
		if errors.Is(err, store.ErrNotFound) {
			h.notFoundDevice(w, r)
			return
		}
		h.renderForm(w, r, http.StatusUnprocessableEntity, form)
		return
	}
	if err := h.saveMQTTPassword(r, d.ID, old, mq); err != nil {
		h.fail(w, r, err) // the other fields are saved; the log has the cause
		return
	}
	redirect(w, r, "/devices/"+strconv.FormatInt(int64(d.ID), 10)+"?ok=saved")
}

// deviceDelete deletes a device. backups=keep keeps its backups (they stay
// listed under Settings -> Backups with the device's name and MAC);
// backups=delete deletes them first, kept ones included. Either choice needs
// the acknowledgement ack=1, checked here, not only in the browser. A form
// without the backups field (the dialogs of v0.10) deletes the backups as
// before; the device delete page of N2 always sends the field.
func (h *handler) deviceDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := h.deviceID(w, r)
	if !ok {
		return
	}
	limitBody(w, r)
	choice := r.PostFormValue("backups")
	switch choice {
	case "keep", "delete":
		if r.PostFormValue("ack") != "1" {
			h.refuseDeviceDelete(w, r, id, devDeleteView{Choice: choice, AckErr: true})
			return
		}
	case "":
		choice = "delete"
	default:
		h.refuseDeviceDelete(w, r, id, devDeleteView{ChoiceErr: true, AckErr: r.PostFormValue("ack") != "1"})
		return
	}
	if choice == "delete" {
		if del, ok := h.backupBulkDeleter(); ok {
			if _, err := h.d.Inventory.Get(r.Context(), id); errors.Is(err, store.ErrNotFound) {
				h.notFoundDevice(w, r)
				return
			}
			if _, err := del.DeleteAll(r.Context(), actor(r), id); err != nil {
				h.fail(w, r, err)
				return
			}
		}
	}
	switch err := h.d.Inventory.DeleteWith(r.Context(), actor(r), id, inventory.DeleteOptions{Backups: choice}); {
	case errors.Is(err, store.ErrNotFound):
		h.notFoundDevice(w, r)
	case err != nil:
		h.fail(w, r, err)
	default:
		redirect(w, r, "/devices?ok=deleted")
	}
}

// defaultPasswordFor returns the fleet default device password (N20) when the
// form asks for it (use_default_password=1, the prefill checkbox), else "".
// The password itself never goes to the browser: the form only shows whether
// a default exists, and the server fills it in here.
func (h *handler) defaultPasswordFor(r *http.Request) string {
	if r.PostFormValue("use_default_password") != "1" || h.d.DefaultDevicePassword == nil {
		return ""
	}
	return h.d.DefaultDevicePassword()
}

// refuseDeviceDelete answers a refused delete (422) with the delete page again, the field
// errors marked and the posted choice kept; nothing was deleted.
func (h *handler) refuseDeviceDelete(w http.ResponseWriter, r *http.Request, id model.DeviceID, p devDeleteView) {
	d, err := h.d.Inventory.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		h.notFoundDevice(w, r)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.renderDeviceDelete(w, r, http.StatusUnprocessableEntity, d, p)
}
