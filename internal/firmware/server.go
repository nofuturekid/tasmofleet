package firmware

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/clock"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

const defaultRatePerMinute = 120

// RequestEvent describes one request to the firmware listener. OnRequest is
// called when the response starts, so for a GET with Status 200 it is the
// "device X started downloading" hint (Bytes is the announced length, 0 for
// HEAD and non-200 answers).
type RequestEvent struct {
	Time       time.Time
	RemoteIP   string // without port, for the device lookup
	Method     string
	Release    string // empty if the path was not /fw/<release>/<file>
	Filename   string
	ArtifactID model.ArtifactID // 0 unless an indexed artifact was found
	Status     int
	Bytes      int64
}

// ServerOptions configure NewServer. All fields are optional.
type ServerOptions struct {
	OnRequest func(RequestEvent)
	Clock     clock.Clock // nil = real time
	// RateLimitPerMinute caps requests per client IP (a misbehaving IoT
	// client must not starve the OTA). 0 = 120, negative = unlimited.
	RateLimitPerMinute int
	Logger             *slog.Logger
}

// NewServer returns the device-facing handler: GET/HEAD /fw/<release>/<file>
// for indexed, non-safeboot artifacts, nothing else. c must implement
// Locator (the cache from New does); otherwise every request gets a 404.
func NewServer(c Cache, o ServerOptions) http.Handler {
	s := &server{opts: o, clk: o.Clock, log: o.Logger, win: map[string]*window{}}
	s.loc, _ = c.(Locator) // nil if c cannot look up: the handler then answers 404
	if s.clk == nil {
		s.clk = clock.Real{}
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.limit = o.RateLimitPerMinute
	if s.limit == 0 {
		s.limit = defaultRatePerMinute
	}
	return s
}

type server struct {
	opts  ServerOptions
	loc   Locator
	clk   clock.Clock
	log   *slog.Logger
	limit int

	mu  sync.Mutex
	win map[string]*window
}

type window struct {
	start time.Time
	n     int
}

func (s *server) allow(ip string) bool {
	if s.limit < 0 {
		return true
	}
	now := s.clk.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.win) > 1024 {
		for k, w := range s.win {
			if now.Sub(w.start) >= time.Minute {
				delete(s.win, k)
			}
		}
	}
	w := s.win[ip]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		s.win[ip] = w
	}
	w.n++
	return w.n <= s.limit
}

// parsePath accepts exactly /fw/<release>/<file> in canonical form. The
// decoded path must equal the raw one (no percent-encoding anywhere) and
// both elements must pass the strict character check, so "..", encoded
// slashes, backslashes and NUL can never name another file.
func parsePath(r *http.Request) (release, file string, ok bool) {
	if r.URL.RawPath != "" {
		return "", "", false
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "fw" {
		return "", "", false
	}
	if !validRelease(parts[2]) || !validPathElement(parts[3]) {
		return "", "", false
	}
	return parts[2], parts[3], true
}

// parseCustomPath accepts exactly /c/<key>/fw/<release>/<file>, with the same
// strictness as parsePath.
func parseCustomPath(r *http.Request) (key, release, file string, ok bool) {
	if r.URL.RawPath != "" {
		return "", "", "", false
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "c" || parts[3] != "fw" {
		return "", "", "", false
	}
	if !validPathElement(parts[2]) || !validRelease(parts[4]) || !validPathElement(parts[5]) {
		return "", "", "", false
	}
	return parts[2], parts[4], parts[5], true
}

func (s *server) emit(e RequestEvent) {
	s.log.Info("firmware request", "ip", e.RemoteIP, "method", e.Method, "release", e.Release,
		"file", e.Filename, "status", e.Status, "bytes", e.Bytes)
	if s.opts.OnRequest != nil {
		s.opts.OnRequest(e)
	}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	ev := RequestEvent{Time: s.clk.Now(), RemoteIP: ip, Method: r.Method}
	reply := func(code int, msg string) {
		ev.Status = code
		s.emit(ev)
		http.Error(w, msg, code)
	}
	if !s.allow(ip) {
		w.Header().Set("Retry-After", "60")
		reply(http.StatusTooManyRequests, "too many requests")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		reply(http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	release, file, ok := parsePath(r)
	var custom string
	if !ok {
		custom, release, file, ok = parseCustomPath(r)
	}
	if !ok || s.loc == nil {
		reply(http.StatusNotFound, "not found")
		return
	}
	ev.Release, ev.Filename = release, file
	var a model.Artifact
	var path string
	if custom != "" {
		cl, isCustom := s.loc.(CustomLocator)
		if !isCustom {
			reply(http.StatusNotFound, "not found")
			return
		}
		a, path, err = cl.LookupCustom(r.Context(), custom, release, file)
	} else {
		a, path, err = s.loc.Lookup(r.Context(), release, file)
	}
	if err != nil {
		if !errors.Is(err, ErrNotInIndex) && !errors.Is(err, ErrBadName) {
			s.log.Error("firmware lookup failed", "release", release, "file", file, "err", err)
		}
		reply(http.StatusNotFound, "not found")
		return
	}
	f, err := os.Open(path) //nolint:gosec // path comes from Locator, built from validated names
	if err != nil {
		s.log.Error("firmware file unreadable", "path", path, "err", err)
		reply(http.StatusNotFound, "not found")
		return
	}
	defer func() { _ = f.Close() }() // read-only
	ev.ArtifactID, ev.Status = a.ID, http.StatusOK
	if r.Method == http.MethodGet {
		ev.Bytes = a.Size
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	s.emit(ev)
	http.ServeContent(w, r, file, a.FetchedAt, f)
}
