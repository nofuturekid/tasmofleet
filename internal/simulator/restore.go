package simulator

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

// configDumpSize is the size of a Tasmota settings dump on ESP8266/ESP32
// (decode-config accepts 4096 bytes); anything else is rejected like the
// real firmware rejects a bad file.
const configDumpSize = 4096

// uploadKind is Tasmota's Web.upload_file_type as far as the simulator models it.
type uploadKind int

const (
	uploadNone     uploadKind = iota // UPL_NONE: a POST /u2 applies nothing, yet says Successful
	uploadFirmware                   // UPL_TASMOTA, set by GET /up
	uploadSettings                   // UPL_SETTINGS, set by GET /rs
	uploadUFS                        // UPL_UFSFILE, set by GET /ufsd
)

// rsPage is the "Restore configuration" page as HandleRestoreConfiguration writes it (v16
// xdrv_01_9_webserver.ino 3035-3053 with HTTP_FORM_RST_UPG): the same upload form as /up.
func rsPage(name string) []byte {
	return []byte("<!DOCTYPE html><html lang=\"en\" class=\"\"><head><meta charset='utf-8'><title>" + name + " Restore</title></head><body>" +
		"<fieldset><legend><b>&nbsp;Restore Configuration&nbsp;</b></legend>" +
		"<form method='post' action='u2?fsz=' enctype='multipart/form-data'><br><input type='file' name='u2'><br>" +
		"<br><button type='submit'>Start Restore</button></form></fieldset></body></html>")
}

// serveRs models HandleRestoreConfiguration: the page sets the settings upload type. The minimal
// firmware registers no /rs.
func (d *simDevice) serveRs(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	minimal := d.minimalLocked()
	name := d.deviceName(d.viewLocked())
	if !minimal {
		d.uploadType = uploadSettings
	}
	d.mu.Unlock()
	if minimal {
		d.notFound(w, r)
		return
	}
	d.setHeaders(w, "", "text/html")
	d.writeBody(w, http.StatusOK, rsPage(name))
}

// restoreUpload is the settings branch of HandleUploadLoop and HandleUploadDone: the dump is
// checked (SettingsConfigRestore), applied and the device restarts. A dump of the wrong size is
// refused with the device's messages and changes nothing.
func (d *simDevice) restoreUpload(w http.ResponseWriter, r *http.Request) {
	reply := func(ok bool, msg string) {
		d.setHeaders(w, "", "text/html")
		d.writeBody(w, http.StatusOK, uploadPage(ok, msg))
	}
	mr, err := r.MultipartReader()
	if err != nil {
		reply(false, "No file selected")
		return
	}
	part, err := mr.NextPart()
	if err != nil || part.FormName() != "u2" || part.FileName() == "" {
		reply(false, "No file selected")
		return
	}
	body, err := io.ReadAll(io.LimitReader(part, configDumpSize+1<<10))
	if err != nil {
		drop(w)
		return
	}
	d.record("POST /u2 " + part.FileName() + " " + strconv.Itoa(len(body)) + " (settings)")
	switch {
	case len(body) > configDumpSize:
		reply(false, "File too large")
		return
	case len(body) < configDumpSize:
		reply(false, "File invalid")
		return
	}
	d.mu.Lock()
	d.applyConfigLocked(body)
	lost := len(d.faultsForLocked("u2", FaultUploadLostReply)) > 0
	d.mu.Unlock()
	d.spawn(func() {
		if d.sleep(secRestartCmd) {
			d.reboot(false)
		}
	})
	if lost {
		drop(w) // applied and restarting, the reply never arrives
		return
	}
	reply(true, "")
}

// applyConfigLocked replaces the settings with a dump (SettingsConfigRestore); the caller
// restarts. Shared by the web restore upload and MQTT FileUpload Type 2. Like the firmware, which
// copies the whole settings block, a simulator dump that carries a boot counter brings it back.
func (d *simDevice) applyConfigLocked(dump []byte) {
	d.configDump = dump
	if len(dump) < len(dumpMagic) || string(dump[:len(dumpMagic)]) != dumpMagic {
		return
	}
	body := dump[len(dumpMagic):]
	end := len(body)
	for end > 0 && body[end-1] == 0 {
		end--
	}
	var doc struct{ Bootcount *int }
	if json.Unmarshal(body[:end], &doc) == nil && doc.Bootcount != nil {
		d.persisted.bootCount = *doc.Bootcount
	}
}
