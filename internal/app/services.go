package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/events"
	"github.com/nofuturekid/tasmofleet/internal/inventory"
	"github.com/nofuturekid/tasmofleet/internal/metrics"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// Services are the pieces shared by the server and the CLI: storage, audit,
// the device client, inventory and the prober.
type Services struct {
	Cfg     config.Config // effective: environment, then stored settings
	Sys     *sysconfig.Service
	Log     *slog.Logger
	Store   *store.SQLite
	Audit   *audit.StoreRecorder
	Hub     *events.Hub
	Metrics *metrics.Registry
	Std     *metrics.Standard
	// Client is the transport router (transport.Router): commands, settings, backups and OTA go
	// through it and reach a device over HTTP or MQTT. HTTP is the raw client with the router's
	// failure memory attached: what measures or verifies HTTP (the health poll, the prober, the MAC
	// verifier, the discovery scan) uses it, because they must not be answered over MQTT.
	Client    tasmota.Client
	HTTP      tasmota.Client
	Router    *transport.Router
	Inventory *inventory.Manager
	Prober    *Prober
	Networks  *tasmota.NetworkSet // DEVICE_NETWORKS in force; follows the settings page live
	Broker    *liveBroker         // matches devices against the configured MQTT broker; follows the settings page
}

// NewLogger builds the process logger from LOG_LEVEL and LOG_FORMAT. The
// level is a shared slog.LevelVar, returned with the logger, so the settings
// page can change it while the process runs (FollowLogLevel).
func NewLogger(cfg config.Config) (*slog.Logger, *slog.LevelVar) {
	level := new(slog.LevelVar)
	SetLogLevel(level, cfg.LogLevel)
	opts := &slog.HandlerOptions{Level: level, ReplaceAttr: logTimeMillis}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), level
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts)), level
}

// logTimeFormat is the log timestamp: milliseconds are enough to read a log,
// slog's default has nanoseconds.
const logTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// logTimeMillis is the slog ReplaceAttr that shortens the top-level time attribute.
func logTimeMillis(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.StringValue(a.Value.Time().Format(logTimeFormat))
	}
	return a
}

// SetLogLevel applies a LOG_LEVEL value; an unknown one means info.
func SetLogLevel(level *slog.LevelVar, name string) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(name)); err != nil {
		l = slog.LevelInfo
	}
	level.Set(l)
}

// FollowLogLevel applies the effective LOG_LEVEL now and after every change on
// the settings page, without a restart (N17).
func FollowLogLevel(sys *sysconfig.Service, level *slog.LevelVar, log *slog.Logger) {
	SetLogLevel(level, sys.Snapshot().LogLevel)
	sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "LOG_LEVEL") {
			SetLogLevel(level, e.New.LogLevel)
			log.Info("log level changed", "level", e.New.LogLevel)
		}
	})
}

// checkDataDirWritable fails with an actionable message when the process cannot
// write to dir. Docker creates a missing bind-mount source as root, so the
// non-root image user hits this on first start; without the check the first
// write (the secretbox key) fails with an opaque error (#33).
func checkDataDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".writecheck-*")
	if err != nil {
		return fmt.Errorf("data dir %s is not writable by uid %d: see README (bind mounts): %w", dir, os.Getuid(), err)
	}
	name := f.Name()
	// Best-effort cleanup of a probe file that only proves writability.
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// OpenServices opens the database and builds the services that do not need
// network listeners. The caller must Close it. It never imports legacy
// environment settings: that happens once, in Run, so a CLI command cannot
// consume the import with the wrong environment.
func OpenServices(cfg config.Config, log *slog.Logger, version string) (*Services, error) {
	return openServices(cfg, log, version, false)
}

