package web

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/a-h/templ"

	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
)

// SetupService backs the first-run setup. Optional: without it there is no setup page.
type SetupService interface {
	// Done reports whether the setup was finished or skipped.
	Done() bool
	// Finish records that the setup is over.
	Finish(ctx context.Context, actor string) error
	// Suggestions are private subnets of the machine's own interfaces. They are only
	// suggestions: nothing is configured until the admin saves them.
	Suggestions() []netip.Prefix
	// NetworksConfigured reports whether device networks are set (without them no device is contacted).
	NetworksConfigured() bool
}

const setupPath = "/setup"

func (h *handler) registerSetup(route func(string, func(http.Handler) http.Handler, http.HandlerFunc), adm func(http.Handler) http.Handler) {
	if h.d.Setup == nil || h.d.SysConfig == nil {
		return
	}
	route("GET "+setupPath, adm, h.setupPage)
	route("POST "+setupPath+"/mqtt-test", adm, h.setupMQTTTest)
	route("POST "+setupPath+"/finish", adm, h.setupFinish)
}

// setupPage is the last step of the first-run wizard (design v2 "setup", step 3 "Basic
// settings"): the setup token and the admin account (steps 1 and 2, N12) are done when an admin
// can open it. One form: the device networks, the broker, and whether to scan when finishing.
type setupPage struct {
	Base, CSRF      string
	Version, Host   string // the card's footer
	Networks        string // DEVICE_NETWORKS as typed or stored, else the suggestion
	NetworksHelp    string // the help text of the field in System settings, so both pages say the same
	Suggested       string // comma separated suggestions, shown as a proposal only
	MQTT            sysMQTT
	MQTTUser        string
	MQTTHasPassword bool
	MQTTResult      string
	Scan            bool // scan the networks when finishing (the prototype's default: on)
	CanScan         bool // discovery is available
	Error           string
	Errors          map[string]string
}

// setupKeys are the settings the setup may change.
var setupKeys = map[string]bool{"DEVICE_NETWORKS": true, "MQTT_URL": true, "MQTT_USER": true, "MQTT_PASSWORD": true}

func (h *handler) setupData(r *http.Request, posted, errs map[string]string) setupPage {
	p := setupPage{Base: httpBase(r), CSRF: h.d.Guard.CSRFToken(r), Version: h.d.Version, Host: r.Host,
		Errors: errs, Scan: true, CanScan: h.d.Discovery != nil}
	var sugg []string
	for _, s := range h.d.Setup.Suggestions() {
		sugg = append(sugg, s.String())
	}
	p.Suggested = strings.Join(sugg, ",")
	if f, ok := sysconfig.Lookup("DEVICE_NETWORKS"); ok {
		p.NetworksHelp = strings.TrimSpace(f.Help)
	}
	url := ""
	for _, st := range h.d.SysConfig.State() {
		switch st.Key {
		case "DEVICE_NETWORKS":
			p.Networks = st.Value
		case "MQTT_URL":
			url = st.Value
		case "MQTT_USER":
			p.MQTTUser = st.Value
		case "MQTT_PASSWORD":
			p.MQTTHasPassword = st.HasValue
		}
	}
	if v, ok := posted["DEVICE_NETWORKS"]; ok {
		p.Networks = v
	}
	if v, ok := posted["MQTT_URL"]; ok {
		url = v
	}
	if v, ok := posted["MQTT_USER"]; ok {
		p.MQTTUser = v
	}
	if p.Networks == "" {
		p.Networks = p.Suggested
	}
	p.MQTT = sysMQTTOf(url)
	if _, ok := posted["scan"]; posted != nil && !ok {
		p.Scan = false
	}
	return p
}

func (h *handler) setupRender(w http.ResponseWriter, r *http.Request, status int, p setupPage) {
	v := h.view(r, "Set up TasmoFleet", "setup", nil)
	b, err := h.bare(r, v, setupView(p))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writePage(w, status, b)
}

func (h *handler) setupPage(w http.ResponseWriter, r *http.Request) {
	h.setupRender(w, r, http.StatusOK, h.setupData(r, nil, nil))
}

// setupForm reads the setup form: only the setup's own settings, the broker parts and the scan
// box; anything else fails the whole request. The broker parts become MQTT_URL.
func (h *handler) setupForm(w http.ResponseWriter, r *http.Request, extras ...string) (values, extra map[string]string, problem error, ok bool) {
	values, _, extra, ok = h.sysForm(w, r, false, append(extras, append([]string{"scan"}, mqttPartKeys...)...)...)
	if !ok {
		return nil, nil, nil, false
	}
	for k := range values {
		if !setupKeys[k] {
			http.Error(w, "unknown form field", http.StatusBadRequest)
			return nil, nil, nil, false
		}
	}
	return values, extra, composeMQTTURL(values, extra, h.storedMQTTURL()), true
}

