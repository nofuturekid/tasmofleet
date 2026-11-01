package firmware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

type serverFixture struct {
	h      http.Handler
	cache  Cache
	dir    string
	art    model.Artifact
	events *eventLog
}

type eventLog struct {
	mu sync.Mutex
	ev []RequestEvent
}

func (l *eventLog) add(e RequestEvent) {
	l.mu.Lock()
	l.ev = append(l.ev, e)
	l.mu.Unlock()
}

func (l *eventLog) all() []RequestEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RequestEvent(nil), l.ev...)
}

func newServerFixture(t *testing.T, mutate func(*ServerOptions)) *serverFixture {
	t.Helper()
	u := newUpstream(t)
	c, _, o := newTestCache(t, u, nil)
	a, err := c.Ensure(context.Background(), "16.0.0", "tasmota-minimal.bin.gz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(context.Background(), "16.0.0", "tasmota32-safeboot.bin"); err != nil {
		t.Fatal(err)
	}
	log := &eventLog{}
	so := ServerOptions{OnRequest: log.add, Logger: quietLog()}
	if mutate != nil {
		mutate(&so)
	}
	return &serverFixture{h: NewServer(c, so), cache: c, dir: o.Dir, art: a, events: log}
}

func (f *serverFixture) do(method, target string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), method, "http://fw"+target, nil)
	r.RemoteAddr = "172.31.13.40:51234"
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}

func TestServerGetAndHead(t *testing.T) {
	f := newServerFixture(t, nil)
	want, err := os.ReadFile(filepath.Join(f.dir, "16.0.0", "tasmota-minimal.bin.gz"))
	if err != nil {
		t.Fatal(err)
	}
	get := f.do(http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz")
	if get.Code != 200 || get.Body.Len() != len(want) {
		t.Fatalf("GET %d, %d bytes, want %d", get.Code, get.Body.Len(), len(want))
	}
	// ESP8266 decides from Content-Length whether the image fits, so it must
	// be present on both verbs.
	if get.Header().Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("GET Content-Length = %q", get.Header().Get("Content-Length"))
	}
	if get.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", get.Header().Get("Content-Type"))
	}
	head := f.do(http.MethodHead, "/fw/16.0.0/tasmota-minimal.bin.gz")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(len(want)) {
		t.Fatalf("HEAD %d, body %d, CL %q", head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}
}

// Acceptance: 404 for unindexed paths, traversal in every encoding, directories.
func TestServerRefusals(t *testing.T) {
	f := newServerFixture(t, nil)
	// A file on disk that is not in the index must stay invisible.
	if err := os.WriteFile(filepath.Join(f.dir, "16.0.0", "stray.bin.gz"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "secret"), []byte("db password"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/fw/16.0.0/tasmota-lite.bin.gz", // not cached
		"/fw/16.0.0/stray.bin.gz",        // on disk, not indexed
		"/fw/9.9.9/tasmota.bin.gz",
		"/fw/16.0.0/", "/fw/16.0.0", "/fw/", "/fw", "/", "/index.html", "/restore/x", "/metrics",
		"/fw/../secret", "/fw/16.0.0/../../secret", "/fw/16.0.0/../secret",
		"/fw/%2e%2e/secret", "/fw/16.0.0/%2e%2e/%2e%2e/secret", "/fw/16.0.0/..%2fsecret",
		"/fw/16.0.0%2f..%2fsecret", "/fw/16.0.0/tasmota-minimal.bin.gz/", "/fw/16.0.0/tasmota-minimal.bin.gz/x",
		"/fw//16.0.0/tasmota-minimal.bin.gz", "/fw/16.0.0//tasmota-minimal.bin.gz", "/fw/16.0.0/%00",
		"/fw/16.0.0/tasmota-minimal.bin.gz%00.png", `/fw/16.0.0/..\secret`, "/FW/16.0.0/tasmota-minimal.bin.gz",
		"/fw/16.0.0/tasmota-minimal.bin.gz?x=../secret/", "/fw/16.0.0/tasmota%2dminimal.bin.gz",
	} {
		for _, m := range []string{http.MethodGet, http.MethodHead} {
			w := f.do(m, p)
			// The one allowed oddity is a query string on a valid path (ignored).
			if p == "/fw/16.0.0/tasmota-minimal.bin.gz?x=../secret/" {
				if w.Code != 200 {
					t.Errorf("%s %s = %d, query string must be ignored", m, p, w.Code)
				}
				continue
			}
			if w.Code != http.StatusNotFound {
				t.Errorf("%s %s = %d, want 404", m, p, w.Code)
			}
		}
	}
}

// Safeboot images are indexed (so Guard can recognise them) but are never an
// OTA target, so the server must not hand them to a device.
func TestServerNeverServesSafeboot(t *testing.T) {
	f := newServerFixture(t, nil)
	if w := f.do(http.MethodGet, "/fw/16.0.0/tasmota32-safeboot.bin"); w.Code != http.StatusNotFound {
		t.Fatalf("safeboot served: %d", w.Code)
	}
}

func TestServerMethods(t *testing.T) {
	f := newServerFixture(t, nil)
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
		w := f.do(m, "/fw/16.0.0/tasmota-minimal.bin.gz")
		if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s = %d Allow=%q", m, w.Code, w.Header().Get("Allow"))
		}
	}
}

