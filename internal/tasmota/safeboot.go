package tasmota

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Reading the upload layout and switching the boot partition are the two
// device actions behind "push into safeboot" (#30). Both are documented in
// docs/research/tasmota-minimal-and-ota.md ("Telling the layouts apart").

// UpLayout says what the /up page of a device offers for a file upload.
type UpLayout string

// Layouts.
const (
	// UpLayoutDirect: the plain upload form (HTTP_FORM_RST_UPG). The device
	// takes an image into its running partition: an ESP8266, an ESP32 with two
	// app partitions, or an ESP32 that already runs safeboot.
	UpLayoutDirect UpLayout = "direct"
	// UpLayoutSafeboot: the form whose button calls upl(this)
	// (HTTP_FORM_RST_UPG_FCT). The normal firmware runs on a single-OTA
	// (safeboot) layout; an upload needs the restart into safeboot first.
	UpLayoutSafeboot UpLayout = "safeboot"
)

// The exact markers of the two upload forms in Tasmota v16.0.0 (xdrv_01_9_webserver.ino:367 and :382). The
// upload button of the plain form (HTTP_FORM_RST_UPG: an ESP8266, an ESP32 with two app partitions, or an
// ESP32 that runs safeboot itself) submits the form itself; the button of HTTP_FORM_RST_UPG_FCT (a single-OTA
// layout whose normal firmware runs) returns upl(this). The script that defines upl(t) is on every ESP32
// page, so only the call counts.
const (
	plainSubmit = "this.form.submit();"
	uplReturn   = "return upl(this);"
	uplCall     = "upl(this)"
)

// ErrUpLayoutAmbiguous: the /up page has an upload form but not exactly the markers of one of the two
// forms (both, neither, a second form, a changed button). Nothing may be concluded from it.
var ErrUpLayoutAmbiguous = errors.New("tasmota: the upgrade page matches neither upload form exactly")

var (
	upFormAction = regexp.MustCompile(`(?i)action=['"]u2\?fsz=['"]`)
	upFileInput  = regexp.MustCompile(`(?i)name=['"]u2['"]`)
)

// ParseUpLayout reads the layout from the /up page, and is the only place that does. It is strict: the
// plain form needs its submit marker and no upl(this) call at all, the safeboot form needs its
// return upl(this) and no plain submit, each exactly once, and there must be exactly one upload form.
// A page without the upload form is ErrUploadUnsupported; anything else that does not match one form
// exactly is ErrUpLayoutAmbiguous (the layout is then unknown, never guessed).
func ParseUpLayout(page []byte) (UpLayout, error) {
	p := string(page)
	forms, inputs := len(upFormAction.FindAllStringIndex(p, -1)), len(upFileInput.FindAllStringIndex(p, -1))
	if forms == 0 || !upUploadForm.MatchString(p) {
		return "", &deviceError{ErrUploadUnsupported, "the upgrade page has no file upload form"}
	}
	if forms != 1 || inputs != 1 {
		return "", &deviceError{ErrUpLayoutAmbiguous, "the upgrade page has more than one upload form"}
	}
	submit, ret, calls := strings.Count(p, plainSubmit), strings.Count(p, uplReturn), strings.Count(p, uplCall)
	switch {
	case ret == 1 && calls == 1 && submit == 0:
		return UpLayoutSafeboot, nil
	case submit == 1 && calls == 0 && ret == 0:
		return UpLayoutDirect, nil
	}
	return "", &deviceError{ErrUpLayoutAmbiguous, "the upgrade page has neither upload form exactly"}
}

// UpPage is what one GET of /up shows: the upload form's layout and who serves the page. A single-OTA ESP32
// that runs its safeboot partition serves the PLAIN form too (xdrv_01_9_webserver.ino:3420), so the form
// alone cannot prove two app partitions: the footer's image name and the SAFEBOOT banner say what runs.
type UpPage struct {
	Layout UpLayout
	// Image is the image name of the page footer ("release-tasmota32"), "" when the footer is missing or
	// has another shape.
	Image string
	// SafebootBanner: the page carries the <h3>SAFEBOOT</h3> banner of the safeboot firmware.
	SafebootBanner bool
}

const safebootBanner = "<h3>SAFEBOOT</h3>"

// ParseUpPage is ParseUpLayout plus the footer image and the banner.
func ParseUpPage(page []byte) (UpPage, error) {
	l, err := ParseUpLayout(page)
	if err != nil {
		return UpPage{}, err
	}
	p := UpPage{Layout: l, SafebootBanner: strings.Contains(string(page), safebootBanner)}
	if m := rootFooter.FindAllStringSubmatch(string(page), -1); len(m) == 1 {
		p.Image = m[0][2]
	}
	return p, nil
}

