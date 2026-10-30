package tasmota

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/netaddr"
)

// Timeout classes (spec §5).
const (
	statusTimeout   = 10 * time.Second
	upgradeTimeout  = 15 * time.Second
	downloadTimeout = 20 * time.Second
	rootTimeout     = 10 * time.Second
	followUpTimeout = 3 * time.Second

	defaultMaxResponse   = 1 << 20
	defaultMaxConcurrent = 8
	dialTimeout          = 5 * time.Second
)

// commandTimeout picks the class by command: flash and OTA commands get the
// longer timeout, everything else the Status class.
func commandTimeout(cmnd string) time.Duration {
	if isOTACommand(cmnd) {
		return upgradeTimeout
	}
	return statusTimeout
}

// isOTACommand: OtaUrl and Upgrade, whose lost replies the OTA engine probes
// itself (safety rule 3).
func isOTACommand(cmnd string) bool {
	l := strings.ToLower(cmnd)
	return strings.Contains(l, "otaurl") || strings.Contains(l, "upgrade")
}

// deviceError carries a sentinel for errors.Is and a message that has been
// redacted. The underlying transport error is deliberately not wrapped: it
// may contain URLs or echoed device output.
type deviceError struct {
	kind error
	msg  string
}

// notSentKind marks a kind whose error proves that no request byte left tasmofleet: the TCP
// connection was never made. It reads and unwraps like the kind it wraps.
type notSentKind struct{ error }

func (k notSentKind) Unwrap() error { return k.error }

// NotSent reports whether err proves that the request never reached the device: the connection
// could not be made (refused, no route), the address is outside the device networks, the device
// answered 401 before it ran anything, or it refuses HTTP API commands (SetOption128, the handler
// sends nothing and runs nothing). A timeout, a dropped connection after the request was written
// or an unparsable answer prove nothing: the command may have run. The transport router fails
// over to MQTT on these errors only.
func NotSent(err error) bool {
	if errors.Is(err, ErrForbiddenAddr) || errors.Is(err, ErrAuth) || errors.Is(err, ErrHTTPAPIRefused) {
		return true
	}
	var k notSentKind
	return errors.As(err, &k)
}

func (e *deviceError) Error() string { return e.kind.Error() + ": " + e.msg }
func (e *deviceError) Unwrap() error { return e.kind }

type httpClient struct {
	o      Options
	hc     *http.Client
	global chan struct{}
	log    *slog.Logger

	mu    sync.Mutex
	locks map[string]chan struct{} // host:port -> 1-slot semaphore
}

// NewHTTPClient returns the production client.
func NewHTTPClient(o Options) Client {
	if o.MaxResponse <= 0 {
		o.MaxResponse = defaultMaxResponse
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = defaultMaxConcurrent
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	allowed := append([]netip.Prefix(nil), o.AllowedNetworks...)
	dialer := &net.Dialer{
		Timeout: dialTimeout,
		// Control runs on the resolved IP right before connect(2), so DNS
		// rebinding and redirects to other names cannot bypass the check, and
		// a refused address never produces a TCP connection.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: unparsable dial address", ErrForbiddenAddr)
			}
			nets := allowed
			if o.Networks != nil {
				nets = o.Networks.Get()
			}
			if !ipAllowed(ap.Addr(), nets) {
				return fmt.Errorf("%w: %s", ErrForbiddenAddr, ap.Addr())
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:             nil, // never route device traffic through an env proxy
		DialContext:       dialer.DialContext,
		DisableKeepAlives: true, // devices answer with Connection: close
	}
	return &httpClient{
		o:      o,
		global: make(chan struct{}, o.MaxConcurrent),
		log:    log,
		locks:  map[string]chan struct{}{},
		hc: &http.Client{
			Transport:     tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	v4Compat   = netip.MustParsePrefix("::/96") // deprecated IPv4-compatible form, also covers ::
	loopback1  = netip.MustParseAddr("::1")

	metadataV4 = netip.MustParseAddr("169.254.169.254")
	metadataV6 = netip.MustParseAddr("fd00:ec2::254")

	// Ranges an allowlist entry can unlock, but only by lying entirely inside.
	unlockable = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("fe80::/10"),
	}
)

// IPAllowed reports whether a device at ip may be contacted given the
// configured device networks. The inventory uses it to validate addresses
// with exactly the rule the dialer enforces.
func IPAllowed(ip netip.Addr, nets []netip.Prefix) bool { return ipAllowed(ip, nets) }

// ipAllowed applies the hard deny list, then the configured allowlist, for
// literal targets and resolved names alike (both reach the dialer's Control
// hook as an IP).
//
// Never allowed: zoned addresses, the cloud metadata addresses, unspecified,
// multicast, NAT64 and IPv4-compatible addresses. Loopback and link-local are
// allowed only if an allowlist entry lies entirely inside that range (e.g.
// 127.0.0.0/8 or 127.0.0.1/32); wide entries such as 0.0.0.0/0 or ::/0 never
// unlock them. Everything else must be inside an allowlist entry.
func ipAllowed(ip netip.Addr, nets []netip.Prefix) bool {
	if ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip == metadataV4, ip == metadataV6, ip.IsUnspecified(), ip.IsMulticast(),
		nat64.Contains(ip), nat64Local.Contains(ip), v4Compat.Contains(ip) && ip != loopback1:
		return false
	}
	for _, d := range unlockable {
		if d.Contains(ip) {
			for _, p := range nets {
				if p.Contains(ip) && p.Bits() >= d.Bits() && d.Contains(p.Addr().Unmap()) {
					return true
				}
			}
			return false
		}
	}
	for _, p := range nets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (c *httpClient) timeout(class time.Duration) time.Duration {
	if c.o.Timeout > 0 {
		return c.o.Timeout
	}
	return class
}

// display is the address as shown in errors and logs: no default port.
func (t Target) display() string { return netaddr.Display(t.host, t.port, "http") }

func (t Target) hostPort() string { return net.JoinHostPort(t.host, strconv.Itoa(t.port)) }

// redact removes the device credentials from s.
func (t Target) redact(s string) string {
	if t.pass == "" {
		return s
	}
	user := t.user
	if user == "" {
		user = "admin"
	}
	s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte(user+":"+t.pass)), "***")
	s = strings.ReplaceAll(s, url.QueryEscape(t.pass), "***")
	return strings.ReplaceAll(s, t.pass, "***")
}

