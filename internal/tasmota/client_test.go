package tasmota

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const testPassword = "Zq7-distinctive-PW-9x!"

var loopbackNet = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}

// target points a Target at an httptest server on loopback.
func target(t *testing.T, srv *httptest.Server, id model.DeviceID, user, pass string) Target {
	t.Helper()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	return TargetFor(model.Device{ID: id, Address: host, Port: port, HTTPUser: user, HTTPPassword: pass})
}

func newClient(o Options) Client {
	if o.AllowedNetworks == nil {
		o.AllowedNetworks = loopbackNet
	}
	return NewHTTPClient(o)
}

func fixtureHandler(t *testing.T, device string, hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		dir := filepath.Join(fixturesDir, device)
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write(readFixture(t, dir, "root.html"))
		case "/cm":
			names := map[string]string{"status 2": "status-2.json", "status 4": "status-4.json", "status 11": "status-11.json",
				"upgrade": "upgrade.json", "otaurl": "otaurl.json", "foobar": "unknown-foobar.json", "": "empty-cmnd.json"}
			f, ok := names[strings.ToLower(r.URL.Query().Get("cmnd"))]
			if !ok {
				f = "unknown-foobar.json"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(readFixture(t, dir, f))
		default:
			http.NotFound(w, r)
		}
	})
}

// The SSRF rule: nothing outside the device networks is ever contacted. The
// server must see zero requests, i.e. the refusal happens before connect().
func TestClient_ForbiddenAddressNeverConnects(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(fixtureHandler(t, "kitchen-spotlight", &hits))
	defer srv.Close()
	c := newClient(Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	tg := target(t, srv, 1, "", "")
	ctx := context.Background()

	if _, err := c.Command(ctx, tg, "Status 2"); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("Command: %v", err)
	}
	if _, err := c.RootPage(ctx, tg); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("RootPage: %v", err)
	}
	if _, err := c.DownloadConfig(ctx, tg); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("DownloadConfig: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server saw %d requests, want 0", n)
	}
}

func TestClient_EmptyAllowListForbidsEverything(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(fixtureHandler(t, "kitchen-spotlight", &hits))
	defer srv.Close()
	c := NewHTTPClient(Options{})
	if _, err := c.Command(context.Background(), target(t, srv, 1, "", ""), "Status 2"); !errors.Is(err, ErrForbiddenAddr) || hits.Load() != 0 {
		t.Errorf("err = %v, hits = %d", err, hits.Load())
	}
}

// DNS rebinding: the name looks harmless, the check must apply to the IP the
// name resolves to at dial time. "localhost" resolves to loopback, which is
// outside the allowed network here.
func TestClient_DNSNameResolvingOutsideIsRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(fixtureHandler(t, "kitchen-spotlight", &hits))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	tg := TargetFor(model.Device{ID: 1, Address: "localhost", Port: port})
	c := newClient(Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}})
	if _, err := c.Command(context.Background(), tg, "Status 2"); !errors.Is(err, ErrForbiddenAddr) {
		t.Errorf("err = %v", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server saw %d requests", hits.Load())
	}
}

// Defense in depth: a misconfigured allowlist (0.0.0.0/0, ::/0) must still
// never reach loopback, the cloud metadata address, link-local, multicast,
// unspecified, zoned addresses or IPv4-in-IPv6 / NAT64 disguises of those.
func TestIPAllowed_AlwaysBlocksDangerousAddresses(t *testing.T) {
	everything := []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("::/0")}
	for _, a := range []string{
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1%eth0", "fe80::1", "::ffff:127.0.0.1",
		"::ffff:169.254.169.254", "224.0.0.1", "ff02::1", "0.0.0.0", "::",
		"64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b:1::1", "::127.0.0.1", "fd00::1%eth0",
	} {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		if ipAllowed(ip, everything) {
			t.Errorf("%s allowed under 0.0.0.0/0 + ::/0", a)
		}
	}
	// Ordinary device addresses still pass.
	for _, a := range []string{"172.31.13.28", "10.1.2.3", "192.168.1.5", "fd00::5"} {
		if !ipAllowed(netip.MustParseAddr(a), everything) {
			t.Errorf("%s refused", a)
		}
	}
}

