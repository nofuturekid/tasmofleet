package mqttcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

// Defaults of Options (D3 of the 0.20.0 plan; live replies took 40-100 ms).
const (
	DefaultGap          = 100 * time.Millisecond
	DefaultReplyWait    = 8 * time.Second
	DefaultBacklogQuiet = 600 * time.Millisecond
	DefaultBacklogMax   = 10 * time.Second
	DefaultFileTotal    = 30 * time.Second

	maxTopics = 1024 // bound of the LWT and INFO tables; known devices are far fewer
	waitDepth = 32   // messages kept for one waiting command; more are dropped
)

// defaultGroupTopic is Tasmota's default GroupTopic: a command to it reaches every device that kept it.
const defaultGroupTopic = "tasmotas"

// ErrRefused marks a command the channel will not publish. Nothing was sent.
var ErrRefused = errors.New("mqttcmd: command refused")

// Doer is what consumers (devicecmd, identity, later the transport router) depend on.
type Doer interface {
	// Do sends cmd to d over MQTT and returns the matching reply.
	Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error)
	// Connected reports whether the broker connection is usable for a command right now.
	Connected() bool
}

// Options tune the channel. Zero values select the defaults.
type Options struct {
	Gap          time.Duration // between two publishes on the whole bus
	ReplyWait    time.Duration // for the reply to one command
	BacklogQuiet time.Duration // after the last collected Backlog reply
	BacklogMax   time.Duration // for a whole Backlog
	FileTotal    time.Duration // for a whole FileDownload (ReplyWait applies to each message)
	// Known, if set, limits the LWT and INFO tables to inventory devices.
	Known func(mqttbus.Message) bool
}

// Channel implements Doer on a bus.
type Channel struct {
	bus   mqttbus.Bus
	o     Options
	unsub func()
	done  chan struct{} // closed by Close: wakes every command in flight
	once  sync.Once

	mu      sync.Mutex
	lwt     map[string]string // topic -> "Online" | "Offline"
	info    map[string]*Info
	sems    map[string]chan struct{}
	waiters map[string]*waiter
	nextPub time.Time

	files     map[string]chan mqttbus.Message // topic -> stat/<t>/FILEDOWNLOAD|FILEUPLOAD of the transfer in flight
	fileClean map[string]bool                 // topic -> the last download ended with a matching MD5
	reboots   map[string]int                  // topic -> LWT Offline->Online pairs seen (bounded by lwt)
	fwPending map[string]int                  // topic -> reboots when a Type 1 upload ended unconfirmed
}

// waiter receives the live replies of the one command in flight on a topic.
type waiter struct {
	ch  chan mqttbus.Message
	off chan struct{} // LWT Offline arrived
}

// New creates the channel and subscribes before it returns, so the first Do cannot miss a reply.
func New(bus mqttbus.Bus, o Options) *Channel {
	if o.Gap == 0 {
		o.Gap = DefaultGap
	}
	if o.ReplyWait == 0 {
		o.ReplyWait = DefaultReplyWait
	}
	if o.BacklogQuiet == 0 {
		o.BacklogQuiet = DefaultBacklogQuiet
	}
	if o.BacklogMax == 0 {
		o.BacklogMax = DefaultBacklogMax
	}
	if o.FileTotal == 0 {
		o.FileTotal = DefaultFileTotal
	}
	c := &Channel{bus: bus, o: o, lwt: map[string]string{}, info: map[string]*Info{}, sems: map[string]chan struct{}{}, waiters: map[string]*waiter{}, files: map[string]chan mqttbus.Message{}, fileClean: map[string]bool{}, reboots: map[string]int{}, fwPending: map[string]int{}, done: make(chan struct{})}
	c.unsub = bus.Subscribe([]mqttbus.Kind{mqttbus.KindLWT, mqttbus.KindInfo, mqttbus.KindResult, mqttbus.KindStatus, mqttbus.KindPower, mqttbus.KindFile}, c.onMessage)
	return c
}

// Close stops listening and wakes every command in flight with ErrUnreachable.
func (c *Channel) Close() {
	c.once.Do(func() {
		c.unsub()
		close(c.done)
	})
}

// closed is the error of a command that was not published.
func (c *Channel) closed() error {
	return fmt.Errorf("%w: the MQTT command channel is closed", tasmota.ErrUnreachable)
}

