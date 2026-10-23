// Package sysconfig is the runtime configuration behind the system settings
// page. Configuration has two tiers:
//
//   - Bootstrap and security settings (authentication, listeners, TLS, keys,
//     trusted proxies, the data directory, ...) come from the environment or
//     the env file only. The page lists them read-only; no web session can
//     change them, so a stolen session can never weaken authentication.
//   - Operational settings (the Fields below) may be edited by an admin. A
//     value is stored in SQLite and applies unless the operator set the same
//     key in the environment, which locks it.
//
// Validation is the validation of package config (the same parsers and the
// same cross-field checks as at start), so the UI and the environment cannot
// disagree. Changes that widen the network reach or touch credentials need a
// step-up. Every change is audited with old and new value, secrets redacted.
package sysconfig

import (
	"slices"

	"github.com/nofuturekid/tasmofleet/internal/config"
)

// Mode says how a change takes effect.
type Mode int

// Apply modes.
const (
	// Live: subscribers pick the new value up without a restart.
	Live Mode = iota
	// Restart: the running component read the value at start; the page shows
	// "restart required" until the process runs with the stored value.
	Restart
)

// StepUpRule says when a change needs a fresh confirmation of the admin.
type StepUpRule int

// Step-up rules.
const (
	StepUpNever  StepUpRule = iota
	StepUpAlways            // any change: the settings that decide who signs in or with which rights
)

// Group is the section of the page a field is shown in.
type Group string

// Groups, in page order.
const (
	GroupNetwork   Group = "Device networks"
	GroupFirmware  Group = "Firmware"
	GroupOTA       Group = "Updates (OTA)"
	GroupBackup    Group = "Backups"
	GroupHealth    Group = "Health"
	GroupMQTT      Group = "MQTT broker"
	GroupDiscovery Group = "Discovery"
	GroupAudit     Group = "Audit"
	GroupMetrics   Group = "Metrics"
	GroupLogging   Group = "Logging"
	GroupSignIn    Group = "Single sign-on (OIDC)"
)

// Field is one operational setting that the page may edit.
type Field struct {
	Key    string
	Group  Group
	Label  string
	Help   string
	Secret bool // write-only: never shown, empty input keeps the stored value
	Apply  Mode
	StepUp StepUpRule
	// Choices, if set, are the only valid list entries (checkboxes).
	Choices []string
	// Enum, if set, lists the only valid values (shown as a select).
	Enum []string
	// Multiline marks lists shown in a textarea.
	Multiline bool
	// Bool marks true/false settings (shown as a select).
	Bool bool
}