// A denied range is unlocked only by an allowlist entry lying entirely inside
// it (the e2e simulators use 127.0.0.0/8). Wide entries never unlock, and the
// metadata address stays blocked even when listed explicitly.
func TestIPAllowed_DeniedRangeNeedsExplicitNarrowEntry(t *testing.T) {
	pf := netip.MustParsePrefix
	tests := []struct {
		ip   string
		nets []netip.Prefix
		want bool
	}{
		{"127.0.0.1", []netip.Prefix{pf("0.0.0.0/0")}, false},
		{"127.0.0.1", []netip.Prefix{pf("0.0.0.0/0"), pf("::/0")}, false},
		{"127.0.0.1", []netip.Prefix{pf("127.0.0.0/8")}, true},
		{"127.0.0.1", []netip.Prefix{pf("127.0.0.1/32")}, true},
		{"127.0.0.2", []netip.Prefix{pf("127.0.0.1/32")}, false},
		{"127.0.0.1", []netip.Prefix{pf("127.0.0.0/7")}, false}, // wider than the denied range
		{"127.0.0.1", []netip.Prefix{pf("10.0.0.0/8")}, false},
		{"::ffff:127.0.0.1", []netip.Prefix{pf("127.0.0.0/8")}, true},
		{"::1", []netip.Prefix{pf("::/0")}, false},
		{"::1", []netip.Prefix{pf("::1/128")}, true},
		{"169.254.10.10", []netip.Prefix{pf("169.254.0.0/16")}, true},
		{"169.254.169.254", []netip.Prefix{pf("169.254.0.0/16")}, false},
		{"169.254.169.254", []netip.Prefix{pf("169.254.169.254/32")}, false},
		{"fd00:ec2::254", []netip.Prefix{pf("fd00:ec2::254/128")}, false},
		{"224.0.0.1", []netip.Prefix{pf("224.0.0.0/4")}, false},
		{"0.0.0.0", []netip.Prefix{pf("0.0.0.0/32")}, false},
		{"fe80::1%eth0", []netip.Prefix{pf("fe80::/10")}, false},
		{"64:ff9b::7f00:1", []netip.Prefix{pf("64:ff9b::/96")}, false},
		{"10.1.2.3", []netip.Prefix{pf("10.0.0.0/8")}, true},
	}
	for _, tc := range tests {
		if got := ipAllowed(netip.MustParseAddr(tc.ip), tc.nets); got != tc.want {
			t.Errorf("%s with %v: %v, want %v", tc.ip, tc.nets, got, tc.want)
		}
	}
}

// Production client with a wide-open allowlist still refuses a loopback
// server: no connection is made.
func TestClient_WideAllowlistStillBlocksLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(fixtureHandler(t, "kitchen-spotlight", &hits))
	defer srv.Close()
	c := NewHTTPClient(Options{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}})
	if _, err := c.Command(context.Background(), target(t, srv, 1, "", ""), "Status 2"); !errors.Is(err, ErrForbiddenAddr) || hits.Load() != 0 {
		t.Errorf("err = %v, hits = %d", err, hits.Load())
	}
}

func TestClient_CommandReplaysFixtures(t *testing.T) {
	srv := httptest.NewServer(fixtureHandler(t, "kitchen-spotlight", nil))
	defer srv.Close()
	c := newClient(Options{})
	tg := target(t, srv, 1, "", "")
	ctx := context.Background()

	raw, err := c.Command(ctx, tg, "Status 2")
	if err != nil {
		t.Fatal(err)
	}
	fw, err := ParseStatus2(raw)
	if err != nil || fw.Hardware != "ESP8266EX" {
		t.Errorf("Status 2 -> %+v, %v", fw, err)
	}
	// Minimal firmware answers Unknown; the client reports it as an error so
	// callers cannot treat it as data.
	if _, err := c.Command(ctx, tg, "FooBar"); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("FooBar: %v", err)
	}
	// The empty-command WARNING is a normal reply body, not an error.
	if _, err := c.Command(ctx, tg, ""); err != nil {
		t.Errorf("empty cmnd: %v", err)
	}
	info, err := c.RootPage(ctx, tg)
	if err != nil || info.Version != "16.0.0" || info.Minimal {
		t.Errorf("RootPage -> %+v, %v", info, err)
	}
}

