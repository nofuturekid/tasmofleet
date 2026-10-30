// Package app wires configuration, store, services, listeners and
// background loops together and owns the process lifecycle (graceful
// shutdown on SIGTERM: stop accepting requests, let running work finish,
// close the store). It contains no business logic.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/changelog"
	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/config"
	"github.com/nofuturekid/tasmofleet/internal/devicecmd"
	"github.com/nofuturekid/tasmofleet/internal/devlock"
	"github.com/nofuturekid/tasmofleet/internal/discovery"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
	"github.com/nofuturekid/tasmofleet/internal/fwselect"
	"github.com/nofuturekid/tasmofleet/internal/health"
	"github.com/nofuturekid/tasmofleet/internal/httpx"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/mqttcmd"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/restore"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// tlsConfig builds the UI listener's TLS configuration (nil: plain HTTP,
// which the configuration only allows behind a proxy or with
// ALLOW_INSECURE_HTTP). Certificate files are re-read on SIGHUP.
func (a *runtime) tlsConfig(ctx context.Context) (*tls.Config, error) {
	cfg := a.cfg
	if cfg.TLSCertFile == "" && !cfg.TLSSelfSigned {
		return nil, nil
	}
	hosts := slices.Clone(cfg.TLSSANs)
	if u, err := url.Parse(cfg.PublicURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	tc, reload, err := httpx.TLSConfig(httpx.TLSOptions{
		CertFile: cfg.TLSCertFile, KeyFile: cfg.TLSKeyFile, SelfSigned: cfg.TLSSelfSigned, DataDir: cfg.DataDir, Hosts: hosts,
	})
	if err != nil {
		return nil, err
	}
	if cfg.TLSSelfSigned {
		a.log.Warn("serving a self-signed certificate: browsers will show a warning", "dir", filepath.Join(cfg.DataDir, "tls"))
	}
	httpx.ReloadOnSIGHUP(ctx, reload)
	return tc, nil
}

// Version is set by the binary (main.version).
var Version = "dev"

// DisplayVersion is Version as the UI (header, About) and the startup log show it: a build
// without a release tag gets the release it is based on (changelog.DisplayVersion).
func DisplayVersion() string { return changelog.DisplayVersion(Version, changelog.Embedded()) }

// shutdownGrace bounds the wait for in-flight requests.
const shutdownGrace = 10 * time.Second

// Run starts everything and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config) error {
	log, level := NewLogger(cfg)
	slog.SetDefault(log)
	svc, err := openServices(cfg, log, Version, true)
	if err != nil {
		return err
	}
	FollowLogLevel(svc.Sys, level, log)
	defer func() {
		if err := svc.Close(); err != nil {
			log.Error("close store", "err", err)
		}
	}()

	a := &runtime{svc: svc, cfg: svc.Cfg, log: log}
	return a.run(ctx)
}

// runtime holds what only the server needs.
type runtime struct {
	svc *Services
	cfg config.Config
	log *slog.Logger

	engine     *ota.Engine
	discovery  *discovery.Manager
	deviceCmd  devicecmd.Service
	bus        mqttbus.Bus
	mqttCmd    *mqttcmd.Channel // the one place that sends commands over MQTT (#256)
	facts      *TransportFacts  // the router's view of a device (transport.Facts)
	sw         *mqttbus.Switch
	mqttKnown  *knownIndex
	tracker    *health.Service
	backups    *backup.Manager
	settings   *settings.Manager
	policy     web.PolicyService
	policyLive *policyReporter
	livePause  *sysconfig.LivePause // Pause / Resume of the background policy reads
	fixer      *policy.Fixer        // "Make compliant": sends and reads back; nil until setupSettings ran
	rules      *sysconfig.Rules
	cache      firmware.Cache
	restore    *restore.Manager
	fwReach    *FWReachLearner // what devices reach (D11); nil in tests that do not need it
	devLocks   *devlock.Locks
	decoder    string // decode-config version, "" if unavailable
	dec        backup.Decoder
	authSvc    *auth.Service
	pages      *web.Pages // sign-in and account pages; Handler attaches the app shell
	fwUp       atomic.Bool
	fwSource   string // where releases come from, for the firmware page (sourceText)
	fwFeed     string // its short name
	bgWG       sync.WaitGroup
	bgContext  context.Context
}

