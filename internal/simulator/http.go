package simulator

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// startHTTP binds the device's HTTP port (the same port after a reboot) and
// serves in the background.
func (d *simDevice) startHTTP() {
	d.mu.Lock()
	addr := d.httpAddr
	d.mu.Unlock()
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	l, err := d.listen(addr)
	if err != nil {
		return // device closed while rebinding
	}
	srv := &http.Server{Handler: d, ReadHeaderTimeout: 10 * time.Second}
	d.mu.Lock()
	d.httpAddr = l.Addr().String()
	d.srv = &httpServer{srv: srv, l: l}
	d.mu.Unlock()
	if d.hasFault(FaultHTTPUnreachable) {
		// The address stays (the inventory holds it) but nothing listens: connections are
		// refused, also after every reboot, like a device on another network.
		_ = l.Close() // the point is the refusal
		return
	}
	served := d.spawn(func() {
		// net.ErrClosed: a reboot closed the listener first (reboot)
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			d.f.log.Warn("simulator http serve", "device", d.topic, "err", err)
		}
	})
	if !served {
		_ = srv.Close() // device closed while rebinding; only releases the listener
	}
}

func (d *simDevice) stopHTTP() {
	d.mu.Lock()
	s := d.srv
	d.srv = nil
	d.mu.Unlock()
	if s != nil {
		_ = s.srv.Close() // closes the listener and all connections; error is not actionable
	}
}

// drop closes the client connection without any reply.
func drop(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			_ = conn.Close() // deliberate: the point is to cut the connection
		}
	}
}

// waitServing blocks while the device cannot answer (blocking download,
// restart wait, hanging reboot). It returns false when the request must be
// dropped: the client gave up or the device rebooted underneath it.
func (d *simDevice) waitServing(r *http.Request) bool {
	d.mu.Lock()
	gen := d.boots
	d.mu.Unlock()
	for {
		d.mu.Lock()
		blocked := d.phase == PhaseDownloading || d.phase == PhaseRestartWait || d.phase == PhaseRebooting
		ch := d.phaseCh
		changed := d.boots != gen
		d.mu.Unlock()
		if changed {
			return false
		}
		if !blocked {
			return true
		}
		select {
		case <-ch:
		case <-r.Context().Done():
			return false
		case <-d.ctx.Done():
			return false
		}
		d.mu.Lock()
		rebooting := d.phase == PhaseRebooting
		d.mu.Unlock()
		if rebooting && !d.hangsOnReboot() {
			return false
		}
	}
}

func (d *simDevice) hangsOnReboot() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hasFaultLocked(FaultRebootHang)
}

func (d *simDevice) setHeaders(w http.ResponseWriter, file, ctype string) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Server", "Tasmota/"+d.versionNow()+" ("+d.hardware+")")
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "-1")
	h.Set("Accept-Ranges", "none")
	h.Set("Connection", "close")
}

func (d *simDevice) versionNow() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.version
}

func (d *simDevice) authFailed(r *http.Request) bool {
	if d.spec.Password == "" {
		return false
	}
	user := d.spec.User
	if user == "" {
		user = "admin"
	}
	// Real Tasmota accepts either HTTP Basic auth or user=/password= query
	// parameters; one valid form is enough.
	if u, p, ok := r.BasicAuth(); ok && u == user && p == d.spec.Password {
		return false
	}
	q := r.URL.Query()
	return q.Get("user") != user || q.Get("password") != d.spec.Password
}

func (d *simDevice) writeBody(w http.ResponseWriter, code int, body []byte) {
	w.WriteHeader(code)
	if f, ok := w.(http.Flusher); ok {
		f.Flush() // flush before the body so the reply is chunked like the real web server
	}
	_, _ = w.Write(body) //nolint:gosec // simulated device reply (JSON/HTML stand-in), not user content
}