// Commands may contain spaces and ';' (Backlog). A raw ';' or '+' in the
// query would be misread by the device or by Go's own query parser.
func TestClient_CommandIsURLEncoded(t *testing.T) {
	var gotRaw, gotCmnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, gotCmnd = r.URL.RawQuery, r.URL.Query().Get("cmnd")
		_, _ = w.Write([]byte(`{"Backlog":"ok"}`))
	}))
	defer srv.Close()
	cmnd := "Backlog Power ON; Delay 5; OtaUrl http://h:80/a+b.bin"
	if _, err := newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), cmnd); err != nil {
		t.Fatal(err)
	}
	if gotCmnd != cmnd {
		t.Errorf("device saw %q, want %q", gotCmnd, cmnd)
	}
	if strings.ContainsAny(gotRaw, " ;") || strings.Contains(gotRaw, "+") && !strings.Contains(gotRaw, "%2B") {
		t.Errorf("raw query not safely encoded: %q", gotRaw)
	}
}

// Credentials travel only in the Authorization header (TasmoAdmin lesson B2).
func TestClient_BasicAuthHeaderAndNoCredentialsInURL(t *testing.T) {
	var auth, rawQuery, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, rawQuery, path = r.Header.Get("Authorization"), r.URL.RawQuery, r.URL.Path
		_, _ = w.Write([]byte(`{"Power":"ON"}`))
	}))
	defer srv.Close()
	tg := target(t, srv, 1, "admin", testPassword)
	if _, err := newClient(Options{}).Command(context.Background(), tg, "Power"); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:"+testPassword))
	if auth != want {
		t.Errorf("Authorization = %q", auth)
	}
	if strings.Contains(rawQuery+path, testPassword) || strings.Contains(rawQuery, "password") || strings.Contains(rawQuery, "user=") {
		t.Errorf("credentials in URL: %q %q", path, rawQuery)
	}
}

func TestClient_NoAuthHeaderWithoutCredentials(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"Power":"ON"}`))
	}))
	defer srv.Close()
	if _, err := newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), "Power"); err != nil || auth != "" {
		t.Errorf("err %v, auth %q", err, auth)
	}
}

func TestClient_AuthFailures(t *testing.T) {
	tests := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"HTTP 401", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"WARNING":"Need user=<username>&password=<password>"}`))
		}},
		{"200 with Need user warning", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"WARNING":"Need user=<username>&password=<password>"}`))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.h)
			defer srv.Close()
			c := newClient(Options{})
			tg := target(t, srv, 1, "admin", testPassword)
			_, err := c.Command(context.Background(), tg, "Status 2")
			if !errors.Is(err, ErrAuth) {
				t.Errorf("Command: %v", err)
			}
			if _, err := c.DownloadConfig(context.Background(), tg); err != nil && !errors.Is(err, ErrAuth) {
				t.Errorf("DownloadConfig: %v", err)
			}
		})
	}
}

// The device password must not appear in any error, whatever went wrong
// (including commands that carry the password, e.g. "WebPassword").
func TestClient_PasswordNeverInErrors(t *testing.T) {
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		_ = conn.Close()
	}))
	defer drop.Close()
	e500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom "+r.Header.Get("Authorization"), 500)
	}))
	defer e500.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://admin:"+testPassword+"@evil.example/", http.StatusFound)
	}))
	defer redirect.Close()
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("password=" + testPassword + " not json"))
	}))
	defer garbage.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closedTarget := target(t, closed, 9, "admin", testPassword)
	closed.Close()
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()

	c := newClient(Options{Timeout: 150 * time.Millisecond})
	ctx := context.Background()
	for name, tg := range map[string]Target{
		"drop": target(t, drop, 1, "admin", testPassword), "500": target(t, e500, 2, "admin", testPassword),
		"redirect": target(t, redirect, 3, "admin", testPassword), "garbage": target(t, garbage, 4, "admin", testPassword),
		"closed": closedTarget, "hang": target(t, hang, 5, "admin", testPassword),
	} {
		for op, f := range map[string]func() error{
			"cmd":  func() error { _, e := c.Command(ctx, tg, "WebPassword "+testPassword); return e },
			"root": func() error { _, e := c.RootPage(ctx, tg); return e },
			"dl":   func() error { _, e := c.DownloadConfig(ctx, tg); return e },
		} {
			err := f()
			if err == nil {
				// /dl returns arbitrary bytes by design; any body is a valid dump.
				if name != "garbage" || op != "dl" {
					t.Errorf("%s/%s: expected an error", name, op)
				}
				continue
			}
			if strings.Contains(err.Error(), testPassword) || strings.Contains(err.Error(), base64.StdEncoding.EncodeToString([]byte("admin:"+testPassword))) {
				t.Errorf("%s/%s leaks the password: %v", name, op, err)
			}
		}
	}
}

// ESP8266 web servers are single-threaded: two concurrent requests to one
// device make it drop connections. Different devices run in parallel.
func TestClient_SerialisesPerDevice(t *testing.T) {
	var inflight, maxInflight atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if n <= m || maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
		inflight.Add(-1)
		_, _ = w.Write([]byte(`{"Power":"ON"}`))
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c := newClient(Options{})
	tg := target(t, srv, 1, "", "")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Command(context.Background(), tg, "Power"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m := maxInflight.Load(); m != 1 {
		t.Errorf("max in-flight requests to one device = %d, want 1", m)
	}
}

func TestClient_DifferentDevicesRunInParallelUpToGlobalLimit(t *testing.T) {
	run := func(limit, devices int) int32 {
		var inflight, maxInflight atomic.Int32
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := inflight.Add(1)
			for {
				m := maxInflight.Load()
				if n <= m || maxInflight.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(60 * time.Millisecond)
			inflight.Add(-1)
			_, _ = w.Write([]byte(`{"Power":"ON"}`))
		})
		c := newClient(Options{MaxConcurrent: limit})
		var wg sync.WaitGroup
		for i := range devices {
			srv := httptest.NewServer(h)
			defer srv.Close()
			tg := target(t, srv, model.DeviceID(i+1), "", "")
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.Command(context.Background(), tg, "Power"); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		return maxInflight.Load()
	}
	if m := run(100, 4); m < 2 {
		t.Errorf("devices were serialised globally (max in-flight %d)", m)
	}
	if m := run(2, 5); m > 2 {
		t.Errorf("global limit 2 exceeded: max in-flight %d", m)
	}
}

// A hanging device must fail within the timeout, and the error must be
// ErrTimeout so the engine can tell it apart from "unreachable".
func TestClient_HangYieldsErrTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	c := newClient(Options{Timeout: 100 * time.Millisecond})
	start := time.Now()
	_, err := c.Command(context.Background(), target(t, srv, 1, "", ""), "Status 2")
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestClient_CallerCancellationIsNotATimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := newClient(Options{Timeout: 5 * time.Second}).Command(ctx, target(t, srv, 1, "", ""), "Status 2")
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v", err)
	}
}

// A request queued behind a slow one must honour its own context while
// waiting for the per-device slot.
func TestClient_QueuedRequestHonoursContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"Power":"ON"}`))
	}))
	defer srv.Close()
	defer close(release)
	c := newClient(Options{Timeout: 5 * time.Second})
	tg := target(t, srv, 1, "", "")
	go func() { _, _ = c.Command(context.Background(), tg, "Power") }()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Command(ctx, tg, "Power"); err == nil {
		t.Error("queued request should fail when its context expires")
	}
}

