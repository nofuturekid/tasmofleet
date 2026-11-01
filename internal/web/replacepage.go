package web

import (
	"fmt"
	"strings"

	"github.com/a-h/templ"

	"net"
	"net/http"
	"strconv"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The Replace device tab (design v2 screen "device", tab "replace", WP P10): pick the new
// device, preview what moves onto it, then start the replacement with one acknowledgement.

// replaceView is the tab, before and after the preview.
type replaceView struct {
	Base, CSRF  string
	Head        templ.Component
	Device      model.Device
	OldLine     string // "192.168.20.22 · ESP32"
	Error       string
	Missing     bool // the acknowledgement was not ticked
	Targets     []ui.Option
	Address     string // typed address (no candidate list, or "another address")
	Port        string
	Backups     []ui.Option // the dumps to choose from, newest first
	PreviewHref string      // the form's GET address
	// The preview
	Preview    bool
	Copied     []ui.KVRow
	NotCopied  []ui.KVRow // Key: what, Value: why
	Warnings   []string
	Files      *replaceFiles // the files row among the copied items; nil without files to write
	Checks     []string      // the always-on copy options
	Hidden     []ui.KVRow    // the confirmed form's fields
	Ack        string
	StartHref  string
	CancelHref string
}

// replaceFiles is the "Files (6, written first)" row of the preview, shown after the Wi-Fi row.
type replaceFiles struct {
	Key   string
	At    int // index in Copied before which the row is shown
	Names []fileRowView
}

// targetValue is a candidate as the select sends it.
func targetValue(addr string, port int) string {
	if port == 0 {
		return addr
	}
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

func (h *handler) renderReplace(w http.ResponseWriter, r *http.Request, status int, p replacePage) {
	v := h.view(r, "Replace "+p.Device.Name, "devices", nil)
	d := p.Device
	rv := replaceView{Base: v.Base, CSRF: v.CSRF, Device: d, Error: p.Error, Address: p.Address, Port: p.Port,
		PreviewHref: fmt.Sprintf("%s/devices/%d/replace/preview", v.Base, d.ID),
		CancelHref:  fmt.Sprintf("%s/devices/%d/replace", v.Base, d.ID)}
	if rv.Error == needConfirm {
		rv.Error, rv.Missing = "", true
	}
	rv.Head = deviceHeader(h.tabsOf(r, v, d, "replace"))
	rv.Checks = []string{"Copy name, topic and MQTT settings", "Copy rules, timers and PowerOnState", "Copy Wi-Fi credentials (the passwords come along, not shown)",
		"Keep the entry: its name, tags, notes and history stand for the new device"}
	rv.OldLine = d.Address
	if d.Identity.Family != "" {
		rv.OldLine += " · " + string(d.Identity.Family)
	}
	chosen := targetValue(p.Address, atoiOr0(p.Port))
	for _, c := range p.Candidates {
		val := targetValue(c.Address, c.Port)
		label := val
		if c.MAC != "" {
			label = c.MAC + " · " + val
		}
		rv.Targets = append(rv.Targets, ui.Option{Value: val, Label: label, Checked: val == chosen})
	}
	if len(rv.Targets) > 0 && p.Address != "" && !anyChecked(rv.Targets) {
		rv.Targets = nil // a typed address that is not in the list: show the fields
	}
	var dumps []model.Backup
	if p.Preview != nil {
		dumps = p.Preview.Dumps
	} else if list, err := h.d.Backups.List(r.Context(), d.ID); err == nil { // newest first
		for _, b := range list {
			if b.Kind == model.BackupConfigDump {
				dumps = append(dumps, b)
			}
		}
	} else {
		h.log.WarnContext(r.Context(), "replace: backups", "err", err) // only the backup choice goes
	}
	sel := p.Backup
	if p.Preview != nil {
		sel = p.Preview.Backup.ID
	}
	for i, b := range dumps {
		label := shortDate(b.TakenAt)
		if b.Version != "" {
			label += " · Tasmota " + b.Version
		}
		if p.Preview != nil && p.Preview.DumpFiles[b.ID] != "" {
			label += " · " + p.Preview.DumpFiles[b.ID]
		}
		if i == 0 {
			label += " · newest"
		}
		rv.Backups = append(rv.Backups, ui.Option{Value: bkStr(b.ID), Label: label, Checked: b.ID == sel || (sel == 0 && i == 0)})
	}
	if pv := p.Preview; pv != nil {
		rv.Preview = true
		rv.StartHref = fmt.Sprintf("%s/devices/%d/replace", v.Base, d.ID)
		c := pv.Carries
		rv.Copied = append(rv.Copied, ui.KVRow{Key: "Name and history", Value: d.Name + " (the entry keeps its tags, notes and history)"},
			ui.KVRow{Key: "Backup", Value: shortDate(pv.Backup.TakenAt) + " · Tasmota " + pv.Backup.Version, Mono: true})
		if c.Known {
			rv.Copied = append(rv.Copied,
				ui.KVRow{Key: "Hostname", Value: orNotSet(c.Hostname), Mono: true},
				ui.KVRow{Key: "IP address", Value: staticOrDHCP(c.StaticIP), Mono: true},
				ui.KVRow{Key: "MQTT topic and client", Value: orNotSet(c.MQTTTopic) + " · " + orNotSet(c.MQTTClient), Mono: true},
				ui.KVRow{Key: "Wi-Fi", Value: ssidText(c.SSIDs), Mono: true},
				ui.KVRow{Key: "Module and template", Value: moduleText(c.HasModule, c.Module, c.HasTemplate, c.Template, c.TemplateIOs), Mono: true})
		} else {
			rv.Copied = append(rv.Copied, ui.KVRow{Key: "Settings", Value: "the whole configuration (this backup has no decoded copy, so the details cannot be shown)"})
		}
		rv.NotCopied = []ui.KVRow{{Key: "MAC address", Value: "Belongs to the hardware: the entry records " + pv.New.MAC}}
		nFiles := len(pv.Files.Files)
		if pv.Files.Restores() {
			rv.Files = &replaceFiles{Key: fmt.Sprintf("Files (%d, written first)", nFiles), At: len(rv.Copied) - 1}
			if !c.Known {
				rv.Files.At = len(rv.Copied)
			}
			for _, f := range pv.Files.Files {
				rv.Files.Names = append(rv.Files.Names, fileRowView{Name: f.Name(), PrivateKey: f.PrivateKey})
			}
			rv.Checks = append(rv.Checks[:3:3], "Copy the files of its filesystem (autoexec.be, Berry scripts, display.ini, Zigbee data, TLS key)", rv.Checks[3])
		} else if nFiles > 0 {
			rv.NotCopied = append(rv.NotCopied, ui.KVRow{Key: fmt.Sprintf("Files (%d)", nFiles), Value: sentence(pv.Files.Why)})
		}
		for _, n := range pv.Files.NotStored {
			why := "Listed in the backup, not stored (" + strings.ToLower(n.Reason) + "). Copy it in the device's file manager if you need it."
			if strings.Contains(n.Reason, "1 MiB") {
				why = "Over 1 MiB: listed in the backup, not stored. Copy it in the device's file manager if you need it."
			}
			rv.NotCopied = append(rv.NotCopied, ui.KVRow{Key: strings.TrimPrefix(n.Path, "/") + " (" + humanBytes(n.Size) + ")", Value: why})
		}
		rv.NotCopied = append(rv.NotCopied,
			ui.KVRow{Key: "Firmware", Value: "The new device keeps Tasmota " + pv.New.Identity.Version},
			ui.KVRow{Key: "Its factory configuration", Value: "Backed up first, so it can be put back"})
		rv.Warnings = append(rv.Warnings, "The old device must stay switched off from now on: the new one carries the same hostname, MQTT topic and (if static) IP address. The old credentials come back on the new device. The download is plain HTTP on your device network.")
		if pv.Detail != "" {
			rv.Warnings = append(rv.Warnings, sentence(pv.Detail))
		}
		rv.Hidden = []ui.KVRow{{Key: "address", Value: p.Address}, {Key: "port", Value: p.Port}, {Key: "backup", Value: bkStr(pv.Backup.ID)}}
		if pv.Major {
			rv.Hidden = append(rv.Hidden, ui.KVRow{Key: "major_ok", Value: "1"})
		}
		if pv.Family {
			rv.Hidden = append(rv.Hidden, ui.KVRow{Key: "family_ok", Value: "1"})
		}
		old := "the old device"
		if pv.Old.MAC != "" {
			old = pv.Old.MAC
		}
		receives := "the configuration"
		if pv.Files.Restores() {
			receives = "the " + filesPlural(nFiles) + " and the configuration"
		}
		ack := "I understand that the new device (" + pv.New.MAC + ") is backed up first, then receives " + receives + " of the backup of " + shortDate(pv.Backup.TakenAt) + " and restarts, that " + d.Name + " then stands for it instead of " + old + ", and that the old credentials come back"
		if pv.Major {
			ack += "; that the backup is from another firmware major version"
		}
		if pv.Family {
			ack += "; and I confirm the chip family of the old device matches the new one"
		}
		rv.Ack = ack + "."
	}
	h.headPage(w, r, status, "replace", v, d, replaceTab(rv))
}

func atoiOr0(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func anyChecked(opts []ui.Option) bool {
	for _, o := range opts {
		if o.Checked {
			return true
		}
	}
	return false
}

func orNotSet(s string) string {
	if s == "" {
		return "not set"
	}
	return s
}

func staticOrDHCP(ip string) string {
	if ip == "" {
		return "DHCP"
	}
	return ip + " (static: the new device takes this address)"
}

func ssidText(ssids []string) string {
	if len(ssids) == 0 {
		return "none"
	}
	s := ""
	for i, x := range ssids {
		if i > 0 {
			s += ", "
		}
		s += x
	}
	return s + " (passwords come along, not shown)"
}

func moduleText(hasModule bool, module int, hasTemplate bool, name string, ios int) string {
	s := ""
	if hasModule {
		s = "module " + strconv.Itoa(module)
	}
	if hasTemplate {
		if s != "" {
			s += ", "
		}
		t := "(unnamed)"
		if name != "" {
			t = "“" + name + "”"
		}
		s += "template " + t + ", " + plural(ios, "GPIO") + " assigned"
	}
	if s == "" {
		return "not set"
	}
	return s
}