func (a *runtime) run(ctx context.Context) error {
	cfg, svc := a.cfg, a.svc
	bg, cancelBG := context.WithCancel(ctx)
	defer cancelBG()
	a.bgContext = bg
	if err := a.setupDecoder(bg); err != nil {
		return err
	}
	a.logStarting()
	for _, w := range cfg.Warnings {
		a.log.Warn(w)
	}

	if err := a.startMQTT(bg); err != nil {
		return err
	}
	a.goBG("mqtt-cmd", func() error { <-bg.Done(); a.mqttCmd.Close(); return nil }) // wakes commands in flight
	a.tracker = health.New(health.Deps{
		Store: svc.Store, Client: svc.HTTP, Bus: a.bus, Hub: svc.Hub, OnMQTTSeen: a.onMQTTSeen, // HTTP only: it measures HTTP
	}, health.Options{
		StaleAfter: staleAfter(cfg.TelePeriod), PollInterval: cfg.PollInterval,
	})
	a.goBG("health", func() error { return a.tracker.Run(bg) })
	a.setupTransport()
	// Network facts are read in the background, never as part of first contact or a refresh.
	nf := newNetFactsReader(a.svc.Inventory.List,
		func(id model.DeviceID) bool { return a.tracker.Get(id).State == model.HealthOnline },
		a.svc.Prober.ReadNet, a.log)
	a.svc.Prober.ReadNetLater = nf.later
	a.goBG("netfacts", func() error { nf.run(bg); return nil })
	// Firmware version over MQTT for devices on our broker that HTTP did not identify (#87).
	mid := newMQTTIdentity(a.bus, a.mqttCmd, a.svc.Inventory.List, a.svc.Store, a.svc.Audit,
		func(id model.DeviceID) { a.svc.Prober.announce(id) }, a.log)
	mid.known = a.mqttKnown.Known
	mid.spawn = func(f func()) { a.goBG("mqtt-identity-ask", func() error { f(); return nil }) } // waited for at shutdown
	a.svc.Prober.MQTT = mid                                                                      // "Refresh identity" over MQTT for devices HTTP cannot reach (#91)
	a.goBG("mqtt-identity", func() error { mid.run(bg); return nil })

	if err := a.setupBackups(bg); err != nil {
		return err
	}
	if err := a.setupFirmware(); err != nil {
		return err
	}
	a.fwReach = newFWReachLearner(a.svc.Store, a.svc.Audit, clock.Real{}, a.log) // before OTA and restore: both report to it
	a.setupDeviceCmd()                                                           // before the OTA engine: it switches relays back on through it
	if err := a.setupSettings(); err != nil {                                    // before the OTA engine: it runs the policy jobs through the applier built here
		return err
	}
	a.setupOTA()
	if err := a.setupDiscovery(); err != nil {
		return err
	}
	if err := a.setupRestore(); err != nil {
		return err
	}
	if err := a.setupAuth(bg); err != nil {
		return err
	}
	handler, err := a.handler()
	if err != nil {
		return err
	}

	tlsCfg, err := a.tlsConfig(bg)
	if err != nil {
		return err
	}
	ui := &http.Server{
		Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
		TLSConfig: tlsCfg,
	}
	uiLn, err := listen(ctx, cfg.Listen)
	if err != nil {
		return fmt.Errorf("ui listener: %w", err)
	}
	errc := make(chan error, 2)
	go func() {
		if tlsCfg != nil {
			errc <- ui.ServeTLS(uiLn, "", "")
			return
		}
		errc <- ui.Serve(uiLn)
	}()
	log := a.log
	log.Info("ui listening", "addr", uiLn.Addr().String(), "tls", tlsCfg != nil, "base_path", cfg.BasePath, "auth", cfg.AuthMode)

	var fw *http.Server
	if a.cache != nil {
		fw = &http.Server{
			Addr: cfg.FWListen, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute,
			Handler: firmware.NewServer(a.cache, firmware.ServerOptions{OnRequest: a.onFirmwareRequest, Logger: log, Extra: a.restoreHandler()}),
		}
		fwLn, err := listen(ctx, cfg.FWListen)
		if err != nil {
			return fmt.Errorf("firmware listener: %w", err)
		}
		a.fwUp.Store(true)
		go func() { errc <- fw.Serve(fwLn) }()
		log.Info("firmware server listening", "addr", fwLn.Addr().String(), "base_url", cfg.FWBaseURL)
	} else {
		log.Warn("FIRMWARE_BASE_URL is not set: firmware cache, device-facing server and OTA are disabled")
	}

	var runErr error
	select {
	case runErr = <-errc:
		if errors.Is(runErr, http.ErrServerClosed) {
			runErr = nil
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = ui.Shutdown(shutdownCtx)
	if fw != nil {
		err = errors.Join(err, fw.Shutdown(shutdownCtx))
	}
	cancelBG()
	a.bgWG.Wait()
	return errors.Join(runErr, err)
}

// goBG runs f in the background; an error is logged (the process keeps serving).
func (a *runtime) goBG(name string, f func() error) {
	a.bgWG.Go(func() {
		if err := f(); err != nil && !errors.Is(err, context.Canceled) {
			a.log.Error("background task failed", "task", name, "err", err)
		}
	})
}

func listen(ctx context.Context, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

// startMQTT creates the bus switch and connects it to the configured broker. The
// broker can change on the settings page: the switch then moves every consumer to
// the new connection (or turns MQTT off) without a restart.
func (a *runtime) startMQTT(ctx context.Context) error {
	a.mqttKnown = newKnownIndex()
	a.sw = mqttbus.NewSwitch()
	a.bus = a.sw
	a.mqttCmd = mqttcmd.New(a.bus, mqttcmd.Options{Known: a.mqttKnown.Known})
	a.goBG("mqtt-known-index", func() error { return a.runKnownIndex(ctx) })
	a.connectMQTT(ctx, a.cfg)
	a.svc.Sys.Subscribe(func(e sysconfig.Event) {
		for _, k := range []string{"MQTT_URL", "MQTT_USER", "MQTT_PASSWORD", "MQTT_FULLTOPIC"} {
			if slices.Contains(e.Keys, k) {
				a.connectMQTT(ctx, e.New)
				return
			}
		}
	})
	return nil
}

// connectMQTT replaces the connection of the bus switch with one made from cfg.
func (a *runtime) connectMQTT(ctx context.Context, cfg config.Config) {
	if cfg.MQTTURL == "" {
		a.sw.Replace(nil, nil)
		a.log.Info("MQTT is off (no broker URL); health uses HTTP polling only")
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	bus, err := mqttbus.Connect(cctx, mqttbus.Options{
		BrokerURL: cfg.MQTTURL, Username: cfg.MQTTUser, Password: cfg.MQTTPassword, FullTopic: cfg.MQTTFullTopic,
		Prefixes: mqttPrefixes, Known: a.mqttKnown.Known,
		OnDropped: func(reason string) { a.svc.Std.MQTTDropped.With(reason).Inc() },
		AllowAddr: runtimeBrokerAddrAllowed,
	})
	if err != nil {
		cancel()
		a.log.Error("MQTT broker settings are not usable; MQTT stays as it was", "err", err)
		return
	}
	a.sw.Replace(bus, cancel)
	a.log.Info("MQTT connection configured", "broker", redactURLHost(cfg.MQTTURL))
}

func redactURLHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return "?"
}

// onMQTTSeen records that a device reports to our broker (spec 6.4). The
// stored record is read back and written whole, because inventory.Update
// replaces the record (including the credentials) with what it is given.
func (a *runtime) onMQTTSeen(id model.DeviceID, topic string) {
	ctx, cancel := context.WithTimeout(a.bgContext, 10*time.Second)
	defer cancel()
	d, err := a.svc.Inventory.Get(ctx, id)
	if err != nil || d.MQTT == model.MQTTOurs || d.Topic != topic {
		return
	}
	d.MQTT = model.MQTTOurs
	if err := a.svc.Inventory.Update(ctx, "system", d); err != nil {
		a.log.Warn("record MQTT capability", "device", id, "err", err)
	}
}

// setupDecoder finds out whether decode-config is usable and which version it is.
func (a *runtime) setupDecoder(ctx context.Context) error {
	cfg := a.cfg
	if cfg.DecoderPath == "" {
		return nil
	}
	if _, err := os.Stat(cfg.DecoderPath); err != nil {
		a.log.Info("decode-config not found; backups keep the raw dump only", "path", cfg.DecoderPath)
		return nil //nolint:nilerr // a missing decoder is optional, not an error
	}
	tmp := filepath.Join(cfg.DataDir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("decoder temp dir: %w", err)
	}
	d := backup.NewExecDecoder(cfg.DecoderPath, tmp)
	if v, err := d.Version(ctx); err == nil {
		a.decoder, a.dec = v, d
	} else {
		a.log.Warn("decode-config is not usable; backups keep the raw dump only", "path", cfg.DecoderPath, "err", err)
	}
	return nil
}

// logStarting is the first info line: the running version, readable without the image tag (#73).
func (a *runtime) logStarting() {
	dec := a.decoder
	if dec == "" {
		dec = "none"
	}
	a.log.Info("tasmofleet starting", "version", DisplayVersion(), "go", goruntime.Version(), "decoder", dec)
}

func (a *runtime) setupBackups(ctx context.Context) error {
	cfg := a.cfg
	dec := a.dec
	m, err := backup.New(backup.Options{
		DataDir: cfg.DataDir, Devices: a.svc.Store, Store: a.svc.Store, Client: a.svc.Client, Decoder: dec,
		Keep: cfg.BackupKeep, MaxAge: cfg.BackupMaxAge, Audit: a.svc.Audit, Events: a.svc.Hub, Log: a.log,
	})
	if err != nil {
		return err
	}
	a.backups = m
	// 0.18 moved backups from backups/<device-id>/ to hostname directories. This
	// runs before the scheduler and any backup; it only moves files whose
	// checksum matches and never deletes one. A failure is logged, not fatal.
	if _, err := m.MigrateLayout(ctx); err != nil {
		a.log.Error("backup layout migration failed; existing backups stay readable where they are", "err", err)
	}
	return nil
}

func (a *runtime) setupFirmware() error {
	cfg := a.cfg
	if cfg.FWBaseURL == "" {
		return nil
	}
	srcs, err := buildSources(cfg.FWSources)
	if cfg.FWUpstreamOverride != "" {
		var src firmware.Source
		if src, err = overrideSource(cfg.FWUpstreamOverride); err == nil {
			srcs = []firmware.Source{src}
			a.log.Warn("FIRMWARE_UPSTREAM_OVERRIDE is set: the official firmware sources are NOT used (tests and development only)", "url", cfg.FWUpstreamOverride)
		}
	}
	if err != nil {
		return err
	}
	a.fwSource, a.fwFeed = sourceText(srcs)
	st := a.svc.Store
	c, err := firmware.New(firmware.Options{
		Dir: filepath.Join(cfg.DataDir, "firmware"), Sources: srcs, BaseURL: cfg.FWBaseURL,
		CrossCheck: len(srcs) > 1, AllowSingleSource: cfg.FWAllowSingleSource,
		HTTPClient: firmwareClient(),
		Inspect: func(header []byte) (model.ChipFamily, error) {
			info, err := fwselect.InspectImage(header)
			return info.Family, err
		},
		InUseBy: jobsByArtifact(st),
		Logger:  a.log,
		// The background refresh keeps the index fresh, so reads must not
		// refetch on their own earlier (see firmware_refresh.go).
		IndexTTL: indexTTL(cfg.FWRefreshInterval),
	}, st)
	if err != nil {
		return err
	}
	a.cache = c
	a.startFirmwareRefresh()
	return nil
}

// onFirmwareRequest logs device downloads; the OTA engine consumes the same
// hint once it is wired.
func (a *runtime) onFirmwareRequest(e firmware.RequestEvent) {
	a.log.Info("firmware request", "component", "firmware", "remote", e.RemoteIP, "method", e.Method,
		"release", e.Release, "file", e.Filename, "status", e.Status, "bytes", e.Bytes)
	// A served image proves the device reaches the firmware server (D11). HEAD and refusals prove nothing.
	if e.Method == http.MethodGet && e.Status == http.StatusOK && e.Release != "" {
		a.learnFWReach(func(ctx context.Context, l *FWReachLearner) error {
			return l.NoteYesFromIP(ctx, e.RemoteIP, FWReachSourceFirmwareServer)
		})
	}
}

// learnFWReach runs one fwreach write in the background: the callers sit on a device-facing
// request or the OTA engine's notifier and must not wait for the database.
func (a *runtime) learnFWReach(f func(ctx context.Context, l *FWReachLearner) error) {
	if a.fwReach == nil {
		return
	}
	a.bgWG.Go(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(a.bgContext), fwReachTimeout)
		defer cancel()
		if err := f(ctx, a.fwReach); err != nil {
			a.log.Warn("firmware reach not recorded", "component", "fwreach", "err", err)
		}
	})
}

// proxy describes the trusted proxies and the external URL. Cookies follow
// the scheme of each request (TLS listener or trusted X-Forwarded-Proto),
// not PublicURL.
func (a *runtime) proxy() httpx.Proxy {
	return httpx.Proxy{TrustedProxies: a.cfg.TrustedProxies, PublicURL: a.cfg.PublicURL, BasePath: a.cfg.BasePath}
}

func (a *runtime) setupAuth(ctx context.Context) error {
	cfg := a.cfg
	a.pages = web.NewPages()
	key, err := auth.LoadOrCreateKey(cfg.DataDir)
	if err != nil {
		return err
	}
	s, err := auth.New(auth.Options{
		SecretKey: key, DataDir: cfg.DataDir, Recheck: cfg.OIDCRecheck,
		Mode:  cfg.AuthMode,
		Proxy: a.proxy(),
		OIDC: auth.OIDCOptions{
			Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCClientSecret, Scopes: cfg.OIDCScopes,
			GroupsClaim: cfg.OIDCGroupsClaim, UsernameClaim: cfg.OIDCUsernameClaim, DisplayName: cfg.OIDCDisplayName, PostLogoutURL: cfg.OIDCPostLogoutURL,
		},
		Header: auth.HeaderOptions{
			User: cfg.HeaderUser, Name: cfg.HeaderName, Email: cfg.HeaderEmail, Groups: cfg.HeaderGroups,
			GroupsSep: cfg.HeaderGroupsSep, LogoutURL: cfg.HeaderLogoutURL,
		},
		Roles: auth.RoleMapping{
			AdminGroups: cfg.AdminGroups, OperatorGroups: cfg.OperatorGroups,
		},
		LocalLogin: cfg.LocalLogin,
		Sessions:   a.svc.Store, LocalUsers: a.svc.Store, Directory: a.svc.Store, Audit: a.svc.Audit, AuditLog: a.svc.Store,
		Idle: cfg.SessionIdle, Max: cfg.SessionMax,
		Pages: a.pages,
	})
	if err != nil {
		return err
	}
	if created, err := s.EnsureInitialAdmin(ctx, cfg.InitialAdminPassword); err != nil {
		return fmt.Errorf("initial admin: %w", err)
	} else if created {
		a.log.Warn("initial admin account created; it must change its password at the first sign-in", "user", auth.InitialAdminName)
	}
	a.authSvc = s
	a.goBG("auth", func() error { s.Run(a.bgContext); return nil })
	return nil
}

// ready reports the readiness of the pieces /readyz covers.
func (a *runtime) ready(ctx context.Context) error {
	if err := a.svc.Store.Ping(ctx); err != nil {
		return fmt.Errorf("database: %w", err)
	}
	if !a.authSvc.Ready() {
		return errors.New("oidc discovery not finished")
	}
	if a.cache != nil && !a.fwUp.Load() {
		return errors.New("firmware listener not up")
	}
	return nil
}

// metricsHandler serves /metrics behind the master switch (N19, read live from
// the settings) and the token / local-scraper gate.
func (a *runtime) metricsHandler() http.Handler {
	return metricsSwitch(func() bool { return a.svc.Sys.Snapshot().MetricsEnabled },
		metricsGate(func() string { return a.svc.Sys.Snapshot().MetricsToken }, a.svc.Metrics.Handler()))
}

func (a *runtime) handler() (http.Handler, error) {
	cfg := a.cfg
	deps := a.webDeps(a.authSvc, describeConfig(cfg))
	ui, err := web.Handler(deps)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if err := a.ready(r.Context()); err != nil {
			a.log.Warn("not ready", "err", err)
			http.Error(w, "not ready\n", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	if a.svc.Sys.Snapshot().MetricsToken == "" { // effective: environment or settings page
		a.log.Warn("no metrics token is set: /metrics answers local scrapers only (set one on the settings page)")
	}
	mux.Handle("GET /metrics", a.metricsHandler())
	routes := a.authSvc.Routes()
	for _, p := range a.authSvc.MountPaths() {
		mux.Handle(p, routes)
	}
	mux.Handle("/", ui)

	var formActions []string
	if cfg.AuthMode == auth.ModeOIDC {
		if u, err := url.Parse(cfg.OIDCIssuer); err == nil && u.Host != "" {
			formActions = append(formActions, u.Scheme+"://"+u.Host)
		}
	}
	sec, err := httpx.SecurityHeaders(nil, formActions)
	if err != nil {
		return nil, err
	}
	var origins []string
	if cfg.PublicURL != "" {
		origins = append(origins, cfg.PublicURL)
	}
	csrf, err := httpx.NewCSRF(a.authSvc, origins...)
	if err != nil {
		return nil, err
	}
	h := a.countRequests(mux)
	h = csrf.Middleware(h)
	h = guardBodies(a.authSvc, h)
	h = a.authSvc.Middleware(h)
	h = sec(h)
	h = httpx.AccessLog(a.log, h)
	h = a.proxy().Middleware(h)
	a.startBackground()
	return h, nil
}

// countRequests feeds tasmofleet_http_requests_total with the matched route
// pattern (low cardinality) and the status code.
func (a *runtime) countRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := r.Pattern // set in place by the innermost ServeMux that matched
		if route == "" {
			route = "unmatched"
		}
		a.svc.Std.HTTPRequests.With(route, strconv.Itoa(rec.status)).Inc()
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush (SSE).
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// firmwareClient never uses an environment proxy: the per-hop host allowlist
// checks the target URL, so a proxy could otherwise read or rewrite firmware
// downloads.
func firmwareClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the default transport is an *http.Transport
	t.Proxy = nil
	return &http.Client{Timeout: 2 * time.Minute, Transport: t}
}

// webDeps is what the UI needs: its Guard (auth.Service) and the rows of the About page.
func (a *runtime) webDeps(guard web.Guard, rows []web.KV) web.Deps {
	mqttState := func() string {
		switch {
		case !a.sw.Enabled():
			return "disabled"
		case a.bus.Connected():
			return "connected"
		default:
			return "disconnected"
		}
	}
	deps := web.Deps{
		Guard: guard, Inventory: a.svc.Inventory, Health: a.tracker, Backups: a.backups,
		Prober: a.svc.Prober, DeviceNames: a.svc.Store, FWReach: a.fwReach, MQTTPasswords: a.svc.Store, History: a.svc.Store, Audit: a.svc.Store, Recorder: a.svc.Audit,
		Jobs: a.svc.Store, Batches: a.svc.Store, Artifacts: a.svc.Store, Prefs: a.svc.Store, Principals: a.svc.Store, Database: a.svc.Store, Hub: a.svc.Hub, Version: DisplayVersion(), DecoderVersion: a.decoder, MQTT: mqttState,
		Config: rows, Pages: a.pages, FirmwareSource: a.fwSource, FirmwareFeed: a.fwFeed,
	}
	deps.MQTTBroker = a.svc.Broker
	deps.MQTTOnly = a.onlyOverMQTT
	if a.facts != nil {
		deps.Transport = webFacts{a.facts} // the router\'s own view: the card shows what the router decides
	}
	deps.DefaultDevicePassword = func() string { return a.svc.Sys.Snapshot().DeviceDefaultPassword }
	if a.tracker != nil {
		deps.StaleAfter = a.tracker.StaleAfter
	}
	g := deviceGuard{locks: a.deviceLocks(), jobs: a.svc.Store}
	if a.deviceCmd != nil {
		deps.DeviceCmd = lockedDeviceCmd{DeviceCmd: a.deviceCmd, g: g}
	}
	if a.engine != nil {
		deps.OTA, deps.Firmware = lockedOTA{OTA: a.engine, g: g}, a.cache
		if a.deviceCmd != nil {
			deps.CommandBatch = lockedCommandBatch{e: a.engine, g: g}
		}
		deps.BackupBatch = lockedBackupBatch{e: a.engine, g: g}
		if a.policy != nil {
			deps.PolicyFix = lockedPolicyFix{e: a.engine, g: g}
		}
	}
	if s := a.settingsDep(); s != nil {
		deps.Settings = lockedSettings{SettingsService: s, g: g}
	}
	deps.Policy = a.policy
	deps.Import = a.svc.Importer()
	deps.Discovery = a.discovery
	deps.SysConfig = a.svc.Sys
	deps.PolicyRules = a.rules
	deps.PolicyLivePause = a.livePause
	deps.Setup = newSetupService(a.svc.Sys)
	if a.restore != nil {
		deps.Restore = a.restore
	}
	return deps
}
