package simulator

import (
	"io"
	"net/http"
	"strconv"
)

// Web-upload faults (POST /u2, "Upgrade by file upload"). They apply to the
// upload only; After is "" or "u2".
const (
	FaultUploadCut       = "upload_cut"        // connection closed halfway through the body; nothing is flashed
	FaultUploadLostReply = "upload_lost_reply" // image flashed and restart scheduled, the reply never arrives
	FaultSlowWrite       = "slow_write"        // the device consumes the body Duration late (slow flash writes)
	// FaultUploadNoSpace: an ESP32 with the safeboot partition layout (one
	// app partition). The running firmware has nowhere to write a full image:
	// Update.begin fails and HandleUploadDone answers "Not enough space";
	// nothing is flashed and the device keeps running. The download path
	// (OtaUrl + Upgrade, through safeboot) is not affected.
	FaultUploadNoSpace = "upload_no_space"
)

// uploadPage is a Tasmota result page of the upload (HandleUploadDone).
func uploadPage(ok bool, msg string) []byte {
	if ok {
		return []byte("<html><head><script>setTimeout(function(){location.href='.';},10000);</script></head><body>" +
			"<div style='text-align:center;'><b>Upload <font color='#008000'>Successful</font></b><br>" +
			"<div style='text-align:center;'><b>Device will restart in a few seconds</b></div></div></body></html>")
	}
	return []byte("<html><body><div style='text-align:center;'><b>Upload <font color='#ff5661'>Failed</font></b><br><br>" +
		msg + "</div></body></html>")
}

// serveU2 models Tasmota's HandleUploadLoop/HandleUploadDone. The trap it
// reproduces on purpose: the upload type is set by a GET of /up, not by the
// POST. A POST /u2 without it flashes nothing, yet the done page says
// "Successful" and the device restarts.
func (d *simDevice) serveU2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		d.setHeaders(w, "", "text/plain")
		d.writeBody(w, http.StatusNotFound, []byte("File Not Found\n\nURI: /u2\nMethod: "+r.Method+"\nArguments: 0\n"))
		return
	}
	d.mu.Lock()
	kind := d.uploadType
	d.uploadType = uploadNone // HandleUploadDone clears it
	cut := len(d.faultsForLocked("u2", FaultUploadCut)) > 0
	lost := len(d.faultsForLocked("u2", FaultUploadLostReply)) > 0
	slow := d.faultsForLocked("u2", FaultSlowWrite)
	wrong := len(d.faultsForLocked("u2", FaultWrongFamily)) > 0
	noSpace := len(d.faultsForLocked("u2", FaultUploadNoSpace)) > 0 ||
		(d.isSafebootLocked() && len(d.faultsForLocked("u2", FaultSafebootRefuseUpload)) > 0)
	d.mu.Unlock()
	reply := func(ok bool, msg string) {
		d.setHeaders(w, "", "text/html")
		d.writeBody(w, http.StatusOK, uploadPage(ok, msg))
	}
	switch kind {
	case uploadSettings:
		d.restoreUpload(w, r)
		return
	case uploadFirmware:
	case uploadNone, uploadUFS: // or the file type a GET /ufsd left: nothing is flashed or applied
		_, _ = io.Copy(io.Discard, r.Body)
		d.record("POST /u2 (no upload type set)")
		d.spawn(d.restartSoon)
		reply(true, "")
		return
	}
	d.setPhase(PhaseDownloading) // the single-threaded web server is busy while it writes
	fail := func(msg string) {
		d.setPhase(PhaseRun)
		reply(false, msg)
	}
	if len(slow) > 0 && !d.sleepFor(slow[0].Duration) {
		drop(w)
		return
	}
	if cut {
		d.record("POST /u2 (connection cut)")
		_, _ = io.CopyN(io.Discard, r.Body, r.ContentLength/2)
		d.setPhase(PhaseRun)
		drop(w)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		fail("No file selected")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "u2" || part.FileName() == "" {
		fail("No file selected")
		return
	}
	body, err := io.ReadAll(io.LimitReader(part, maxImage+1))
	if err != nil {
		d.setPhase(PhaseRun)
		drop(w)
		return
	}
	d.record("POST /u2 " + part.FileName() + " " + strconv.Itoa(len(body)) + " fsz=" + r.URL.Query().Get("fsz"))
	switch {
	case len(body) > maxImage:
		fail("File too large")
		return
	case len(body) == 0 || (body[0] != 0xE9 && body[0] != 0x1F): // 0x1F is gzipped 0xE9
		fail("Invalid file signature")
		return
	case noSpace:
		fail("Not enough space")
		return
	}
	raw, err := unpack(body)
	if err != nil {
		fail("Upload failed. Enable logging 3")
		return
	}
	fam, err := imageFamily(raw)
	if wrong || err != nil || d.chipFamily == "" || fam != d.chipFamily {
		fail("Upload failed. Enable logging 3") // the bootloader check at Update.end()
		return
	}
	version, ok := findVersion(raw)
	if !ok {
		version = d.f.x.FlashVersion
	}
	d.mu.Lock()
	d.armLocked("u2")
	d.nextImage = &flashed{kind: imageKind(part.FileName(), d.esp32), version: version}
	d.setPhaseLocked(PhaseRestartWait)
	d.mu.Unlock()
	d.spawn(func() {
		if d.sleep(secRestartCmd) {
			d.reboot(true)
		}
	})
	if lost {
		drop(w)
		return
	}
	reply(true, "")
}