func (d *simDevice) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !d.waitServing(r) {
		drop(w)
		return
	}
	if d.spec.RefererCheck && refererGuarded(r.URL.Path) && !d.refererOK(r) {
		drop(w) // nothing is executed and nothing is recorded
		return
	}
	cmd := ""
	if r.URL.Path == "/cm" {
		cmd = strings.TrimSpace(r.URL.Query().Get("cmnd"))
	}
	if r.URL.Path == "/cm" && cmd != "" {
		d.record(cmd)
	}
	d.mu.Lock()
	authFault := len(d.faultsForLocked(cmd, FaultAuth)) > 0
	unknownFault := len(d.faultsForLocked(cmd, FaultUnknownCommand)) > 0
	d.mu.Unlock()
	if authFault || d.authFailed(r) {
		d.setHeaders(w, "", "application/json")
		w.Header().Set("WWW-Authenticate", `Basic realm="Tasmota"`)
		d.writeBody(w, http.StatusUnauthorized, []byte(`{"WARNING":"Need user=<username>&password=<password>"}`))
		return
	}
	switch r.URL.Path {
	case "/cm":
		d.serveCommand(w, r, cmd, unknownFault)
	case "/":
		d.servePage(w, "root.html")
	case "/up":
		d.mu.Lock()
		d.uploadArmed = true // HandleUpgradeFirmware sets the upload type
		d.mu.Unlock()
		d.servePage(w, "up.html")
	case "/u2":
		d.serveU2(w, r)
	case "/u4":
		d.serveU4(w, r)
	case "/u1":
		d.serveU1(w, r)
	case "/dl":
		d.serveDump(w)
	case "/ufsd":
		d.serveUfsd(w, r)
	case "/ufsu":
		d.serveUfsu(w, r)
	default:
		d.setHeaders(w, "", "text/plain")
		d.writeBody(w, http.StatusNotFound, []byte(fmt.Sprintf("File Not Found\n\nURI: %s\nMethod: %s\nArguments: %d\n",
			r.URL.Path, r.Method, len(r.URL.Query()))))
	}
}

// refererGuarded: the handlers that start with HttpCheckPriviledgedAccess. The
// main page does not (it only asks for the web password), and an unknown path
// gets the 404 page.
func refererGuarded(path string) bool {
	switch path {
	case "/cm", "/dl", "/up", "/u1", "/u2", "/u4", "/ufsd", "/ufsu":
		return true
	}
	return false
}

// refererOK is HttpCheckPriviledgedAccess's rule: the upper-cased Referer has
// the hostname or the IP of the device at index 7, right after "http://".
func (d *simDevice) refererOK(r *http.Request) bool {
	ref := strings.ToUpper(r.Header.Get("Referer"))
	if ref == "" {
		return false
	}
	d.mu.Lock()
	hostname := d.get("hostname") // the current one: the Hostname command changes it
	d.mu.Unlock()
	return strings.Index(ref, strings.ToUpper(hostname)) == 7 || strings.Index(ref, strings.ToUpper(d.host)) == 7
}

func (d *simDevice) record(cmd string) {
	d.mu.Lock()
	d.cmds = append(d.cmds, redactFileCmd(cmd))
	d.mu.Unlock()
}

func (d *simDevice) serveCommand(w http.ResponseWriter, r *http.Request, cmd string, unknown bool) {
	var body []byte
	var out []outMsg
	if unknown && cmd != "" {
		_, base, idx, hasIdx, data := splitCmd(cmd)
		body = unknownReply(base, idx, hasIdx, data)
		out = []outMsg{{topic: "stat/" + d.topic + "/RESULT", payload: string(body)}}
	} else {
		body, out = d.exec(cmd)
	}
	d.publishAll(out)
	d.mu.Lock()
	dropF := len(d.faultsForLocked(cmd, FaultDrop)) > 0
	timeouts := d.faultsForLocked(cmd, FaultTimeout)
	d.mu.Unlock()
	if dropF {
		drop(w)
		return
	}
	if len(timeouts) > 0 {
		wait := timeouts[0].Duration
		if wait <= 0 {
			select { // the reply never comes
			case <-r.Context().Done():
			case <-d.ctx.Done():
			}
			drop(w)
			return
		}
		if !d.sleepFor(wait) {
			drop(w)
			return
		}
	}
	d.setHeaders(w, "", "application/json")
	d.writeBody(w, http.StatusOK, body)
}