// closedAfterSend is the error of a command that was published and whose answer the shutdown cut
// off: the device may have run it, so it is an unknown outcome (ErrTimeout), never "not sent".
func (c *Channel) closedAfterSend(name string) error {
	return fmt.Errorf("mqtt: %s was sent and the command channel closed before it was answered (outcome unknown): %w", name, tasmota.ErrTimeout)
}

// Connected reports whether the bus is connected and subscribed.
func (c *Channel) Connected() bool { return c.bus.Connected() }

// Do sends cmd and returns the reply that belongs to it (see the package comment). Automated
// reads and writes use this; a reply without a key that names the command is not accepted.
func (c *Channel) Do(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return c.do(ctx, d, cmd, false)
}

// DoLoose is Do for the console: a command whose reply carries other keys than its name
// (Template, Gpios; checked against v16 source, docs/research/tasmota-mqtt-transport.md section 8)
// takes the first RESULT. Every other command is matched exactly as in Do.
func (c *Channel) DoLoose(ctx context.Context, d model.Device, cmd string) (json.RawMessage, error) {
	return c.do(ctx, d, cmd, true)
}

func (c *Channel) do(ctx context.Context, d model.Device, cmd string, loose bool) (json.RawMessage, error) {
	req, err := parseRequest(cmd)
	if err != nil {
		return nil, err
	}
	if err := checkTopic(d); err != nil {
		return nil, err
	}
	if !c.bus.Connected() {
		return nil, fmt.Errorf("%w: %w", tasmota.ErrUnreachable, mqttbus.ErrNotConnected)
	}
	select {
	case <-c.done: // closed: a ready semaphore must not win the race below
		return nil, c.closed()
	default:
	}
	sem := c.sem(d.Topic)
	select {
	case sem <- struct{}{}: // one command in flight per topic; blocked callers are served in order
	case <-ctx.Done():
		return nil, ctx.Err() // still queued: nothing was sent
	case <-c.done:
		return nil, c.closed()
	}
	release := func() { <-sem }

	if c.offline(d.Topic) { // checked after the wait: the device may have gone while we queued
		release()
		return nil, fmt.Errorf("%w: the device reported itself offline (LWT), nothing was sent", tasmota.ErrUnreachable)
	}
	if err := c.waitTurn(ctx); err != nil {
		release()
		return nil, err
	}
	w := &waiter{ch: make(chan mqttbus.Message, waitDepth), off: make(chan struct{}, 1)}
	c.setWaiter(d.Topic, w)
	if err := c.bus.Command(ctx, d.Topic, req.name, req.args); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The publish was attempted: the command may have reached the device.
			c.holdUntilLate(d.Topic, w, req, release)
			return nil, fmt.Errorf("%w: %s may have been sent: %w", tasmota.ErrTimeout, req.name, err)
		}
		c.setWaiter(d.Topic, nil)
		release()
		return nil, fmt.Errorf("%w: %s not sent: %w", tasmota.ErrUnreachable, req.name, err)
	}
	var raw json.RawMessage
	if req.backlog != nil {
		raw, err = c.collectBacklog(ctx, w, req)
	} else {
		raw, err = c.collect(ctx, w, req, loose)
	}
	if errors.Is(err, tasmota.ErrTimeout) {
		// The outcome is unknown and the reply may still come. Keep the topic and the listener for a
		// grace period and drop that reply, or it would answer the next command with the same key.
		c.holdUntilLate(d.Topic, w, req, release)
		return nil, err
	}
	c.setWaiter(d.Topic, nil)
	release()
	if err != nil {
		return nil, err
	}
	if req.redact {
		raw = cmdpolicy.ScrubReply(raw)
	}
	return raw, nil
}

// holdUntilLate keeps the topic busy and the old listener registered until a reply that matches
// the abandoned request arrives (and is dropped), the device goes offline, or ReplyWait has
// passed again. It returns at once; the topic is released by a goroutine.
func (c *Channel) holdUntilLate(topic string, w *waiter, req *request, release func()) {
	go func() {
		defer release()
		defer c.setWaiter(topic, nil)
		timer := time.NewTimer(c.o.ReplyWait)
		defer timer.Stop()
		for {
			select {
			case m := <-w.ch:
				if req.matchesAny(m) {
					return // the late reply, dropped
				}
			case <-w.off:
				return
			case <-timer.C:
				return
			case <-c.done:
				return
			}
		}
	}()
}