// setupShown is what the page shows again after a refused form: the typed values and the box.
func setupShown(values, extra map[string]string) map[string]string {
	shown := map[string]string{}
	for k, v := range values {
		shown[k] = v
	}
	if extra["scan"] == "1" {
		shown["scan"] = "1"
	}
	return shown
}

func (h *handler) setupMQTTTest(w http.ResponseWriter, r *http.Request) {
	values, extra, problem, ok := h.setupForm(w, r)
	if !ok {
		return
	}
	p := h.setupData(r, setupShown(values, extra), nil)
	var ve *sysconfig.ValidationError
	if asValidation(problem, &ve) {
		p.Errors, p.Error = ve.Fields, "Fix the marked fields."
		h.setupRender(w, r, http.StatusUnprocessableEntity, p)
		return
	}
	test := map[string]string{}
	for _, k := range []string{"MQTT_URL", "MQTT_USER"} {
		if v, ok := values[k]; ok {
			test[k] = v
		}
	}
	msg, err := h.d.SysConfig.TestMQTT(r.Context(), sysconfig.TestRequest{Actor: actor(r), Values: test, Password: values["MQTT_PASSWORD"], StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	switch {
	case err == nil:
		p.MQTTResult = msg
		h.setupRender(w, r, http.StatusOK, p)
	case asValidation(err, &ve):
		p.Errors, p.Error = ve.Fields, "Fix the marked fields."
		h.setupRender(w, r, http.StatusUnprocessableEntity, p)
	default:
		p.Error = "The connection test could not run (wait a few seconds between tests)."
		h.setupRender(w, r, http.StatusTooManyRequests, p)
	}
}

// setupFinish saves what the form holds, ends the setup and, when asked, scans the device
// networks (each range of at most /22) and shows that scan. A refused value ends nothing.
func (h *handler) setupFinish(w http.ResponseWriter, r *http.Request) {
	values, extra, problem, ok := h.setupForm(w, r)
	if !ok {
		return
	}
	err := problem
	if err == nil && len(values) > 0 {
		_, err = h.d.SysConfig.Apply(r.Context(), sysconfig.Request{Actor: actor(r), Values: values, StepUp: h.sysStepUp(r), Audit: h.d.Recorder})
	}
	if err != nil {
		var ve *sysconfig.ValidationError
		if asValidation(err, &ve) {
			p := h.setupData(r, setupShown(values, extra), ve.Fields)
			p.Error = "Nothing was saved and the setup is not finished. Fix the marked fields."
			h.setupRender(w, r, http.StatusUnprocessableEntity, p)
			return
		}
		h.fail(w, r, err)
		return
	}
	if err := h.d.Setup.Finish(r.Context(), actor(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	if extra["scan"] == "1" && h.d.Discovery != nil {
		if cidrs := setupScanRanges(h.d.SysConfig.State()); len(cidrs) > 0 {
			if id, err := h.d.Discovery.StartScan(r.Context(), actor(r), cidrs); err == nil {
				redirect(w, r, "/discovery/scan/"+string(id))
				return
			}
			redirect(w, r, "/devices/new#card-scan") // the scan was refused (one is running): the page says why when started there
			return
		}
	}
	redirect(w, r, "/")
}

// setupScanRanges are the stored device networks a scan may cover: IPv4, at most /22 each.
func setupScanRanges(state []sysconfig.FieldState) []netip.Prefix {
	var out []netip.Prefix
	for _, st := range state {
		if st.Key != "DEVICE_NETWORKS" {
			continue
		}
		for _, c := range strings.Split(st.Value, ",") {
			if pf, err := netip.ParsePrefix(strings.TrimSpace(c)); err == nil && pf.Addr().Is4() && pf.Bits() >= 22 {
				out = append(out, pf.Masked())
			}
		}
	}
	return out
}

func asValidation(err error, target **sysconfig.ValidationError) bool { return errors.As(err, target) }

// setupInvalid marks a field the server rejected and points to its message.
func setupInvalid(p setupPage, key string) templ.Attributes {
	if p.Errors[key] == "" {
		return templ.Attributes{}
	}
	return templ.Attributes{"aria-invalid": "true", "aria-describedby": "err-v." + key}
}

func setupSecretHint(stored bool) string {
	if stored {
		return "unchanged (a password is stored)"
	}
	return "not set"
}