func openServices(cfg config.Config, log *slog.Logger, version string, importLegacy bool) (*Services, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	if err := checkDataDirWritable(cfg.DataDir); err != nil {
		return nil, err
	}
	box, err := secretbox.LoadOrCreate(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "tasmofleet.db"), box)
	if err != nil {
		return nil, err
	}
	reg := metrics.NewRegistry()
	std := metrics.NewStandard(reg, version, cfg.DecoderPath != "")
	rec := audit.New(st, log, func() { std.AuditErrors.With().Inc() })
	// The stored settings (system settings page) apply to every key the
	// operator did not set; from here on cfg is the effective configuration.
	sys, err := sysconfig.New(context.Background(), sysconfig.Options{
		Base: cfg, Store: st, Audit: rec, Log: log, SkipLegacyImport: !importLegacy,
		TestSSO: func(ctx context.Context, issuer string) (string, error) {
			return auth.CheckIssuer(ctx, issuer, brokerAddrAllowed)
		},
		TestMQTT: func(ctx context.Context, p sysconfig.MQTTParams) error {
			return mqttbus.Probe(ctx, mqttbus.Options{BrokerURL: p.URL, Username: p.User, Password: p.Password, AllowAddr: brokerAddrAllowed})
		},
	})
	if err != nil {
		_ = st.Close() // the settings error is the one to report
		return nil, err
	}
	cfg = sys.Snapshot()
	hub := events.NewHub(64)
	nets := tasmota.NewNetworkSet(cfg.DeviceNetworks)
	sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "DEVICE_NETWORKS") {
			nets.Set(e.New.DeviceNetworks)
			log.Info("device networks changed", "networks", len(e.New.DeviceNetworks))
		}
	})
	router := transport.NewRouter(transport.Options{
		HTTP: tasmota.NewHTTPClient(tasmota.Options{Networks: nets, Logger: log}), Devices: st, Log: log,
	})
	client := router.HTTPOnly()
	inv, err := inventory.New(inventory.Options{
		Store: st, Audit: rec, Events: hub, NetworkSet: nets,
		Verifier: macVerifier{client: client},
	})
	if err != nil {
		_ = st.Close() // the constructor error is the one to report
		return nil, err
	}
	broker := &liveBroker{}
	broker.set(cfg.MQTTURL)
	sys.Subscribe(func(e sysconfig.Event) {
		if slices.Contains(e.Keys, "MQTT_URL") {
			broker.set(e.New.MQTTURL)
		}
	})
	return &Services{
		Broker: broker, Cfg: cfg, Sys: sys, Networks: nets, Log: log, Store: st, Audit: rec, Hub: hub, Metrics: reg, Std: std, Client: router, HTTP: client, Router: router, Inventory: inv,
		Prober: &Prober{Client: client, Devices: st, Broker: broker, Hub: hub, Audit: rec},
	}, nil
}

// Close releases the database.
func (s *Services) Close() error { return s.Store.Close() }

// describeConfig lists the effective configuration for the About page.
// Fields tagged secret:"true" are masked.
func describeConfig(cfg config.Config) []web.KV {
	var out []web.KV
	rv := reflectValue(cfg)
	for _, f := range rv {
		v := f.value
		if f.secret {
			v = ""
			if f.nonEmpty {
				v = "***"
			}
		}
		out = append(out, web.KV{Key: config.Prefix + f.key, Value: redactURLs(strings.TrimSpace(v))})
	}
	return out
}

// redactURLs drops the userinfo of every URL-looking value (MQTT_URL may be
// tcp://user:password@host:1883) and any other secret-bearing URL part.
func redactURLs(v string) string {
	if !strings.Contains(v, "://") {
		return v
	}
	u, err := url.Parse(v)
	if err != nil {
		return "***" // cannot prove it is free of credentials
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	u.RawQuery = ""
	return u.String()
}

// liveBroker is the broker matcher of the current MQTT_URL.
type liveBroker struct {
	p atomic.Pointer[mqttbus.BrokerMatcher]
}

func (b *liveBroker) set(rawURL string) { b.p.Store(mqttbus.NewBrokerMatcher(rawURL, nil)) }

// Same reports whether a device configured for host:port talks to our broker.
func (b *liveBroker) Same(host string, port int) bool { return b.p.Load().Same(host, port) }
