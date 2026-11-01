// Package config loads the configuration from environment variables
// (TASMOFLEET_*) and an optional file in the same KEY=value format
// (TASMOFLEET_CONFIG_FILE, default /data/tasmofleet.env). Environment wins
// over the file. Every key is listed in the spec, section "Configuration";
// the `env` tags below are the authoritative list.
//
// Secrets may also be given as *_FILE variants (e.g.
// TASMOFLEET_OIDC_CLIENT_SECRET_FILE=/run/secrets/oidc) for Docker secrets.
package config

import (
	"net/netip"
	"time"
)

// Config is the complete runtime configuration. Fields tagged ui:"true" are
// operational settings edited on the system settings page and stored in the
// database; the environment is no longer a source for them (see Legacy).
// Fields tagged ui:"lock" are the sign-in settings: editable on the page unless
// the environment sets them (then the environment wins and the field is locked).
// All other fields are bootstrap and security settings: environment or env file
// only, shown read-only in the UI.
type Config struct {
	// Legacy holds the values of operational settings that were still given as
	// environment variables or in the env file (key -> value, secrets
	// included; never log it). They are no longer configuration: the system
	// settings page owns those keys. sysconfig imports them once into the
	// database and clears the map. POLICY_RULES (inline JSON or a file path) is
	// kept here the same way. It has no env tag: Load fills it.
	Legacy map[string]string
	// Locked lists the sign-in settings (ui:"lock") the operator set in the
	// environment or the env file: they apply as before and the settings page
	// shows them read-only. Sign-in settings left unset can be edited there.
	Locked map[string]bool
	// TLSSelfSigned is true when the UI listener serves a self-signed certificate
	// (persisted under <DATA_DIR>/tls). Load decides: that is the mode when no
	// certificate, no trusted proxy and no ALLOW_INSECURE_HTTP is configured.
	// It has no env tag (TASMOFLEET_TLS_SELF_SIGNED was removed in 0.17).
	TLSSelfSigned bool
	// AutoTLS is true when the server serves a self-signed certificate only
	// because nothing else was configured. It has no env tag: Load decides.
	AutoTLS bool
	// HAProfile is true inside a Home Assistant app (SUPERVISOR_TOKEN is set): Load then reads
	// /data/options.json and applies the app defaults (haprofile.go). It has no env tag.
	HAProfile bool
	// Warnings are start-up notices Load collected (for example removed
	// settings that are still set). They have no env tag; the caller logs them.
	Warnings []string

	DataDir   string `env:"DATA_DIR" default:"/data"`
	Listen    string `env:"LISTEN" default:":8080"`        // UI + API; one port in every mode, containers never set it
	FWListen  string `env:"FIRMWARE_LISTEN" default:":80"` // device-facing firmware server
	FWBaseURL string `env:"FIRMWARE_BASE_URL" ui:"true"`   // URL devices use, e.g. http://192.168.1.10 (required for OTA)
	PublicURL string `env:"PUBLIC_URL"`                    // e.g. https://tasmofleet.example.com (required for oidc)
	BasePath  string `env:"BASE_PATH"`
	// LogLevel is debug | info | warn | error. A sign-in style setting: the
	// environment locks it, otherwise the settings page changes it live.
	LogLevel  string `env:"LOG_LEVEL" ui:"lock" default:"info"`
	LogFormat string `env:"LOG_FORMAT" default:"json"` // json | text

	TrustedProxies []netip.Prefix `env:"TRUSTED_PROXIES"`           // comma separated CIDRs
	DeviceNetworks []netip.Prefix `env:"DEVICE_NETWORKS" ui:"true"` // CIDRs devices may live in; required

	// AuthMode is the primary login method: oidc | header | local. Local
	// accounts (LocalLogin) can be combined with oidc and header.
	AuthMode string `env:"AUTH_MODE" default:"local" ui:"lock"`

	OIDCIssuer        string        `env:"OIDC_ISSUER" ui:"lock"` // e.g. https://auth.example.com/application/o/tasmofleet/
	OIDCClientID      string        `env:"OIDC_CLIENT_ID" ui:"lock"`
	OIDCClientSecret  string        `env:"OIDC_CLIENT_SECRET" ui:"lock" secret:"true"`
	OIDCScopes        []string      `env:"OIDC_SCOPES" ui:"lock" default:"openid,profile,email"` // Authentik puts "groups" into the profile scope
	OIDCGroupsClaim   string        `env:"OIDC_GROUPS_CLAIM" ui:"lock" default:"groups"`
	OIDCUsernameClaim string        `env:"OIDC_USERNAME_CLAIM" ui:"lock" default:"preferred_username"`
	OIDCNameClaim     string        `env:"OIDC_NAME_CLAIM" ui:"lock" default:"name"`             // the person's full name, shown instead of the user name (#283)
	OIDCDisplayName   string        `env:"OIDC_DISPLAY_NAME" ui:"lock" default:"Single sign-on"` // login button: "Sign in with <name>"
	OIDCPostLogoutURL string        `env:"OIDC_POST_LOGOUT_URL" ui:"lock"`                       // default PUBLIC_URL + base path
	HeaderUser        string        `env:"HEADER_USER" default:"X-authentik-username"`
	HeaderName        string        `env:"HEADER_NAME" default:"X-authentik-name"`
	HeaderEmail       string        `env:"HEADER_EMAIL" default:"X-authentik-email"`
	HeaderGroups      string        `env:"HEADER_GROUPS" default:"X-authentik-groups"`
	HeaderGroupsSep   string        `env:"HEADER_GROUPS_SEPARATOR" default:"|"`
	HeaderLogoutURL   string        `env:"HEADER_LOGOUT_URL"` // e.g. outpost sign-out URL
	AdminGroups       []string      `env:"ADMIN_GROUPS" ui:"lock"`
	OperatorGroups    []string      `env:"OPERATOR_GROUPS" ui:"lock"`
	LocalLogin        bool          `env:"LOCAL_LOGIN" default:"true"` // local accounts on the login page (always on in AUTH_MODE=local)
	SessionIdle       time.Duration `env:"SESSION_IDLE" default:"2h"`
	SessionMax        time.Duration `env:"SESSION_MAX" default:"12h"`
	OIDCRecheck       time.Duration `env:"OIDC_RECHECK" default:"15m"` // refresh token + group re-evaluation interval
	// InitialAdminPassword seeds the first local admin "admin" when no local
	// user exists (must be changed at first login). Empty: a random password
	// is generated and logged once.
	InitialAdminPassword string `env:"INITIAL_ADMIN_PASSWORD" secret:"true"`

	// TLS without a reverse proxy. Exactly one of: cert+key files, a
	// self-signed certificate (TLSSelfSigned, decided by Load), or plain HTTP,
	// which is only served behind TRUSTED_PROXIES or with AllowInsecureHTTP.
	TLSCertFile string `env:"TLS_CERT_FILE"`
	TLSKeyFile  string `env:"TLS_KEY_FILE"`
	// TLSSANs are extra DNS names/IPs for the self-signed certificate
	// (localhost, 127.0.0.1, ::1 and the hostname are always included).
	TLSSANs           []string `env:"TLS_SANS"`
	AllowInsecureHTTP bool     `env:"ALLOW_INSECURE_HTTP" default:"false"`

	MQTTURL       string `env:"MQTT_URL" ui:"true"` // tcp://192.168.1.5:1883; empty disables MQTT
	MQTTUser      string `env:"MQTT_USER" ui:"true"`
	MQTTPassword  string `env:"MQTT_PASSWORD" ui:"true" secret:"true"`
	MQTTFullTopic string `env:"MQTT_FULLTOPIC" ui:"true" default:"%prefix%/%topic%/"`

	// DeviceDefaultPassword is the fleet's usual Tasmota web password. It only
	// prefills the forms that add a device (manual add, accepting a proposal);
	// each device stores its own copy. Write-only on the settings page.
	DeviceDefaultPassword string `env:"DEVICE_DEFAULT_PASSWORD" ui:"true" secret:"true"`

	TelePeriod   time.Duration `env:"TELEPERIOD" ui:"true" default:"300s"`
	PollInterval time.Duration `env:"POLL_INTERVAL" ui:"true" default:"5m"`

	OTAParallel   int  `env:"OTA_PARALLEL" ui:"true" default:"2"`
	OTARestoreURL bool `env:"OTA_RESTORE_OTAURL" ui:"true" default:"true"`
	// OTAPowerMaxAge is how old the relay state read before an update may be
	// when the restore would switch relays back on; older, nothing is switched.
	OTAPowerMaxAge time.Duration `env:"OTA_POWER_MAX_AGE" ui:"true" default:"20m"`
	FWSources      []string      `env:"FIRMWARE_SOURCES" ui:"true" default:"github,ota.tasmota.com"`
	// FWRefreshInterval is how often the upstream index is refreshed in the
	// background (after the refresh at start); 0 = only at start.
	FWRefreshInterval time.Duration `env:"FIRMWARE_REFRESH_INTERVAL" ui:"true" default:"6h"`
	// FWKeepReleases: after a refresh and once a day the cache deletes the files of releases
	// older than the newest this many cached ones, except files a queued or running update needs
	// and uploads; 0 = never prune.
	FWKeepReleases int `env:"FIRMWARE_KEEP_RELEASES" ui:"true" default:"3"`
	// FWAllowSingleSource accepts files that only one configured source
	// offers (no cross-check possible); default refuses them.
	FWAllowSingleSource bool `env:"FIRMWARE_ALLOW_SINGLE_SOURCE" ui:"true" default:"false"`
	// FWUpstreamOverride replaces all sources with one upstream at this URL
	// (tests and dev only; the host allowlist then is that URL's host).
	FWUpstreamOverride string `env:"FIRMWARE_UPSTREAM_OVERRIDE"`

	// CRWarnPercent: the Identity card warns when the settings text area of a device (StatusFWR.CR)
	// is fuller than this many percent. 50..100.
	CRWarnPercent int `env:"CR_WARN_PERCENT" ui:"true" default:"90"`

	BackupKeep int `env:"BACKUP_KEEP" ui:"true" default:"10"`
	// BackupMaxAge: backups younger than this are never pruned, however many
	// newer ones exist (so repeated manual runs cannot evict the restore points).
	BackupMaxAge   time.Duration `env:"BACKUP_MAX_AGE" ui:"true" default:"720h"`
	BackupSchedule time.Duration `env:"BACKUP_INTERVAL" ui:"true" default:"0s"`              // 0 = manual only
	DecoderPath    string        `env:"DECODER_PATH" default:"/usr/local/bin/decode-config"` // empty disables

	// SnapshotSchedule is how often TasmoFleet writes a consistent copy of its own database to
	// <DATA_DIR>/snapshots (0 = off); SnapshotKeep is how many copies stay (the oldest go first).
	SnapshotSchedule time.Duration `env:"SNAPSHOT_INTERVAL" ui:"true" default:"0s"`
	SnapshotKeep     int           `env:"SNAPSHOT_KEEP" ui:"true" default:"7"`

	// Discovery. MQTT discovery listens to Tasmota discovery messages (needs MQTT).
	// The scheduled scan probes DiscoveryScanCIDRs (inside DEVICE_NETWORKS) every
	// DiscoveryScanInterval; it is off by default.
	DiscoveryMQTT         bool           `env:"DISCOVERY_MQTT" ui:"true" default:"true"`
	DiscoveryScan         bool           `env:"DISCOVERY_SCAN" ui:"true" default:"false"`
	DiscoveryScanInterval time.Duration  `env:"DISCOVERY_SCAN_INTERVAL" ui:"true" default:"24h"`
	DiscoveryScanCIDRs    []netip.Prefix `env:"DISCOVERY_SCAN_CIDRS" ui:"true"`

	// PolicyLiveInterval is how often each device is read for the policy view
	// (jittered, side-effect-free queries only). 0 turns the background reads
	// off; MQTT updates and the on-demand check stay.
	PolicyLiveInterval time.Duration `env:"POLICY_LIVE_INTERVAL" ui:"true" default:"1h"`

	// MetricsToken is the bearer token for /metrics. Set on the settings page, or
	// fixed by the environment (the variable then locks the field). Empty: /metrics
	// answers only loopback scrapers that did not come through a proxy.
	MetricsToken string `env:"METRICS_TOKEN" ui:"lock" secret:"true"`
	// MetricsEnabled is the master switch for /metrics: off, it answers 404.
	MetricsEnabled bool `env:"METRICS_ENABLED" ui:"true" default:"true"`
	// MetricsDeviceLabels puts device names into metric labels (off by default).
	MetricsDeviceLabels bool          `env:"METRICS_DEVICE_LABELS" ui:"true" default:"false"`
	AuditKeep           time.Duration `env:"AUDIT_RETENTION" ui:"true" default:"8760h"`

	// Notifications (#249): which events are sent, and the two channels they go to. Both channels
	// are off until switched on. The two secrets are write-only on the settings page.
	NotifyUpdate        bool   `env:"NOTIFY_UPDATE" ui:"true" default:"true"`
	NotifyUpdateWhen    string `env:"NOTIFY_UPDATE_WHEN" ui:"true" default:"any"` // any | problems
	NotifyBackup        bool   `env:"NOTIFY_BACKUP" ui:"true" default:"true"`
	NotifySnapshot      bool   `env:"NOTIFY_SNAPSHOT" ui:"true" default:"true"`
	NotifyWebhook       bool   `env:"NOTIFY_WEBHOOK" ui:"true" default:"false"`
	NotifyWebhookFormat string `env:"NOTIFY_WEBHOOK_FORMAT" ui:"true" default:"json"` // json | homeassistant
	NotifyWebhookURL    string `env:"NOTIFY_WEBHOOK_URL" ui:"true"`
	NotifyWebhookAuth   string `env:"NOTIFY_WEBHOOK_AUTH" ui:"true" default:"none"` // none | header
	NotifyWebhookHeader string `env:"NOTIFY_WEBHOOK_HEADER" ui:"true" default:"Authorization"`
	NotifyWebhookSecret string `env:"NOTIFY_WEBHOOK_SECRET" ui:"true" secret:"true"`
	NotifySMTP          bool   `env:"NOTIFY_SMTP" ui:"true" default:"false"`
	NotifySMTPHost      string `env:"NOTIFY_SMTP_HOST" ui:"true"`
	NotifySMTPPort      int    `env:"NOTIFY_SMTP_PORT" ui:"true" default:"587"`
	NotifySMTPSecurity  string `env:"NOTIFY_SMTP_SECURITY" ui:"true" default:"starttls"` // starttls | tls | none
	NotifySMTPUser      string `env:"NOTIFY_SMTP_USER" ui:"true"`
	NotifySMTPPassword  string `env:"NOTIFY_SMTP_PASSWORD" ui:"true" secret:"true"`
	NotifySMTPFrom      string `env:"NOTIFY_SMTP_FROM" ui:"true"`
	NotifySMTPTo        string `env:"NOTIFY_SMTP_TO" ui:"true"` // comma separated addresses
}
