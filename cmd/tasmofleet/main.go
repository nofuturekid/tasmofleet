// Command tasmofleet is the Tasmota fleet manager.
//
//	tasmofleet serve                              run the server (default)
//	tasmofleet user add|passwd|role|disable|enable|delete|list   manage local accounts (password on stdin)
//	tasmofleet import tasmoadmin [--dry-run] <dir> import devices.csv + MyConfig.json
//	tasmofleet settings export|import <file>      operational settings and policy rules as JSON (automated setups)
//	tasmofleet sessions revoke [--user <name>]    end every web session, or one user's
//	tasmofleet healthcheck                        GET /healthz on the local listener (container HEALTHCHECK)
//	tasmofleet version [--full]                   the version; --full adds the build stamp (commit)
//
// Local accounts are managed here (host access) and, for admins with step-up,
// in the web UI; both go through auth.Accounts.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/app"
	"github.com/nofuturekid/tasmofleet/internal/config"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tasmofleet:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	app.Version = version
	switch cmd {
	case "version":
		_, _ = fmt.Fprintln(stdout, app.DisplayVersion())
		if len(args) > 0 && args[0] == "--full" { // the build stamp as built in, for bug reports (#436)
			_, _ = fmt.Fprintln(stdout, "build", version)
		}
		return nil
	case "serve":
		cfg, err := config.Load(os.Environ())
		if err != nil {
			return err
		}
		return app.Run(ctx, cfg)
	case "healthcheck":
		return healthcheck(ctx)
	case "user":
		return userCmd(ctx, args, stdin, stdout)
	case "import":
		return importCmd(ctx, args, stdout)
	case "settings":
		return settingsCmd(ctx, args, stdin, stdout)
	case "sessions":
		return sessionsCmd(ctx, args, stdout)
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// openServices loads the configuration and opens the data directory, like
// the server does, so the CLI sees the same database.
func openServices() (*app.Services, error) {
	cfg, err := config.Load(os.Environ())
	if err != nil {
		return nil, err
	}
	if os.Getenv(config.Prefix+"LOG_LEVEL") == "" {
		cfg.LogLevel = "warn" // CLI output is the result, not the audit log lines
	}
	log, _ := app.NewLogger(cfg) // the CLI keeps its level; the settings page is for the server
	return app.OpenServices(cfg, log, version)
}

func readLine(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 4096))
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\r\n")
	if s == "" {
		return "", errors.New("empty password")
	}
	return s, nil
}

func importCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "tasmoadmin" {
		return errors.New("usage: tasmofleet import tasmoadmin [--dry-run] [--no-probe] <dir>")
	}
	fs := flag.NewFlagSet("import tasmoadmin", flag.ContinueOnError)
	dry := fs.Bool("dry-run", false, "print the plan without writing or contacting devices")
	noProbe := fs.Bool("no-probe", false, "do not contact the devices (no MAC or identity)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: tasmofleet import tasmoadmin [--dry-run] [--no-probe] <dir>")
	}
	svc, err := openServices()
	if err != nil {
		return err
	}
	defer func() { _ = svc.Close() }() // exiting; the import result is what matters
	res, err := app.ImportTasmoAdmin(ctx, svc, app.ImportOptions{Dir: fs.Arg(0), DryRun: *dry, NoProbe: *noProbe}, stdout)
	if err != nil {
		return err
	}
	if res.Failed > 0 {
		return fmt.Errorf("%d devices could not be imported", res.Failed)
	}
	return nil
}

// healthcheck probes the local UI listener; used as container HEALTHCHECK
// because the distroless image has no curl.
func healthcheck(ctx context.Context) error {
	cfg, err := config.Load(os.Environ())
	if err != nil {
		return err
	}
	addr := cfg.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	scheme := "http"
	client := http.DefaultClient
	if cfg.TLSCertFile != "" || cfg.TLSSelfSigned {
		// The local probe only asks whether the process is alive.
		scheme = "https"
		client = &http.Client{Transport: &http.Transport{TLSClientConfig: insecureLoopback()}}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+addr+cfg.BasePath+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }() // body is not read
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}
