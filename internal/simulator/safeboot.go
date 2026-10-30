package simulator

import (
	"net/http"
	"strings"
)

// Faults of the switch into and out of safeboot (/u4) and of safeboot itself
// (push upload, #30). After is "" or "u4" for the switch faults.
const (
	// FaultSwitchLostReply: /u4 is acted on (the device restarts) but the reply never arrives.
	FaultSwitchLostReply = "switch_lost_reply"
	// FaultSwitchNoAnswer: /u4 is not acted on and the connection is cut without a reply.
	FaultSwitchNoAnswer = "switch_no_answer"
	// FaultSafebootRefuseUpload: while running safeboot, /u2 answers "Upload Failed";
	// nothing is flashed and the device stays in safeboot.
	FaultSafebootRefuseUpload = "safeboot_refuse_upload"
	// FaultStuckInSafeboot: every restart out of safeboot (after an upload or /u4?u4=ota)
	// comes back in safeboot, as if the boot partition could not be changed.
	FaultStuckInSafeboot = "stuck_in_safeboot"
)

// isSafebootLocked: the ESP32 runs its factory (safeboot) partition.
func (d *simDevice) isSafebootLocked() bool { return d.esp32 && d.image == "safeboot" }

func (d *simDevice) isSafeboot() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isSafebootLocked()
}

// singleOTA: the device has the safeboot layout (one app partition). The
// dual-layout profile has two and no factory partition to switch to.
func (d *simDevice) singleOTA() bool { return d.esp32 && !d.def.DualOTA }

// plainUploadForm turns the safeboot-layout /up page (the button calls
// upl(this)) into the plain one, which is what safeboot itself and a
// dual-partition device serve (HTTP_FORM_RST_UPG).
func plainUploadForm(page []byte) []byte {
	s := strings.ReplaceAll(string(page), "return upl(this);", "this.form.submit();")
	return []byte(strings.Replace(s, "<body>", "<body><span style='text-align:center;color:var(--c_txtwrn);'><h3>SAFEBOOT</h3></span>", 1))
}

// serveU4 models HandleSwitchBootPartition (xdrv_01_9_webserver.ino:3845).
// api mode answers "false" (a restart into the target was triggered), "true"
// (single-OTA layout, nothing to do) or "none" (no factory partition).
func (d *simDevice) serveU4(w http.ResponseWriter, r *http.Request) {
	if !d.esp32 || r.Method != http.MethodGet {
		d.setHeaders(w, "", "text/plain")
		d.writeBody(w, http.StatusNotFound, []byte("File Not Found\n\nURI: /u4\nMethod: "+r.Method+"\nArguments: 0\n"))
		return
	}
	q := r.URL.Query()
	_, api := q["api"]
	target := q.Get("u4")
	d.mu.Lock()
	single, factory := d.singleOTA(), d.isSafebootLocked()
	noAnswer := len(d.faultsForLocked("u4", FaultSwitchNoAnswer)) > 0
	lost := len(d.faultsForLocked("u4", FaultSwitchLostReply)) > 0
	d.mu.Unlock()
	if noAnswer {
		d.record("GET /u4 " + target + " (no answer)")
		drop(w)
		return
	}
	toSafeboot := target == "fct" && single && !factory
	toApp := target == "ota" && (!single || factory) // a dual layout would switch its boot partition
	if !toSafeboot && !toApp {
		d.record("GET /u4 " + target + " (nothing to do)")
		reply := "none"
		if single {
			reply = "true"
		}
		d.setHeaders(w, "", "text/plain")
		if api {
			d.writeBody(w, http.StatusOK, []byte(reply))
			return
		}
		w.Header().Set("Location", "/")
		d.writeBody(w, http.StatusFound, nil)
		return
	}
	d.record("GET /u4 " + target)
	d.mu.Lock()
	d.commitLocked() // SettingsSaveAll: only the normal firmware writes flash
	if toSafeboot {
		d.appImage, d.appVersion = d.image, d.version
		d.nextImage = &flashed{kind: "safeboot", version: d.safebootVersion}
	} else if d.appImage != "" {
		d.nextImage = &flashed{kind: d.appImage, version: d.appVersion}
	}
	d.mu.Unlock()
	d.spawn(d.restartSoon)
	if lost {
		drop(w)
		return
	}
	d.setHeaders(w, "", "text/plain")
	d.writeBody(w, http.StatusOK, []byte("false"))
}