func (d *simDevice) publishAll(out []outMsg) {
	for _, m := range out {
		d.mq.publish(m.topic, m.payload, m.retain)
	}
}

func (d *simDevice) servePage(w http.ResponseWriter, file string) {
	d.mu.Lock()
	minimal := d.minimalLocked()
	v := d.viewLocked()
	image, version := d.image, d.version
	d.mu.Unlock()
	d.setHeaders(w, file, "text/html")
	if minimal && file == "root.html" {
		title := strings.TrimSuffix(strings.TrimPrefix(string(d.cap.files["root-title.txt"]), "<title>"), "")
		d.writeBody(w, http.StatusOK, []byte("<!DOCTYPE html><html lang=\"en\" class=\"\"><head><meta charset='utf-8'>"+
			d.titleFor(title)+"</head><body><div style='text-align:left;display:inline-block;min-width:340px;'>"+
			"<h3>MINIMAL firmware<br>please upgrade</h3><h2>"+d.deviceName(v)+"</h2>"+
			"<p><form action='up' method='get'><button>Firmware Upgrade</button></form></p>"+
			"<p><form action='cs' method='get'><button>Console</button></form></p>"+
			"<p><form action='.' method='get' onsubmit='return confirm(\"Confirm Restart\");'><button name='rst' class='button bred'>Restart</button></form></p>"+
			"<div style='text-align:right;font-size:11px;'><hr/><a href='https://bit.ly/tasmota' target='_blank' style='color:#aaa;'>Tasmota "+
			version+" (release-"+image+") by Theo Arends</a></div></div></body></html>"))
		return
	}
	body := d.render(file, v)
	if file == "up.html" && d.isSafeboot() {
		body = plainUploadForm(body) // safeboot serves the plain form even on the single-OTA layout
	}
	d.writeBody(w, http.StatusOK, body)
}

func (d *simDevice) titleFor(captured string) string {
	return "<title>" + strings.TrimPrefix(strings.TrimSpace(captured), "<title>") + "</title>"
}

func (d *simDevice) deviceName(view) string {
	t := strings.TrimSuffix(strings.TrimPrefix(string(d.cap.files["root-title.txt"]), "<title>"), " Main Menu")
	return strings.TrimSpace(t)
}

// serveU1 is the web UI's "start upgrade" link: OtaUrl <o>, then Upgrade 1.
func (d *simDevice) serveU1(w http.ResponseWriter, r *http.Request) {
	var out []outMsg
	if o := r.URL.Query().Get("o"); o != "" {
		d.record("OtaUrl " + o)
		_, m := d.exec("OtaUrl " + o)
		out = append(out, m...)
	}
	d.record("Upgrade 1")
	_, m := d.exec("Upgrade 1")
	out = append(out, m...)
	d.publishAll(out)
	d.setHeaders(w, "", "text/html")
	d.writeBody(w, http.StatusOK, []byte("<html><body><div>Upgrade started ...</div></body></html>"))
}

func (d *simDevice) serveDump(w http.ResponseWriter) {
	d.mu.Lock()
	minimal := d.minimalLocked()
	dump := d.configDump
	if dump == nil {
		dump = d.dumpLocked()
	}
	ver := d.version
	d.mu.Unlock()
	if minimal {
		d.setHeaders(w, "", "text/plain")
		d.writeBody(w, http.StatusNotFound, []byte("File Not Found\n\nURI: /dl\nMethod: GET\nArguments: 0\n"))
		return
	}
	d.setHeaders(w, "", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=Config_"+d.topic+"_"+ver+".dmp")
	d.writeBody(w, http.StatusOK, dump)
}
