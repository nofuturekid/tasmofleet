package web

import (
	"slices"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The Settings page is nine tabs; each tab is a grid of cards, each card names the settings it
// holds, in reading order. A setting that no card names lands in Advanced, "Other settings", so it
// can never vanish from the UI. Adding a setting is: its sysconfig field, its key in a card here,
// and its sysFieldCopy entry.

// sysCardDef is a card of a tab. Stack is the column stack it sits in (#391: cards stack directly
// under each other per column, see ui.CardStacks); cards are listed stack by stack, so the document
// order is the phone reading order, unless Phone gives a card another place. Depends is the
// data-depends rule that dims the card (sysform.js). Special cards (Backups storage, the deleted devices' backups) have no keys.
type sysCardDef struct {
	ID, Title, Note, Depends string
	Stack                    int
	Phone                    int  // 1-based place on a phone when it differs from the document order; 0 = document order
	StepUp                   bool // saving asks the admin to confirm: a badge in the header
	Keys                     []string
}

// sysTabDef is a tab with its cards. Spans has one entry per stack: how many of the three
// columns it takes (ui.SpanAll for a full-width stack).
type sysTabDef struct {
	Slug, Label string
	Spans       []int
	Cards       []sysCardDef
}

// sysCard is a card with the rows it shows.
type sysCard struct {
	sysCardDef
	Rows       []sysRow
	RestartAll bool // every setting of the card applies after a restart: one badge in the header
}

// sysTab is one tab of the page.
type sysTab struct {
	Label, Slug string
	Spans       []int
	Cards       []sysCard
}

// sysLayout is the approved tab order (maintainer, issue #275) with the cards of each tab.
var sysLayout = []sysTabDef{
	{"general", "General", []int{1, 1}, []sysCardDef{
		{ID: "logging", Title: "Logging & audit", Stack: 0, Keys: []string{"LOG_LEVEL", "AUDIT_RETENTION"}},
		{ID: "metrics", Title: "Prometheus metrics", Stack: 1, Keys: []string{"METRICS_ENABLED", "METRICS_TOKEN", "METRICS_DEVICE_LABELS"}},
	}},
	{"network", "Network & devices", []int{1, 1, 1}, []sysCardDef{
		{ID: "networks", Title: "Device networks", Note: "Where your devices live.", Stack: 0, Keys: []string{"DEVICE_NETWORKS"}},
		{ID: "health", Title: "Health checks", Note: "How often devices are checked.", Stack: 1, Keys: []string{"POLL_INTERVAL", "POLICY_LIVE_INTERVAL", "TELEPERIOD", "CR_WARN_PERCENT"}},
		{ID: "credentials", Title: "Default device credentials", Note: "Prefilled for new devices.", Stack: 2, Keys: []string{"DEVICE_DEFAULT_PASSWORD"}},
	}},
	{"mqtt", "MQTT", []int{2, 1}, []sysCardDef{
		{ID: "broker", Title: "Broker", Note: "Changes apply at once. An empty host turns MQTT off.", Stack: 0, Keys: []string{"MQTT_URL", "MQTT_USER", "MQTT_PASSWORD"}},
		{ID: "topics", Title: "Topics", Stack: 1, Keys: []string{"MQTT_FULLTOPIC"}},
	}},
	{"discovery", "Discovery", []int{1, 1}, []sysCardDef{
		{ID: "mqttdiscovery", Title: "MQTT discovery", Stack: 0, Keys: []string{"DISCOVERY_MQTT"}},
		{ID: "scan", Title: "Network scan", Stack: 1, Keys: []string{"DISCOVERY_SCAN", "DISCOVERY_SCAN_INTERVAL", "DISCOVERY_SCAN_CIDRS"}},
	}},
	{"backups", "Backups", []int{2, 1}, []sysCardDef{
		{ID: "storage", Title: "Storage", Note: "Read-only. Not part of the form.", Stack: 0, Phone: 1},
		{ID: "orphans", Title: "Backups of deleted devices", Stack: 0, Phone: 2},
		{ID: "database", Title: "TasmoFleet database", Note: "Download and delete act at once, outside the settings form. Admins only; every download and every scheduled snapshot is written to the audit log.", Stack: 0, Phone: 4},
		{ID: "schedule", Title: "Device backup schedule", Stack: 1, Phone: 3, Keys: []string{"BACKUP_INTERVAL", "BACKUP_KEEP", "BACKUP_MAX_AGE"}},
		{ID: "snapshots", Title: "Snapshot schedule", Stack: 1, Phone: 5, Keys: []string{"SNAPSHOT_INTERVAL", "SNAPSHOT_KEEP"}},
	}},
	{"notifications", "Notifications", []int{1, 2}, []sysCardDef{
		{ID: "notify-events", Title: "Events", Note: "What is sent. Both channels get the same events.", Stack: 0, Phone: 1,
			Keys: []string{"NOTIFY_UPDATE", "NOTIFY_UPDATE_WHEN", "NOTIFY_BACKUP", "NOTIFY_SNAPSHOT"}},
		{ID: "notify-message", Title: "Message", Note: "Read-only preview.", Stack: 0, Phone: 3},
		{ID: "notify-webhook", Title: "Webhook", Note: "Home Assistant, ntfy, Gotify, anything that takes a JSON POST.", Stack: 1, Phone: 2,
			Keys: []string{"NOTIFY_WEBHOOK", "NOTIFY_WEBHOOK_FORMAT", "NOTIFY_WEBHOOK_URL", "NOTIFY_WEBHOOK_AUTH", "NOTIFY_WEBHOOK_HEADER", "NOTIFY_WEBHOOK_SECRET"}},
		{ID: "notify-email", Title: "E-mail (SMTP)", Note: "SMTP with STARTTLS, TLS or none. No provider APIs in this version.", Stack: 1, Phone: 4,
			Keys: []string{"NOTIFY_SMTP", "NOTIFY_SMTP_HOST", "NOTIFY_SMTP_PORT", "NOTIFY_SMTP_SECURITY", "NOTIFY_SMTP_USER", "NOTIFY_SMTP_PASSWORD", "NOTIFY_SMTP_FROM", "NOTIFY_SMTP_TO"}},
	}},
	{"firmware", "Firmware & updates", []int{2, 1}, []sysCardDef{
		{ID: "source", Title: "Firmware source", Stack: 0, Keys: []string{"FIRMWARE_BASE_URL", "FIRMWARE_SOURCES", "FIRMWARE_REFRESH_INTERVAL", "FIRMWARE_KEEP_RELEASES"}},
		{ID: "crosscheck", Title: "Cross-check", Stack: 1, Keys: []string{"FIRMWARE_ALLOW_SINGLE_SOURCE"}},
		{ID: "ota", Title: "Updates (OTA)", Stack: 1, Keys: []string{"OTA_PARALLEL", "OTA_POWER_MAX_AGE", "OTA_RESTORE_OTAURL"}},
	}},
	{"signin", "Sign-in", []int{ui.SpanAll, 2, 1}, []sysCardDef{
		{ID: "mode", Title: "Sign-in mode", Stack: 0, StepUp: true, Keys: []string{"AUTH_MODE"}},
		{ID: "idp", Title: "Identity provider", Note: "Disabled while Local accounts is selected.", Stack: 1, Depends: "v.AUTH_MODE=oidc", StepUp: true,
			Keys: []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_SCOPES", "OIDC_NAME_CLAIM", "OIDC_DISPLAY_NAME"}},
		{ID: "roles", Title: "Roles and claims", Stack: 2, Depends: "v.AUTH_MODE=oidc", StepUp: true,
			Keys: []string{"ADMIN_GROUPS", "OPERATOR_GROUPS", "OIDC_GROUPS_CLAIM", "OIDC_USERNAME_CLAIM"}},
	}},
	{"advanced", "Advanced", []int{1}, []sysCardDef{
		{ID: "other", Title: "Other settings"},
	}},
}

// sysAdvanced is the index of the Advanced tab and its catch-all card.
const sysAdvanced, sysOtherCard = 8, 0

// sysGroupCard is where a new setting that no card names yet lands, by its sysconfig group, so it
// shows next to its relatives; a group without an entry goes to Advanced, "Other settings".
var sysGroupCard = map[sysconfig.Group]string{
	sysconfig.GroupLogging: "logging", sysconfig.GroupAudit: "logging", sysconfig.GroupMetrics: "metrics",
	sysconfig.GroupNetwork: "networks", sysconfig.GroupHealth: "health", sysconfig.GroupMQTT: "broker",
	sysconfig.GroupDiscovery: "scan", sysconfig.GroupBackup: "schedule", sysconfig.GroupSnapshot: "snapshots", sysconfig.GroupFirmware: "source",
	sysconfig.GroupOTA: "ota", sysconfig.GroupSignIn: "idp", sysconfig.GroupNotify: "notify-events",
}

// sysPlace is the tab and card of a setting: the card that names it, else the card of its group,
// else Advanced.
func sysPlace(key string) (tab, card int) {
	for ti, t := range sysLayout {
		for ci, c := range t.Cards {
			if slices.Contains(c.Keys, key) {
				return ti, ci
			}
		}
	}
	if f, ok := sysconfig.Lookup(key); ok {
		for ti, t := range sysLayout {
			for ci, c := range t.Cards {
				if c.ID == sysGroupCard[f.Group] {
					return ti, ci
				}
			}
		}
	}
	return sysAdvanced, sysOtherCard
}

// sysTabsOf fills the layout with the rows. A setting that depends on a switch (sysFieldCopy.Dep)
// hangs under that switch when both sit in the same card.
func sysTabsOf(rows map[string]sysRow) []sysTab {
	tabs := make([]sysTab, len(sysLayout))
	for ti, def := range sysLayout {
		tabs[ti] = sysTab{Label: def.Label, Slug: def.Slug, Spans: def.Spans, Cards: make([]sysCard, len(def.Cards))}
		for ci, cd := range def.Cards {
			tabs[ti].Cards[ci] = sysCard{sysCardDef: cd}
		}
	}
	keys := make([]string, 0, len(rows))
	for _, f := range sysconfig.Fields() {
		if _, ok := rows[f.Key]; ok {
			keys = append(keys, f.Key)
		}
	}
	placed := map[string]bool{}
	for ti, def := range sysLayout {
		for ci, cd := range def.Cards {
			c := &tabs[ti].Cards[ci]
			for _, k := range cd.Keys {
				if row, ok := rows[k]; ok {
					c.Rows = append(c.Rows, row)
					placed[k] = true
				}
			}
		}
	}
	for _, k := range keys {
		if !placed[k] {
			ti, ci := sysPlace(k)
			tabs[ti].Cards[ci].Rows = append(tabs[ti].Cards[ci].Rows, rows[k])
		}
	}
	for ti := range tabs {
		for ci := range tabs[ti].Cards {
			c := &tabs[ti].Cards[ci]
			c.Rows = sysNest(c.Rows)
			if c.RestartAll = sysRestartAll(c.Rows); c.RestartAll {
				for i := range c.Rows {
					c.Rows[i].InRestartCard = true
					for j := range c.Rows[i].Deps {
						c.Rows[i].Deps[j].InRestartCard = true
					}
				}
			}
		}
	}
	return tabs
}

// sysNest hangs each dependent row under the switch it names, when that switch is in the card. A
// row may hang under a row that itself hangs under a switch (the webhook header under the
// authentication choice under the webhook switch).
func sysNest(rows []sysRow) []sysRow {
	var out []sysRow
	for _, r := range rows {
		dep := sysFieldCopy(r.Key).Dep
		if dep != "" {
			if target := sysFindRow(out, dep); target != nil {
				target.Deps = append(target.Deps, r)
				continue
			}
		}
		out = append(out, r)
	}
	return out
}

// sysFindRow finds a row by key among rows and the rows hung under them.
func sysFindRow(rows []sysRow, key string) *sysRow {
	for i := range rows {
		if rows[i].Key == key {
			return &rows[i]
		}
		if t := sysFindRow(rows[i].Deps, key); t != nil {
			return t
		}
	}
	return nil
}

// sysRestartAll says that every editable setting of a card applies after a restart, so the card
// carries one badge instead of each field.
func sysRestartAll(rows []sysRow) bool {
	n := 0
	for _, r := range rows {
		for _, x := range append([]sysRow{r}, r.Deps...) {
			if x.Locked {
				continue
			}
			if x.Apply == sysconfig.Live {
				return false
			}
			n++
		}
	}
	return n > 0
}

// hasProblem is a row with an error or a stored secret that must be entered again: the page opens
// the tab it sits on.
func (r sysRow) hasProblem() bool {
	if r.Err != "" || r.NeedsReentry {
		return true
	}
	return slices.ContainsFunc(r.Deps, sysRow.hasProblem)
}

// sysCopy is what the page says about a setting: a short label and help (one line, at most 72
// characters), the longer text behind the info button, the width class and the switch it depends on.
type sysCopy struct {
	Label string // replaces the sysconfig label where the card already says what it is about
	Help  string
	Info  string
	Width string // full, half or xs
	Dep   string // key of the switch this setting only matters for
}

// sysCopies is the copy of every setting. Help is the one line under the field; Info is the rest.
var sysCopies = map[string]sysCopy{
	"NOTIFY_UPDATE":      {Help: "One message per job, not per device.", Width: "full"},
	"NOTIFY_UPDATE_WHEN": {Label: "Send when", Width: "full", Dep: "NOTIFY_UPDATE"},
	"NOTIFY_BACKUP":      {Help: "One message per run, listing the devices.", Width: "full", Info: "Counts the scheduled run only. A manual backup shows its error on screen. All devices that fail in one run make one message."},
	"NOTIFY_SNAPSHOT":    {Help: "The nightly SQLite snapshot.", Width: "full"},
	"NOTIFY_WEBHOOK":     {Help: "POST with a JSON body when an event above happens.", Width: "full"},
	"NOTIFY_WEBHOOK_FORMAT": {Help: "Same body; Home Assistant reads trigger.json.event.", Width: "full", Dep: "NOTIFY_WEBHOOK",
		Info: "Generic JSON and the Home Assistant preset send the same body. For Home Assistant use a webhook trigger and the address http://<host>:8123/api/webhook/<id>; the automation reads trigger.json.event, .result, .title and .text."},
	"NOTIFY_WEBHOOK_URL": {Help: "http or https. Local addresses are fine.", Width: "full", Dep: "NOTIFY_WEBHOOK", Info: "The address is stored as entered. Its path and query can be a secret (a Home Assistant webhook id): the audit log, the status line and the logs show only scheme, host and port."},
	"NOTIFY_WEBHOOK_AUTH": {Width: "full", Dep: "NOTIFY_WEBHOOK",
		Info: "None: a Home Assistant webhook id is its own secret. Secret header: a fixed header such as a bearer token, sent with every message."},
	"NOTIFY_WEBHOOK_HEADER": {Width: "half", Dep: "NOTIFY_WEBHOOK_AUTH"},
	"NOTIFY_WEBHOOK_SECRET": {Help: "Write-only. Empty keeps it, for the same address only.", Width: "half", Dep: "NOTIFY_WEBHOOK_AUTH",
		Info: "Sent only to the exact address it was entered for. Change the address (host, port, scheme, path or query) and it must be entered again; a save or a test without it sends none."},
	"NOTIFY_SMTP":          {Help: "One message per event.", Width: "full"},
	"NOTIFY_SMTP_HOST":     {Width: "grow", Dep: "NOTIFY_SMTP"},
	"NOTIFY_SMTP_PORT":     {Width: "port", Dep: "NOTIFY_SMTP"},
	"NOTIFY_SMTP_SECURITY": {Width: "tls", Dep: "NOTIFY_SMTP"},
	"NOTIFY_SMTP_USER":     {Help: "Empty if the server needs no login.", Width: "half", Dep: "NOTIFY_SMTP"},
	"NOTIFY_SMTP_PASSWORD": {Help: "Write-only. Empty keeps it, for the same server only.", Width: "half", Dep: "NOTIFY_SMTP",
		Info: "Sent only to the server, port, security and user it was entered for. Change one of them and it must be entered again; a save or a test without it sends none."},
	"NOTIFY_SMTP_FROM": {Width: "half", Dep: "NOTIFY_SMTP"},
	"NOTIFY_SMTP_TO":   {Help: "Several addresses separated by commas.", Width: "half", Dep: "NOTIFY_SMTP"},

	"LOG_LEVEL":       {Help: "Applies at once. Debug is verbose.", Width: "full"},
	"AUDIT_RETENTION": {Help: "At least 24h. 720h = 30 days.", Width: "xs", Info: "How long audit events and finished update transactions (with their logs) are kept. A running transaction is never removed. 8760h is 365 days."},

	"METRICS_ENABLED":       {Label: "Serve /metrics", Help: "Off: /metrics answers 404.", Width: "full"},
	"METRICS_TOKEN":         {Help: "Write-only. Empty: local scraper only.", Width: "half", Dep: "METRICS_ENABLED", Info: "Bearer token a remote Prometheus sends (Authorization: Bearer ...). Empty: only a scraper on this host, not behind a proxy, is served. Applies at once."},
	"METRICS_DEVICE_LABELS": {Help: "Off: metrics carry counts only.", Width: "full"},

	"DEVICE_NETWORKS":         {Help: "CIDRs, one per line or comma separated.", Width: "full", Info: "TasmoFleet never contacts an address outside these networks: no polling, no scan, no update."},
	"POLL_INTERVAL":           {Help: "Without MQTT. At least 1s.", Width: "xs"},
	"POLICY_LIVE_INTERVAL":    {Label: "Policy check read", Help: "0 = off.", Width: "xs", Info: "How often each device is read for the policy view (jittered, side-effect-free queries). 0 = no background reads; MQTT updates and the on-demand check stay."},
	"CR_WARN_PERCENT":         {Help: "Percent, 50 to 100.", Width: "xs", Info: "Warn on the device page when a device's settings text area (Status 2 CR) is fuller than this."},
	"TELEPERIOD":              {Label: "Telemetry period", Help: "Stale after 2× + 1 min.", Width: "xs", Info: "Expected telemetry period. A device counts as stale after twice this period plus a minute."},
	"DEVICE_DEFAULT_PASSWORD": {Help: "Write-only. Prefills new devices.", Width: "half", Info: "Prefilled as the web password when you add a device or accept a discovered one (user admin). Each device keeps its own copy; you can still change it per device."},

	"MQTT_URL":       {Label: "Host", Width: "full"},
	"MQTT_USER":      {Help: "Empty if the broker needs no login.", Width: "half"},
	"MQTT_PASSWORD":  {Help: "Write-only. Empty keeps it.", Width: "half"},
	"MQTT_FULLTOPIC": {Help: "Must contain %prefix% and %topic% once each.", Width: "full", Info: "Discovery listens on tasmota/discovery/+/config."},

	"DISCOVERY_MQTT":          {Help: "Needs MQTT. Finds are proposals.", Width: "full", Info: "A discovered device is never added on its own: an admin accepts it on the Discovered page."},
	"DISCOVERY_SCAN":          {Label: "Scheduled network scan", Help: "Probes the networks below on a schedule.", Width: "full"},
	"DISCOVERY_SCAN_INTERVAL": {Help: "At least 1h.", Width: "xs", Dep: "DISCOVERY_SCAN"},
	"DISCOVERY_SCAN_CIDRS":    {Help: "Inside the device networks, max /22 each.", Width: "full", Dep: "DISCOVERY_SCAN", Info: "Each CIDR must lie inside the device networks and hold at most 1024 addresses (/22)."},

	"BACKUP_INTERVAL":   {Label: "Interval", Help: "e.g. 24h.", Width: "xs"},
	"BACKUP_KEEP":       {Label: "Kept per device", Help: "Lowering it can delete restore points.", Width: "xs"},
	"SNAPSHOT_INTERVAL": {Label: "Interval", Help: "e.g. 24h.", Width: "xs"},
	"SNAPSHOT_KEEP":     {Label: "Snapshots kept", Help: "The oldest are deleted first.", Width: "xs", Dep: "SNAPSHOT_INTERVAL"},
	"BACKUP_MAX_AGE":    {Label: "Never prune younger than", Help: "e.g. 720h.", Width: "xs", Info: "Protects restore points from repeated manual runs: a backup younger than this is never pruned, even above the per-device count."},

	"FIRMWARE_BASE_URL":            {Help: "Plain http:// address devices fetch firmware from.", Width: "full", Info: "Devices download firmware from this address, e.g. the container address. Required for updates."},
	"FIRMWARE_SOURCES":             {Help: "Two sources = cross-check.", Width: "full"},
	"FIRMWARE_REFRESH_INTERVAL":    {Help: "0 = only at start.", Width: "xs", Info: "How often the upstream index is refreshed in the background, e.g. 6h."},
	"FIRMWARE_KEEP_RELEASES":       {Help: "0 = never delete.", Width: "xs", Info: "Cached firmware of releases older than the newest this many is deleted after an index refresh and once a day. Files a queued or running update needs and uploads stay."},
	"FIRMWARE_ALLOW_SINGLE_SOURCE": {Help: "Off is recommended.", Width: "full", Info: "Off: a firmware file must be offered by two sources with the same checksum. On: one source is enough. Shown in the warning colour while on."},

	"OTA_PARALLEL":       {Help: "Fleet-wide.", Width: "xs"},
	"OTA_POWER_MAX_AGE":  {Label: "Relay state max age", Help: "Older states stay off.", Width: "xs", Info: "A relay state read before an update may switch relays back on only while it is younger than this."},
	"OTA_RESTORE_OTAURL": {Help: "Device gets its previous update address back.", Width: "full"},

	"AUTH_MODE":           {Width: "full"},
	"OIDC_ISSUER":         {Help: "With the trailing slash.", Width: "full"},
	"OIDC_CLIENT_ID":      {Help: "Registered for TasmoFleet at your IdP.", Width: "full"},
	"OIDC_CLIENT_SECRET":  {Help: "Write-only.", Width: "half", Info: "Sent only to the issuer it was entered for. Change the issuer and the secret must be entered again."},
	"OIDC_SCOPES":         {Help: "Comma separated.", Width: "half"},
	"OIDC_NAME_CLAIM":     {Help: "Default: name.", Width: "half", Info: "Claim with the person's full name, shown instead of the user name. Users can override it on their Account page."},
	"OIDC_DISPLAY_NAME":   {Help: "“Sign in with <name>”.", Width: "half", Info: "Name of your identity provider on the login page. Default: Single sign-on."},
	"ADMIN_GROUPS":        {Help: "IdP groups, comma separated.", Width: "full"},
	"OPERATOR_GROUPS":     {Help: "IdP groups, comma separated.", Width: "full"},
	"OIDC_GROUPS_CLAIM":   {Help: "Default: groups.", Width: "full", Info: "Claim in the ID token that lists the user's groups."},
	"OIDC_USERNAME_CLAIM": {Help: "Default: preferred_username.", Width: "full", Info: "Claim shown as the user name."},
}

// sysFieldCopy is the copy of a setting; one without an entry (a new field) falls back to the
// sysconfig help, cut to its first sentence, and the half width.
func sysFieldCopy(key string) sysCopy {
	if c, ok := sysCopies[key]; ok {
		return c
	}
	f, _ := sysconfig.Lookup(key)
	help, _, _ := strings.Cut(f.Help, ". ")
	if len(help) > 72 {
		help = strings.TrimSpace(help[:72])
	}
	return sysCopy{Help: help, Width: "half", Info: f.Help}
}

// sysLabel is the label of a row: the card's own wording, else the sysconfig label.
func sysLabel(r sysRow) string {
	if l := sysFieldCopy(r.Key).Label; l != "" {
		return l
	}
	return r.Label
}

// sysScheduleOn says whether the backup schedule switch is on: the interval is not zero. An
// interval the server rejected stays on, so the error is shown next to the switch that caused it.
func sysScheduleOn(r sysRow) bool {
	if r.Err != "" {
		return true
	}
	if r.Posted == "0" || r.Posted == "" {
		return false
	}
	d, err := time.ParseDuration(r.Posted)
	return err != nil || d != 0
}

// sysChecked lists the choices of a list setting that are on.
func sysChecked(r sysRow) []string {
	return strings.Split(r.Posted, ",")
}

// sysCardBadges are the badges in a card's header: one Restart badge when every setting needs a
// restart, and the note that saving asks to confirm.
func sysCardBadges(c sysCard) []ui.SettingsBadge {
	var out []ui.SettingsBadge
	if c.RestartAll {
		out = append(out, ui.SettingsBadge{Tone: ui.ToneWarn, Label: "Restart"})
	}
	if c.StepUp {
		out = append(out, ui.SettingsBadge{Tone: ui.ToneAccent, Label: "Asks you to confirm"})
	}
	return out
}

// sysDependsOn is the control a dependent setting follows: the switch of a schedule (the interval
// field itself is no switch), else the setting's own on/off control.
func sysDependsOn(key string) string {
	if slices.Contains(scheduleIntervals, key) {
		return "sw." + key
	}
	if key == "NOTIFY_WEBHOOK_AUTH" {
		return "v." + key + "=" + config.NotifyAuthHeader // a choice, not a switch: the header fields follow "Secret header"
	}
	return "v." + key
}

// snapDeleteText is the dialog text of "Delete" on a stored snapshot.
func snapDeleteText(name string) string {
	return "The snapshot " + name + " is deleted from the data directory. The running database and the other snapshots are not touched."
}
