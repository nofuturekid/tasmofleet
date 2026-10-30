package web

import (
	"crypto/rand"
	"crypto/subtle"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const (
	pendingCmdTTL = 10 * time.Minute
	pendingCmdMax = 64
)

// pendingCmd is a command that waits for its confirmation step. The command
// text stays here, in memory, and never goes back into a page: its arguments
// may be a secret, and the confirmation only has to carry this token.
type pendingCmd struct {
	binding string // login session (or header subject) that asked
	cmd     string // canonical text
	ids     []model.DeviceID
	// cutOff: the preview named that the device is reached over MQTT only and the command can cut it
	// off; only then does the confirmation carry transport.WithCutOffConsent.
	cutOff  bool
	expires time.Time
}

type pendingCmds struct {
	mu sync.Mutex
	m  map[string]pendingCmd
}

// put stores the command for binding and returns its single-use token. Expired
// entries go first; when the store is still full the entry that expires first
// makes room (only admins get here).
func (p *pendingCmds) put(now time.Time, binding, cmd string, ids []model.DeviceID) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]pendingCmd{}
	}
	for k, v := range p.m {
		if !now.Before(v.expires) {
			delete(p.m, k)
		}
	}
	for len(p.m) >= pendingCmdMax {
		oldest, first := "", time.Time{}
		for k, v := range p.m {
			if first.IsZero() || v.expires.Before(first) {
				oldest, first = k, v.expires
			}
		}
		delete(p.m, oldest)
	}
	token := rand.Text()
	p.m[token] = pendingCmd{binding: binding, cmd: cmd, ids: ids, expires: now.Add(pendingCmdTTL)}
	return token
}

// putCutOff is put for a command whose preview carried the MQTT-only consequence.
func (p *pendingCmds) putCutOff(now time.Time, binding, cmd string, ids []model.DeviceID) string {
	token := p.put(now, binding, cmd, ids)
	p.mu.Lock()
	defer p.mu.Unlock()
	if v, ok := p.m[token]; ok {
		v.cutOff = true
		p.m[token] = v
	}
	return token
}

// take removes and returns the command if it belongs to binding and is fresh.
// Someone else's token stays where it is.
func (p *pendingCmds) take(now time.Time, token, binding string) (pendingCmd, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.m[token]
	if !ok || subtle.ConstantTimeCompare([]byte(v.binding), []byte(binding)) != 1 {
		return pendingCmd{}, false
	}
	delete(p.m, token)
	if !now.Before(v.expires) {
		return pendingCmd{}, false
	}
	return v, true
}