func (c *httpClient) deviceLock(key string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.locks[key]
	if !ok {
		l = make(chan struct{}, 1)
		c.locks[key] = l
	}
	return l
}

// acquire takes the device slot first and the global slot second, so a
// request queued behind a slow device does not hold a global slot.
func (c *httpClient) acquire(ctx context.Context, key string) (release func(), err error) {
	dev := c.deviceLock(key)
	select {
	case dev <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case c.global <- struct{}{}:
	case <-ctx.Done():
		<-dev
		return nil, ctx.Err()
	}
	return func() { <-c.global; <-dev }, nil
}

// get performs one GET and returns the body (at most MaxResponse bytes).
func (c *httpClient) get(ctx context.Context, t Target, path, rawQuery string, class time.Duration) ([]byte, error) {
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return nil, err
	}
	defer release()

	rctx, cancel := context.WithTimeout(ctx, c.timeout(class))
	defer cancel()
	u := url.URL{Scheme: "http", Host: t.hostPort(), Path: path, RawQuery: rawQuery}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &deviceError{ErrBadResponse, "invalid target"}
	}
	if t.user != "" || t.pass != "" {
		user := t.user
		if user == "" {
			user = "admin" // Tasmota's fixed web user
		}
		req.SetBasicAuth(user, t.pass)
	}
	setReferer(req, t)
	resp, err := c.hc.Do(req)
	if err != nil {
		if path == "/cm" {
			return nil, c.mapCommandNoReply(ctx, t, cmndOf(rawQuery), err)
		}
		return nil, c.mapErr(ctx, t, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body, nothing to recover
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, &deviceError{ErrAuth, "HTTP 401"}
	case resp.StatusCode != http.StatusOK:
		return nil, &deviceError{ErrBadResponse, "HTTP " + strconv.Itoa(resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.o.MaxResponse+1))
	if err != nil {
		return nil, c.mapErr(ctx, t, err)
	}
	if int64(len(body)) > c.o.MaxResponse {
		return nil, &deviceError{ErrBadResponse, "response larger than limit"}
	}
	return body, nil
}

// cmndOf extracts the command from the /cm query. An unparsable query yields
// "", which is not an OTA command; the request itself would have failed first.
func cmndOf(rawQuery string) string {
	v, err := url.ParseQuery(rawQuery)
	if err != nil {
		return ""
	}
	return v.Get("cmnd")
}

// setReferer sends the Referer that Tasmota's default (SetOption128 0) demands
// of every /cm, /dl, /up and /u2 request: the device's hostname or IP right
// after "http://". The target address is the one the request goes to, so it
// matches whenever the device is addressed by its IP or its own hostname. It
// is a CSRF guard for browsers; a non-browser client may set it.
func setReferer(req *http.Request, t Target) {
	req.Header.Set("Referer", "http://"+t.display()+"/")
}

// mapCommandNoReply is mapErr for a /cm command that got no answer. EOF before
// any response byte could be a dropped link or a device that refuses HTTP API
// commands (SetOption128 0, no Referer match). One follow-up GET of the main
// page, which Tasmota does not Referer-guard, tells them apart: if it answers,
// the device is up and refuses. OtaUrl and Upgrade never get the follow-up, so
// it cannot interfere with the OTA engine's own probe-after-loss logic.
func (c *httpClient) mapCommandNoReply(parent context.Context, t Target, cmnd string, err error) error {
	if errors.Is(err, io.EOF) && parent.Err() == nil && !isOTACommand(cmnd) && c.rootAnswers(parent, t) {
		return &deviceError{ErrHTTPAPIRefused, t.redact("set SetOption128 1 on " + t.display() +
			" (console, or Configure Other > HTTP API), or add it by its own IP or hostname")}
	}
	return c.mapErr(parent, t, err)
}

// rootAnswers reports whether GET / gets any HTTP answer within a short time.
// The caller holds the device slot.
func (c *httpClient) rootAnswers(parent context.Context, t Target) bool {
	ctx, cancel := context.WithTimeout(parent, c.timeout(followUpTimeout))
	defer cancel()
	u := url.URL{Scheme: "http", Host: t.hostPort(), Path: "/"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	if t.user != "" || t.pass != "" {
		user := t.user
		if user == "" {
			user = "admin"
		}
		req.SetBasicAuth(user, t.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close() // only the fact of an answer matters
	return true
}

// mapErr turns a transport error into a sentinel-carrying, redacted error.
func (c *httpClient) mapErr(parent context.Context, t Target, err error) error {
	switch {
	case errors.Is(err, ErrForbiddenAddr):
		return &deviceError{ErrForbiddenAddr, t.redact(t.host)}
	case errors.Is(parent.Err(), context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded), isNetTimeout(err):
		return &deviceError{ErrTimeout, "no answer from " + t.redact(t.display())}
	}
	msg := "connection to " + t.display() + " failed"
	var oe *net.OpError
	kind := ErrUnreachable
	if errors.As(err, &oe) {
		msg = oe.Op + " " + t.display() + ": " + oe.Err.Error()
		if oe.Op == "dial" {
			kind = notSentKind{ErrUnreachable}
		}
	}
	return &deviceError{kind, t.redact(msg)}
}

func isNetTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func encodeCmnd(cmnd string) string {
	// Spaces as %20: Tasmota decodes it everywhere, and '+' is ambiguous.
	return "cmnd=" + strings.ReplaceAll(url.QueryEscape(cmnd), "+", "%20")
}

func (c *httpClient) command(ctx context.Context, t Target, cmnd string) (json.RawMessage, error) {
	body, err := c.get(ctx, t, "/cm", encodeCmnd(cmnd), commandTimeout(cmnd))
	if err != nil {
		return nil, err
	}
	if err := classifyReply(body); err != nil {
		if errors.Is(err, ErrBadResponse) && !errors.Is(err, ErrCommandError) {
			return nil, &deviceError{ErrBadResponse, "reply to command is not a JSON object"}
		}
		return nil, err
	}
	return body, nil
}

// webLogWarning reports a reply that is a WARNING other than the empty
// command notice (Need user is already mapped to ErrAuth).
func webLogWarning(raw []byte) bool {
	m, err := decodeObject(raw)
	if err != nil {
		return false
	}
	w, ok := stringField(m, "WARNING")
	return ok && !strings.HasPrefix(w, "Enter command")
}

func (c *httpClient) Command(ctx context.Context, t Target, cmnd string) (json.RawMessage, error) {
	raw, err := c.command(ctx, t, cmnd)
	if err != nil || !webLogWarning(raw) {
		return raw, err
	}
	l := strings.ToLower(strings.TrimSpace(cmnd))
	if strings.HasPrefix(l, "backlog") || strings.HasPrefix(l, "weblog") {
		return nil, &deviceError{ErrBadResponse, "device answered with a WARNING"}
	}
	// Command output over HTTP needs WebLog >= 2. This writes flash, so it
	// happens once per call and is logged.
	c.log.Warn("device web log level too low, setting WebLog 2", "device", int64(t.device))
	if _, err := c.command(ctx, t, "WebLog 2"); err != nil {
		return nil, err
	}
	// Legacy firmware runs the command first and only then warns, so it has
	// been applied already. Only queries may be sent again; repeating a
	// Power TOGGLE or a Restart would undo or double it.
	if v, cerr := cmdpolicy.Classify(cmnd); cerr != nil || v.Class != cmdpolicy.ClassRead {
		return nil, &deviceError{ErrBadResponse, "command was applied but the device could not return its output (web log level raised, check the state)"}
	}
	raw, err = c.command(ctx, t, cmnd)
	if err == nil && webLogWarning(raw) {
		return nil, &deviceError{ErrBadResponse, "device still answers with a WARNING after WebLog 2"}
	}
	return raw, err
}

func (c *httpClient) DownloadConfig(ctx context.Context, t Target) ([]byte, error) {
	return c.get(ctx, t, "/dl", "", downloadTimeout)
}

func (c *httpClient) RootPage(ctx context.Context, t Target) (RootInfo, error) {
	body, err := c.get(ctx, t, "/", "", rootTimeout)
	if err != nil {
		return RootInfo{}, err
	}
	return ParseRootPage(body)
}