func TestClient_ConnectionRefusedIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	tg := target(t, srv, 1, "", "")
	srv.Close()
	_, err := newClient(Options{}).Command(context.Background(), tg, "Status 2")
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v", err)
	}
}

// Why: the router may start the same command over MQTT only when it is certain the HTTP request
// never left. A refused connection is that proof; a dropped connection after the request was
// written (the server closes without answering) is not: the command may have run.
func TestNotSent(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	tg := target(t, srv, 1, "", "")
	srv.Close()
	_, err := newClient(Options{}).Command(context.Background(), tg, "Power1 TOGGLE")
	if !errors.Is(err, ErrUnreachable) || !NotSent(err) {
		t.Errorf("refused connection: err = %v, NotSent = %v", err, NotSent(err))
	}

	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, _ := w.(http.Hijacker).Hijack()
		_ = c.Close() // the request arrived; nothing comes back
	}))
	defer cut.Close()
	_, err = newClient(Options{}).Command(context.Background(), target(t, cut, 1, "", ""), "Power1 TOGGLE")
	if err == nil || NotSent(err) {
		t.Errorf("dropped after the request: err = %v, NotSent = %v (must be unknown)", err, NotSent(err))
	}

	for _, e := range []error{ErrForbiddenAddr, ErrAuth, ErrHTTPAPIRefused} {
		if !NotSent(&deviceError{e, "x"}) {
			t.Errorf("%v is certain: nothing ran", e)
		}
	}
	for _, e := range []error{ErrTimeout, ErrBadResponse, ErrUnreachable, ErrUnknownCommand} {
		if NotSent(&deviceError{e, "x"}) {
			t.Errorf("%v proves nothing", e)
		}
	}
}

