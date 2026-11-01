package tasmota

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
)

// ConfigRestorer is implemented by the production client next to Client. It is a separate
// interface so the many fakes of Client stay unchanged.
type ConfigRestorer interface {
	// RestoreConfig does what the web UI's "Restore configuration" does (#430): GET /rs, which sets
	// the device's upload type to settings, then a multipart POST of dump to /u2, both in one
	// device slot so no other request of this client comes between them. A nil error means the
	// device accepted the dump and restarts with it.
	//
	// Errors: ErrUploadRefused (the device answered its "Failed" page: nothing was applied, the
	// device keeps running) and every error that matches ErrNotSent (the POST was never sent, for
	// example a device without /rs, ErrUploadUnsupported, or a refused login) are certain. Any other
	// error (timeout, lost connection, a reply that is neither page) leaves it open whether the
	// device applied the dump: the caller must look at the device and never send it again.
	RestoreConfig(ctx context.Context, t Target, dump []byte) error
}

var _ ConfigRestorer = (*httpClient)(nil)

// restoreReloadScript is HTTP_SCRIPT_RELOAD_TIME, which HandleUploadDone writes only on success
// (v16 xdrv_01_9_webserver.ino 120, 3487-3490). Unlike "Successful", it is the same in every
// language build.
const restoreReloadScript = "location.href='.'"

func (c *httpClient) RestoreConfig(ctx context.Context, t Target, dump []byte) error {
	if len(dump) == 0 {
		return &notSentError{&deviceError{ErrBadResponse, "empty configuration dump"}}
	}
	// One slot for both requests: the upload type is global device state.
	release, err := c.acquire(ctx, t.hostPort())
	if err != nil {
		return &notSentError{err}
	}
	defer release()
	rctx, cancel := context.WithTimeout(ctx, c.timeout(uploadTimeout))
	defer cancel()

	// The page is the one guard against the trap of an unset upload type: a POST /u2 without it
	// answers "Successful" and restarts the device with nothing applied. The minimal firmware has
	// no /rs (404).
	pctx, pcancel := context.WithTimeout(rctx, c.timeout(rootTimeout))
	page, err := c.fetch(pctx, ctx, t, http.MethodGet, "/rs", "", nil, "", 0)
	pcancel()
	if err != nil {
		if errors.Is(err, ErrBadResponse) && strings.HasSuffix(err.Error(), "HTTP 404") {
			err = &deviceError{ErrUploadUnsupported, "the device has no restore page (minimal firmware?)"}
		}
		return &notSentError{err}
	}
	if !upUploadForm.Match(page) {
		return &notSentError{&deviceError{ErrUploadUnsupported, "the restore page has no file upload form"}}
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("u2", "Config.dmp")
	if err != nil {
		return &notSentError{&deviceError{ErrBadResponse, "invalid multipart body"}}
	}
	if _, err := fw.Write(dump); err != nil {
		return &notSentError{&deviceError{ErrBadResponse, "invalid multipart body"}}
	}
	if err := mw.Close(); err != nil {
		return &notSentError{&deviceError{ErrBadResponse, "invalid multipart body"}}
	}
	// Tasmota's web server cannot read chunked bodies: the length is exact.
	reply, err := c.fetch(rctx, ctx, t, http.MethodPost, "/u2", "fsz="+strconv.Itoa(len(dump)),
		bytes.NewReader(body.Bytes()), mw.FormDataContentType(), int64(body.Len()))
	if err != nil {
		return err
	}
	err = restoreReply(string(reply))
	var de *deviceError
	if errors.As(err, &de) {
		de.msg = t.redact(de.msg) // the reason is device text
	}
	return err
}

// restoreReply reads HandleUploadDone's page: the success page carries the reload script, the
// failure page the coloured heading and the reason, and nothing else is an answer to the upload.
func restoreReply(page string) error {
	result := strings.Contains(page, "<font color='#")
	switch {
	case result && strings.Contains(page, restoreReloadScript):
		return nil
	case result:
		return &deviceError{ErrUploadRefused, uploadReason(page)}
	}
	return &deviceError{ErrBadResponse, "restore reply is neither the success nor the failure page"}
}

// uploadReason is the text after the failure heading, in any language.
func uploadReason(page string) string {
	_, after, ok := strings.Cut(page, "</font></b><br><br>")
	if !ok {
		return "no reason given"
	}
	s := strings.TrimSpace(htmlTag.ReplaceAllString(after, " "))
	if i := strings.Index(s, "  "); i > 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	if s == "" {
		return "no reason given"
	}
	return s
}