// ProvesDualOTA reports whether the page proves that the NORMAL firmware runs on two app partitions:
// the plain form, a footer with an image that is neither safeboot nor minimal, and no SAFEBOOT banner.
// Anything missing or ambiguous is no proof.
func (p UpPage) ProvesDualOTA() bool {
	img := strings.ToLower(p.Image)
	return p.Layout == UpLayoutDirect && img != "" && !strings.Contains(img, "safeboot") && !strings.Contains(img, "minimal") && !p.SafebootBanner
}

// UpPageReader is implemented by the production client next to BootSwitcher.
type UpPageReader interface {
	// ReadUpPage is a GET of /up: read-only, it changes nothing.
	ReadUpPage(ctx context.Context, t Target) (UpPage, error)
}

var _ UpPageReader = (*httpClient)(nil)

// BootTarget is the partition /u4 switches to.
type BootTarget string

// Boot targets.
const (
	BootSafeboot BootTarget = "fct" // restart into the safeboot (factory) partition
	BootApp      BootTarget = "ota" // restart back into the app partition
)

// BootReply is the plain-text answer of /u4 in api mode.
type BootReply string

// Replies.
const (
	// BootRestarting ("false"): the device is not on the target and restarts
	// into it now. The caller polls; it never sends the switch again.
	BootRestarting BootReply = "false"
	// BootAlready ("true"): the device is on a single-OTA layout and nothing
	// was done (fct while in safeboot, ota while in the app).
	BootAlready BootReply = "true"
	// BootNone ("none"): the device has no safeboot partition; nothing was done.
	BootNone BootReply = "none"
)

// ErrSwitchNotDone: the device answered, but with something other than the
// switch: nothing was changed (an error status, or a firmware without /u4).
// Every other error of SwitchBoot leaves open whether the device restarts.
var ErrSwitchNotDone = errors.New("tasmota: boot partition not switched")

// BootSwitcher is implemented by the production client next to Client, like
// Uploader.
type BootSwitcher interface {
	// ReadUpLayout is a GET of /up: read-only, it changes nothing.
	ReadUpLayout(ctx context.Context, t Target) (UpLayout, error)
	// SwitchBoot sends GET /u4?u4=<to>&api= exactly once. BootRestarting
	// means the device restarts. A transport error (timeout, lost reply) leaves
	// it open whether it does: the caller probes, never sends it again.
	// ErrSwitchNotDone and ErrAuth are certain: nothing changed.
	SwitchBoot(ctx context.Context, t Target, to BootTarget) (BootReply, error)
}

var _ BootSwitcher = (*httpClient)(nil)

func (c *httpClient) ReadUpLayout(ctx context.Context, t Target) (UpLayout, error) {
	p, err := c.ReadUpPage(ctx, t)
	return p.Layout, err
}

func (c *httpClient) ReadUpPage(ctx context.Context, t Target) (UpPage, error) {
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return UpPage{}, err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(rootTimeout))
	defer cancel()
	page, err := c.fetch(rctx, ctx, t, http.MethodGet, "/up", "", nil, "", 0)
	if err != nil {
		return UpPage{}, err
	}
	return ParseUpPage(page)
}

func (c *httpClient) SwitchBoot(ctx context.Context, t Target, to BootTarget) (BootReply, error) {
	if to != BootSafeboot && to != BootApp {
		return "", &deviceError{ErrBadResponse, "unknown boot target"}
	}
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return "", err
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(rootTimeout))
	defer cancel()
	reply, err := c.fetch(rctx, ctx, t, http.MethodGet, "/u4", "u4="+url.QueryEscape(string(to))+"&api=", nil, "", 0)
	if err != nil {
		// An HTTP status other than 200 came from a device that did not act on
		// the request (fetch words those "HTTP <code>").
		var de *deviceError
		if errors.As(err, &de) && errors.Is(de.kind, ErrBadResponse) && strings.HasPrefix(de.msg, "HTTP ") {
			return "", &deviceError{ErrSwitchNotDone, de.msg}
		}
		return "", err
	}
	switch r := BootReply(strings.TrimSpace(string(reply))); r {
	case BootRestarting, BootAlready, BootNone:
		return r, nil
	}
	// Neither of the three words: the request may still have been acted on.
	return "", &deviceError{ErrBadResponse, "unexpected answer to the boot partition switch"}
}