// Redirects would let a device (or a MITM) steer the server to an arbitrary
// URL, bypassing the target abstraction.
func TestClient_DoesNotFollowRedirects(t *testing.T) {
	var other atomic.Int32
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other.Add(1) }))
	defer o.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, o.URL+"/cm", http.StatusFound)
	}))
	defer srv.Close()
	_, err := newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), "Status 2")
	if !errors.Is(err, ErrBadResponse) || other.Load() != 0 {
		t.Errorf("err = %v, redirect target hits = %d", err, other.Load())
	}
}

func TestClient_ResponseSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Big":"` + strings.Repeat("x", 4096) + `"}`))
	}))
	defer srv.Close()
	c := newClient(Options{MaxResponse: 1024})
	tg := target(t, srv, 1, "", "")
	if _, err := c.Command(context.Background(), tg, "Big"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("Command: %v", err)
	}
	if _, err := c.DownloadConfig(context.Background(), tg); !errors.Is(err, ErrBadResponse) {
		t.Errorf("DownloadConfig: %v", err)
	}
	if _, err := newClient(Options{}).Command(context.Background(), tg, "Big"); err != nil {
		t.Errorf("default limit must allow 4 KiB: %v", err)
	}
}

func TestClient_NonJSONAndErrorStatus(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"html":     func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) },
		"500":      func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", 500) },
		"404":      func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"splice":   func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"StatusFWR":{"Ver{"Status`)) },
		"empty":    func(w http.ResponseWriter, r *http.Request) {},
		"jsonlist": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`[1]`)) },
	} {
		srv := httptest.NewServer(h)
		_, err := newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), "Status 2")
		srv.Close()
		if !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// Command output over HTTP depends on WebLog >= 2. The client fixes that
// once per call, then retries once; it never loops (each WebLog write hits
// flash) and never retries Backlog (side effects).
func TestClient_WebLogQuirk(t *testing.T) {
	serve := func(alwaysWarn bool) (*httptest.Server, func() []string) {
		var mu sync.Mutex
		var cmds []string
		weblogSet := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := r.URL.Query().Get("cmnd")
			mu.Lock()
			cmds = append(cmds, c)
			if c == "WebLog 2" {
				weblogSet = true
			}
			ok := weblogSet && !alwaysWarn
			mu.Unlock()
			switch {
			case c == "WebLog 2":
				_, _ = w.Write([]byte(`{"WebLog":2}`))
			case ok:
				_, _ = w.Write([]byte(`{"Power":"ON"}`))
			default:
				_, _ = w.Write([]byte(`{"WARNING":"Weblog level too low"}`))
			}
		}))
		return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), cmds...) }
	}

	srv, cmds := serve(false)
	defer srv.Close()
	c := newClient(Options{})
	raw, err := c.Command(context.Background(), target(t, srv, 1, "", ""), "Power")
	if err != nil || string(raw) != `{"Power":"ON"}` {
		t.Fatalf("raw %s, err %v", raw, err)
	}
	if got := strings.Join(cmds(), "|"); got != "Power|WebLog 2|Power" {
		t.Errorf("sequence %q", got)
	}

	srv2, cmds2 := serve(true)
	defer srv2.Close()
	if _, err := newClient(Options{}).Command(context.Background(), target(t, srv2, 2, "", ""), "Power"); !errors.Is(err, ErrBadResponse) {
		t.Errorf("persistent warning: %v", err)
	}
	if got := len(cmds2()); got != 3 {
		t.Errorf("persistent warning caused %d requests, want 3 (one fix, one retry)", got)
	}

	srv3, cmds3 := serve(false)
	defer srv3.Close()
	_, _ = newClient(Options{}).Command(context.Background(), target(t, srv3, 3, "", ""), "Backlog Power ON")
	if got := cmds3(); len(got) != 1 {
		t.Errorf("Backlog must not trigger the WebLog fix and retry: %v", got)
	}
}

// Legacy firmware applies the command and then warns about the web log level.
// Sending a state-changing command again would toggle twice (a silent no-op
// reported as success) or restart twice, so only queries are repeated.
func TestClient_WebLogWarningNeverRepeatsStateChangingCommands(t *testing.T) {
	run := func(cmnd string) []string {
		var mu sync.Mutex
		var cmds []string
		weblogSet := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := r.URL.Query().Get("cmnd")
			mu.Lock()
			cmds = append(cmds, c)
			if c == "WebLog 2" {
				weblogSet = true
			}
			set := weblogSet
			mu.Unlock()
			switch {
			case c == "WebLog 2":
				_, _ = w.Write([]byte(`{"WebLog":2}`))
			case set:
				_, _ = w.Write([]byte(`{"Result":"ok"}`))
			default:
				_, _ = w.Write([]byte(`{"WARNING":"Weblog level too low"}`))
			}
		}))
		defer srv.Close()
		_, _ = newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), cmnd)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), cmds...)
	}
	if got := strings.Join(run("Power1 TOGGLE"), "|"); got != "Power1 TOGGLE|WebLog 2" {
		t.Errorf("TOGGLE sequence %q: it must be sent exactly once", got)
	}
	if got := strings.Join(run("Restart 1"), "|"); got != "Restart 1|WebLog 2" {
		t.Errorf("Restart sequence %q", got)
	}
	if got := strings.Join(run("Status 2"), "|"); got != "Status 2|WebLog 2|Status 2" {
		t.Errorf("a query is retried once: %q", got)
	}
	_, err := func() (any, error) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cmnd") == "WebLog 2" {
				_, _ = w.Write([]byte(`{"WebLog":2}`))
				return
			}
			_, _ = w.Write([]byte(`{"WARNING":"Weblog level too low"}`))
		}))
		defer srv.Close()
		return newClient(Options{}).Command(context.Background(), target(t, srv, 2, "", ""), "Power1 TOGGLE")
	}()
	if !errors.Is(err, ErrBadResponse) || !strings.Contains(err.Error(), "applied") {
		t.Errorf("the caller must learn that the command was applied: %v", err)
	}
}

// The WARNING for an empty command is not a WebLog problem.
func TestClient_EnterCommandWarningDoesNotTriggerWebLogFix(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = w.Write([]byte(`{"WARNING":"Enter command cmnd="}`))
	}))
	defer srv.Close()
	if _, err := newClient(Options{}).Command(context.Background(), target(t, srv, 1, "", ""), ""); err != nil || n.Load() != 1 {
		t.Errorf("err %v, requests %d", err, n.Load())
	}
}

func TestClient_DownloadConfig(t *testing.T) {
	dump := []byte{0x00, 0x01, 0xA5, 0x5A, 0xFF}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dl" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(dump)
	}))
	defer srv.Close()
	got, err := newClient(Options{}).DownloadConfig(context.Background(), target(t, srv, 1, "", ""))
	if err != nil || string(got) != string(dump) {
		t.Errorf("got %x, %v", got, err)
	}
}

func TestClient_RootPageMinimalFirmware(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(minimalRoot)) }))
	defer srv.Close()
	info, err := newClient(Options{}).RootPage(context.Background(), target(t, srv, 1, "", ""))
	if err != nil || !info.Minimal || info.Image != "release-minimal" || info.Lang != "de" {
		t.Errorf("%+v, %v", info, err)
	}
}

// ESP8266 replies come with long timeouts only where flash or OTA is
// involved; a stuck Status query must not block the device slot for 20 s.
func TestTimeoutClasses(t *testing.T) {
	tests := []struct {
		cmnd string
		want time.Duration
	}{
		{"Status 2", 10 * time.Second}, {"Power", 10 * time.Second},
		{"OtaUrl http://x", 15 * time.Second}, {"otaurl", 15 * time.Second},
		{"Upgrade 1", 15 * time.Second}, {"Upgrade", 15 * time.Second},
	}
	for _, tc := range tests {
		if got := commandTimeout(tc.cmnd); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.cmnd, got, tc.want)
		}
	}
	if downloadTimeout != 20*time.Second || rootTimeout != 10*time.Second {
		t.Errorf("dl %v root %v", downloadTimeout, rootTimeout)
	}
}

// Why: error texts reach users ("no answer from plug.example.internal"); the
// default port 80 is noise, other ports are information.
func TestTarget_DisplayOmitsDefaultPort(t *testing.T) {
	d := func(port int) string {
		return TargetFor(model.Device{Address: "plug.example.internal", Port: port}).display()
	}
	if got := d(0); got != "plug.example.internal" {
		t.Errorf("default: %q", got)
	}
	if got := d(80); got != "plug.example.internal" {
		t.Errorf("80: %q", got)
	}
	if got := d(8080); got != "plug.example.internal:8080" {
		t.Errorf("8080: %q", got)
	}
}
