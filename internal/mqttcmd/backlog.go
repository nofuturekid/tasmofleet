package mqttcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// collectBacklog gathers one reply per queued command. Tasmota v16 publishes nothing for the
// Backlog itself; the queue is worked off (200 ms apart by default) and every command publishes
// its own RESULT or STATUSn. The replies are merged in queue order, later keys winning, which is
// what /cm returns over HTTP. It ends when every queued command has answered, BacklogQuiet after
// the last reply when some did not (a command without a reply, e.g. Delay), or at BacklogMax.
// Without any reply within ReplyWait it is a timeout: the outcome is unknown.
func (c *Channel) collectBacklog(ctx context.Context, w *waiter, req *request) (json.RawMessage, error) {
	got := make([][]byte, len(req.backlog))
	n := 0
	first := time.NewTimer(c.o.ReplyWait)
	defer first.Stop()
	quiet := time.NewTimer(time.Hour)
	quiet.Stop()
	defer quiet.Stop()
	limit := time.NewTimer(c.o.BacklogMax)
	defer limit.Stop()
	firstC := first.C
	for n < len(got) {
		select {
		case m := <-w.ch:
			i, v := assign(req, got, m)
			if i < 0 {
				continue
			}
			if v.err != nil {
				return nil, fmt.Errorf("mqtt: %s in a Backlog: %w", req.backlog[i].name, v.err)
			}
			got[i] = v.reply
			n++
			firstC = nil
			quiet.Reset(c.o.BacklogQuiet)
		case <-w.off:
			return nil, fmt.Errorf("mqtt: the device went offline during the Backlog (outcome unknown): %w", tasmota.ErrTimeout)
		case <-c.done:
			return nil, c.closedAfterSend("the Backlog")
		case <-firstC:
			return nil, fmt.Errorf("mqtt: no answer to the Backlog: %w", tasmota.ErrTimeout)
		case <-quiet.C:
			return mergeObjects(got), nil
		case <-limit.C:
			if n == 0 {
				return nil, fmt.Errorf("mqtt: no answer to the Backlog: %w", tasmota.ErrTimeout)
			}
			return mergeObjects(got), nil
		case <-ctx.Done():
			return nil, fmt.Errorf("mqtt: the Backlog was sent and not fully answered before the caller gave up (outcome unknown): %w: %w", tasmota.ErrTimeout, ctx.Err())
		}
	}
	return mergeObjects(got), nil
}

// assign gives m to the first queued command that has not answered yet and recognises it.
func assign(req *request, got [][]byte, m mqttbus.Message) (int, verdict) {
	for i, sub := range req.backlog {
		if got[i] != nil {
			continue
		}
		if v, ok := sub.match(m, false); ok {
			return i, v
		}
	}
	return -1, verdict{}
}

// mergeObjects merges JSON objects in order. A key that repeats keeps its first position and its
// last value. Replies that are not objects cannot occur (match only accepts objects or STATUS
// objects), nil entries (no reply) are skipped.
func mergeObjects(parts [][]byte) json.RawMessage {
	var order []string
	vals := map[string]json.RawMessage{}
	for _, p := range parts {
		if p == nil {
			continue
		}
		keys, values, ok := topLevel(p)
		if !ok {
			continue
		}
		for _, k := range keys {
			if _, seen := vals[k]; !seen {
				order = append(order, k)
			}
			vals[k] = values[k]
		}
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range order {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k) // a string cannot fail
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vals[k])
	}
	b.WriteByte('}')
	return b.Bytes()
}
