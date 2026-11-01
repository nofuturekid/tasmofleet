package transport

import (
	"context"
	"errors"
	"fmt"

	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// ErrFilesNeedHTTP: the file system of a device cannot be reached over MQTT (Tasmota has no file
// transfer there), and the call was pinned to MQTT. Nothing was sent.
var ErrFilesNeedHTTP = errors.New("files need HTTP")

// MQTTOnly reports whether every call recorded in ctx's Trace so far went over MQTT, i.e. the device
// is reached over MQTT only. False without a Trace or before any call.
func MQTTOnly(ctx context.Context) bool {
	t, _ := ctx.Value(keyTrace).(*Trace)
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mqtt && !t.http
}

var _ tasmota.FileSystem = (*Router)(nil)

// fsHTTP returns the observed HTTP client as a FileSystem, or the error that says why it is none.
// Files travel over HTTP only: a call pinned to MQTT is refused before anything is sent.
func (r *Router) fsHTTP(ctx context.Context) (tasmota.FileSystem, error) {
	if p, ok := PathFrom(ctx); ok && p == MQTT {
		noteTrace(ctx, None)
		return nil, ErrFilesNeedHTTP
	}
	fs, ok := r.http.(tasmota.FileSystem)
	if !ok {
		return nil, fmt.Errorf("%w: the HTTP client has no file system access", tasmota.ErrNotImplemented)
	}
	return fs, nil
}

func traceHTTP(ctx context.Context, err error) {
	if tasmota.NotSent(err) {
		noteTrace(ctx, None)
	} else {
		noteTrace(ctx, HTTP)
	}
}

// FSInfo implements tasmota.FileSystem over HTTP.
func (r *Router) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	fs, err := r.fsHTTP(ctx)
	if err != nil {
		return tasmota.FSInfo{}, err
	}
	i, err := fs.FSInfo(ctx, t)
	traceHTTP(ctx, err)
	return i, err
}

// ListFiles implements tasmota.FileSystem over HTTP.
func (r *Router) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	fs, err := r.fsHTTP(ctx)
	if err != nil {
		return nil, err
	}
	e, err := fs.ListFiles(ctx, t, dir)
	traceHTTP(ctx, err)
	return e, err
}

// DownloadFile implements tasmota.FileSystem over HTTP.
func (r *Router) DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error) {
	fs, err := r.fsHTTP(ctx)
	if err != nil {
		return nil, err
	}
	b, err := fs.DownloadFile(ctx, t, path, max)
	traceHTTP(ctx, err)
	return b, err
}

// UploadFile implements tasmota.FileSystem over HTTP.
func (r *Router) UploadFile(ctx context.Context, t tasmota.Target, path string, data []byte) error {
	fs, err := r.fsHTTP(ctx)
	if err != nil {
		return err
	}
	err = fs.UploadFile(ctx, t, path, data)
	traceHTTP(ctx, err)
	return err
}

// ---- the observer passes file calls through and learns from them

var _ tasmota.FileSystem = (*observer)(nil)

func (o *observer) fs() (tasmota.FileSystem, error) {
	fs, ok := o.c.(tasmota.FileSystem)
	if !ok {
		return nil, fmt.Errorf("%w: the HTTP client has no file system access", tasmota.ErrNotImplemented)
	}
	return fs, nil
}

func (o *observer) FSInfo(ctx context.Context, t tasmota.Target) (tasmota.FSInfo, error) {
	fs, err := o.fs()
	if err != nil {
		return tasmota.FSInfo{}, err
	}
	i, err := fs.FSInfo(ctx, t)
	o.see(t, err)
	return i, err
}

func (o *observer) ListFiles(ctx context.Context, t tasmota.Target, dir string) ([]tasmota.FileEntry, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	e, err := fs.ListFiles(ctx, t, dir)
	o.see(t, err)
	return e, err
}

func (o *observer) DownloadFile(ctx context.Context, t tasmota.Target, path string, max int64) ([]byte, error) {
	fs, err := o.fs()
	if err != nil {
		return nil, err
	}
	b, err := fs.DownloadFile(ctx, t, path, max)
	o.see(t, err)
	return b, err
}

func (o *observer) UploadFile(ctx context.Context, t tasmota.Target, path string, data []byte) error {
	fs, err := o.fs()
	if err != nil {
		return err
	}
	err = fs.UploadFile(ctx, t, path, data)
	o.see(t, err)
	return err
}