var fields = []Field{
	{Key: "DEVICE_NETWORKS", Group: GroupNetwork, Label: "Device networks", Apply: Live, Multiline: true,
		Help: "CIDRs the devices live in, one per line or comma separated. Nothing outside is ever contacted. "},
	{Key: "DEVICE_DEFAULT_PASSWORD", Group: GroupNetwork, Label: "Default device password", Apply: Live, Secret: true,
		Help: "Write-only. Prefills the web password when a device is added or a discovered device is accepted (user admin); each device keeps its own copy."},

	{Key: "FIRMWARE_BASE_URL", Group: GroupFirmware, Label: "Firmware base URL", Apply: Restart,
		Help: "Plain http:// URL devices use to fetch firmware, e.g. http://172.31.12.61 (the container address). Required for updates; needs a restart."},
	{Key: "FIRMWARE_SOURCES", Group: GroupFirmware, Label: "Upstream sources", Apply: Restart, Choices: config.KnownFirmwareSources(),
		Help: "Where firmware is fetched from. Two sources allow the cross-check."},
	{Key: "FIRMWARE_ALLOW_SINGLE_SOURCE", Bool: true, Group: GroupFirmware, Label: "Accept single-source files", Apply: Restart,
		Help: "Off (recommended): a file must be offered by two sources (cross-check)."},
	{Key: "FIRMWARE_REFRESH_INTERVAL", Group: GroupFirmware, Label: "Index refresh interval", Apply: Restart,
		Help: "How often the upstream index is refreshed in the background, e.g. 6h. 0 = only at start."},

	{Key: "OTA_PARALLEL", Group: GroupOTA, Label: "Updates in parallel", Apply: Live, Help: "Fleet-wide number of devices updated at the same time."},
	{Key: "OTA_RESTORE_OTAURL", Bool: true, Group: GroupOTA, Label: "Restore original OtaUrl", Apply: Live,
		Help: "After an update the device gets its own previous update address back, not the address of this server."},
	{Key: "OTA_POWER_MAX_AGE", Group: GroupOTA, Label: "Relay state maximum age", Apply: Live,
		Help: "A relay state read before an update may switch relays back on only while it is younger than this."},

	{Key: "BACKUP_KEEP", Group: GroupBackup, Label: "Backups kept per device", Apply: Live,
		Help: "Lowering it can delete restore points."},
	{Key: "BACKUP_MAX_AGE", Group: GroupBackup, Label: "Never prune backups younger than", Apply: Live,
		Help: "Protects restore points from repeated manual runs, e.g. 720h."},
	{Key: "BACKUP_INTERVAL", Group: GroupBackup, Label: "Scheduled backup interval", Apply: Live,
		Help: "How often all devices are backed up, e.g. 24h. 0 = manual only."},

	{Key: "POLL_INTERVAL", Group: GroupHealth, Label: "HTTP poll interval", Apply: Live, Help: "Health check by HTTP for devices without MQTT, at least 1s."},
	{Key: "POLICY_LIVE_INTERVAL", Group: GroupHealth, Label: "Policy check read interval", Apply: Live,
		Help: "How often each device is read for the policy view (jittered, side-effect-free queries), e.g. 1h. 0 = no background reads; MQTT updates and the on-demand check stay."},
	{Key: "TELEPERIOD", Group: GroupHealth, Label: "Expected telemetry period", Apply: Live, Help: "A device is stale after twice this period plus a minute."},

	{Key: "MQTT_URL", Group: GroupMQTT, Label: "Broker URL", Apply: Live,
		Help: "tcp://host:1883 or ssl://host:8883 (TLS). Empty turns MQTT off; changes apply at once. Put the user name and password in the fields below, not in the URL."},
	{Key: "MQTT_USER", Group: GroupMQTT, Label: "User", Apply: Live, Help: "Leave empty if the broker needs no login."},
	{Key: "MQTT_PASSWORD", Group: GroupMQTT, Label: "Password", Apply: Live, Secret: true,
		Help: "Write-only. Leave empty to keep the stored password."},
	{Key: "MQTT_FULLTOPIC", Group: GroupMQTT, Label: "Full topic pattern", Apply: Live, Help: "Must contain %prefix% and %topic% once each."},

	{Key: "DISCOVERY_MQTT", Bool: true, Group: GroupDiscovery, Label: "Discover devices from MQTT", Apply: Live, Help: "Needs MQTT. Finds are proposals; an admin accepts them."},
	{Key: "DISCOVERY_SCAN", Bool: true, Group: GroupDiscovery, Label: "Scheduled network scan", Apply: Live, Help: "Off by default. Probes the CIDRs below on a schedule."},
	{Key: "DISCOVERY_SCAN_INTERVAL", Group: GroupDiscovery, Label: "Scan interval", Apply: Live, Help: "At least 1h."},
	{Key: "DISCOVERY_SCAN_CIDRS", Group: GroupDiscovery, Label: "Scan networks", Apply: Live, Multiline: true,
		Help: "CIDRs inside the device networks, at most 1024 addresses (/22) each."},

	{Key: "AUTH_MODE", Group: GroupSignIn, Label: "Sign-in mode", Apply: Restart, StepUp: StepUpAlways, Enum: []string{"local", "oidc"},
		Help: "local: accounts of this server. oidc: single sign-on through your identity provider; local accounts always remain as break-glass access. Needs PUBLIC_URL in the container configuration."},
	{Key: "OIDC_ISSUER", Group: GroupSignIn, Label: "Issuer URL", Apply: Restart, StepUp: StepUpAlways, Help: "e.g. https://auth.example/application/o/tasmofleet/ (with the trailing slash)."},
	{Key: "OIDC_CLIENT_ID", Group: GroupSignIn, Label: "Client ID", Apply: Restart, StepUp: StepUpAlways, Help: "The client ID registered for tasmofleet at your identity provider."},
	{Key: "OIDC_CLIENT_SECRET", Group: GroupSignIn, Label: "Client secret", Apply: Restart, StepUp: StepUpAlways, Secret: true, Help: "Write-only. Sent only to the issuer it was entered for; change the issuer and it must be entered again."},
	{Key: "OIDC_SCOPES", Group: GroupSignIn, Label: "Scopes", Apply: Restart, Help: "Comma separated, default openid,profile,email."},
	{Key: "OIDC_GROUPS_CLAIM", Group: GroupSignIn, Label: "Groups claim", Apply: Restart, StepUp: StepUpAlways, Help: "Claim in the ID token that lists the user's groups. Default: groups."},
	{Key: "OIDC_USERNAME_CLAIM", Group: GroupSignIn, Label: "User name claim", Apply: Restart, StepUp: StepUpAlways, Help: "Claim shown as the user name. Default: preferred_username."},
	{Key: "OIDC_DISPLAY_NAME", Group: GroupSignIn, Label: "Button name", Apply: Restart,
		Help: "Name of your identity provider on the login page: the button reads \"Sign in with <name>\". Default: Single sign-on."},
	{Key: "ADMIN_GROUPS", Group: GroupSignIn, Label: "Admin groups", Apply: Restart, StepUp: StepUpAlways, Help: "Comma separated IdP groups that get the admin role."},
	{Key: "OPERATOR_GROUPS", Group: GroupSignIn, Label: "Operator groups", Apply: Restart, StepUp: StepUpAlways, Help: "Comma separated IdP groups that get the operator role."},

	{Key: "METRICS_ENABLED", Group: GroupMetrics, Label: "Serve /metrics", Bool: true, Apply: Live, Help: "Off: /metrics answers 404. On: token and local-scraper rules apply."},
	{Key: "METRICS_DEVICE_LABELS", Group: GroupMetrics, Label: "Device names in metric labels", Bool: true, Apply: Live, Help: "Off by default: metrics then carry counts only."},
	{Key: "AUDIT_RETENTION", Group: GroupAudit, Label: "Audit retention", Apply: Live,
		Help: "How long audit events and finished update transactions (with their logs) are kept, at least 24h; a running transaction is never removed. Hours: 720h is 30 days, 8760h is 365 days."},
	{Key: "LOG_LEVEL", Group: GroupLogging, Label: "Log level", Apply: Live, Enum: config.LogLevels(),
		Help: "How much the server logs. debug is verbose; changes apply at once."},
}

// Fields returns the editable fields in page order.
func Fields() []Field { return slices.Clone(fields) }

// Lookup finds an editable field by key.
func Lookup(key string) (Field, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}