// An index row whose file is gone or wrong-sized would send a device a
// truncated image; fail with 404 instead.
func TestServerChecksFileAgainstIndex(t *testing.T) {
	f := newServerFixture(t, nil)
	p := filepath.Join(f.dir, "16.0.0", "tasmota-minimal.bin.gz")
	if err := os.WriteFile(p, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz"); w.Code != http.StatusNotFound {
		t.Fatalf("size-mismatched file served: %d", w.Code)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if w := f.do(http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz"); w.Code != http.StatusNotFound {
		t.Fatalf("missing file: %d", w.Code)
	}
}

func TestServerEmitsRequestEvents(t *testing.T) {
	f := newServerFixture(t, nil)
	f.do(http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz")
	f.do(http.MethodHead, "/fw/16.0.0/tasmota-minimal.bin.gz")
	f.do(http.MethodGet, "/fw/16.0.0/nope.bin.gz")
	ev := f.events.all()
	if len(ev) != 3 {
		t.Fatalf("events = %+v", ev)
	}
	e := ev[0]
	// The engine maps RemoteIP to a device and treats a served GET as "device
	// started downloading"; the port must be stripped for that lookup.
	if e.RemoteIP != "172.31.13.40" || e.Method != "GET" || e.Release != "16.0.0" || e.Filename != "tasmota-minimal.bin.gz" || e.Status != 200 || e.Bytes != f.art.Size || e.ArtifactID != f.art.ID {
		t.Fatalf("event = %+v", e)
	}
	if ev[1].Method != "HEAD" || ev[1].Bytes != 0 {
		t.Fatalf("HEAD event = %+v", ev[1])
	}
	if ev[2].Status != 404 || ev[2].ArtifactID != 0 {
		t.Fatalf("404 event = %+v", ev[2])
	}
}

func TestServerRateLimitPerIP(t *testing.T) {
	clk := newFakeClock()
	f := newServerFixture(t, func(o *ServerOptions) { o.Clock = clk; o.RateLimitPerMinute = 5 })
	do := func(ip string) int {
		r := httptest.NewRequestWithContext(context.Background(), http.MethodHead, "http://fw/fw/16.0.0/tasmota-minimal.bin.gz", nil)
		r.RemoteAddr = ip + ":1000"
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		return w.Code
	}
	for i := 0; i < 5; i++ {
		if c := do("172.31.13.40"); c != 200 {
			t.Fatalf("request %d = %d", i, c)
		}
	}
	if c := do("172.31.13.40"); c != http.StatusTooManyRequests {
		t.Fatalf("over limit = %d, want 429", c)
	}
	// One noisy client must not lock out the others.
	if c := do("172.31.13.41"); c != 200 {
		t.Fatalf("other IP = %d", c)
	}
	clk.Advance(61 * time.Second)
	if c := do("172.31.13.40"); c != 200 {
		t.Fatalf("after window = %d", c)
	}
}

// The handler must be usable over real TCP, as devices use it.
func TestServerOverHTTP(t *testing.T) {
	f := newServerFixture(t, nil)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/fw/16.0.0/tasmota-minimal.bin.gz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || int64(len(b)) != f.art.Size || resp.ContentLength != f.art.Size {
		t.Fatalf("status %d len %d CL %d", resp.StatusCode, len(b), resp.ContentLength)
	}
}

// Server(Cache) from the frozen skeleton must work with the real cache and
// stay closed (404) for caches that cannot look up indexed files.
func TestServerFromCacheOnly(t *testing.T) {
	f := newServerFixture(t, nil)
	h := Server(f.cache)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz", nil))
	if w.Code != 200 {
		t.Fatalf("Server(cache) = %d", w.Code)
	}
	w = httptest.NewRecorder()
	Server(noLookupCache{}).ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/fw/16.0.0/tasmota-minimal.bin.gz", nil))
	if w.Code != 404 {
		t.Fatalf("Server(noLookup) = %d, want 404", w.Code)
	}
}

type noLookupCache struct{ Cache }

// Whatever bytes a client sends, an accepted path must name exactly one
// plain file element pair; no accepted path may contain traversal.
func FuzzParsePath(f *testing.F) {
	for _, s := range []string{"/fw/16.0.0/tasmota.bin.gz", "/fw/../x", "/fw/a/%2e%2e", "/fw//", "/fw/a/b/c", "/fw/\\x/y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		r, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://fw/", nil)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.ParseRequestURI(raw)
		if err != nil {
			return
		}
		r.URL = u
		rel, file, ok := parsePath(r)
		if !ok {
			return
		}
		for _, e := range []string{rel, file} {
			if e == "" || strings.ContainsAny(e, "/\\\x00%") || strings.HasPrefix(e, ".") {
				t.Fatalf("accepted %q -> %q %q", raw, rel, file)
			}
		}
	})
}

// Why (#430): the firmware listener serves firmware only; a restore is pushed to the device, so
// nothing below /restore/ exists here.
func TestServerKnowsNoRestorePath(t *testing.T) {
	f := newServerFixture(t, nil)
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://fw/restore/abc", nil)
	r.RemoteAddr = "172.31.13.40:1000"
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 405 && w.Code != 404 {
		t.Errorf("got %d", w.Code)
	}
}