// collect waits for the one reply of a single command.
func (c *Channel) collect(ctx context.Context, w *waiter, req *request, loose bool) (json.RawMessage, error) {
	timer := time.NewTimer(c.o.ReplyWait)
	defer timer.Stop()
	for {
		select {
		case m := <-w.ch:
			v, ok := req.match(m, loose)
			if !ok {
				continue
			}
			if v.err != nil {
				return nil, v.err
			}
			return v.reply, nil
		case <-w.off:
			return nil, fmt.Errorf("mqtt: the device went offline before it answered %s (outcome unknown): %w", req.name, tasmota.ErrTimeout)
		case <-timer.C:
			return nil, fmt.Errorf("mqtt: no answer to %s: %w", req.name, tasmota.ErrTimeout)
		case <-ctx.Done():
			return nil, fmt.Errorf("mqtt: %s was sent and not answered before the caller gave up (outcome unknown): %w: %w", req.name, tasmota.ErrTimeout, ctx.Err())
		case <-c.done:
			return nil, c.closedAfterSend(req.name)
		}
	}
}

func checkTopic(d model.Device) error {
	switch {
	case d.Topic == "":
		return fmt.Errorf("%w: the device has no MQTT topic", ErrRefused)
	case strings.ContainsAny(d.Topic, "/+#") || strings.TrimSpace(d.Topic) != d.Topic:
		return fmt.Errorf("%w: the topic contains characters that are not allowed", ErrRefused)
	case d.Topic == defaultGroupTopic || (d.GroupTopic != "" && d.Topic == d.GroupTopic):
		return fmt.Errorf("%w: the topic is the group topic, which would reach many devices", ErrRefused)
	}
	return nil
}

func (c *Channel) sem(topic string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.sems[topic]
	if s == nil {
		s = make(chan struct{}, 1)
		c.sems[topic] = s
	}
	return s
}

func (c *Channel) setWaiter(topic string, w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w == nil {
		delete(c.waiters, topic)
		return
	}
	c.waiters[topic] = w
}

func (c *Channel) offline(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lwt[topic] == "Offline"
}

// waitTurn keeps Gap between two publishes on the whole bus.
func (c *Channel) waitTurn(ctx context.Context) error {
	now := time.Now()
	c.mu.Lock()
	turn := now
	if c.nextPub.After(turn) {
		turn = c.nextPub
	}
	c.nextPub = turn.Add(c.o.Gap)
	c.mu.Unlock()
	wait := turn.Sub(now)
	if wait <= 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closed() // not published yet
	}
}

// onMessage runs on the bus goroutine and never blocks.
func (c *Channel) onMessage(m mqttbus.Message) {
	switch m.Kind {
	case mqttbus.KindLWT:
		c.onLWT(m)
	case mqttbus.KindInfo:
		c.onInfo(m)
	case mqttbus.KindResult, mqttbus.KindStatus, mqttbus.KindPower:
		if m.Retained {
			return // old news, never an answer
		}
		c.mu.Lock()
		w := c.waiters[m.Topic]
		c.mu.Unlock()
		if w != nil {
			select {
			case w.ch <- m:
			default: // a flood must not block the bus; the real reply is lost, the caller times out
			}
		}
	case mqttbus.KindFile:
		c.onFile(m)
	case mqttbus.KindState, mqttbus.KindSensor, mqttbus.KindDiscovery, mqttbus.KindUpgrade:
		// not subscribed
	}
}

func (c *Channel) onLWT(m mqttbus.Message) {
	state := string(m.Payload)
	if state != "Online" && state != "Offline" {
		return
	}
	c.mu.Lock()
	_, have := c.lwt[m.Topic]
	c.mu.Unlock()
	if !have && c.o.Known != nil && !c.o.Known(m) { // outside the lock: may consult the inventory
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !have && len(c.lwt) >= maxTopics {
		return
	}
	if state == "Online" && c.lwt[m.Topic] == "Offline" {
		c.reboots[m.Topic]++ // a reboot: Offline (the will) followed by Online
	}
	c.lwt[m.Topic] = state
	if m.Retained {
		// Replayed right after we subscribed: we may have missed a reboot, so what the device
		// announced before is of unknown age.
		delete(c.info, m.Topic)
	}
	if state == "Offline" {
		// Every software restart publishes Offline (the will, no DISCONNECT): what the device
		// announced before it is stale, and a command waiting for its answer lost it.
		delete(c.info, m.Topic)
		if w := c.waiters[m.Topic]; w != nil {
			select {
			case w.off <- struct{}{}:
			default:
			}
		}
	}
}
