package mqttcmd

import (
	"time"

	"github.com/nofuturekid/tasmofleet/internal/mqttbus"
)

// maxInfoBytes bounds what is kept per INFO message (a real one is a few hundred bytes).
const maxInfoBytes = 4096

// Info is what a device announced at its latest boot: INFO1 (module, version, topics) and INFO3
// (restart reason). Tasmota publishes them once per boot, so they exist only for a device seen
// booting while we listened.
type Info struct {
	Info1, Info3 []byte
	At           time.Time // of the latest message
}

// Info returns the INFO1/INFO3 seen since the device's last LWT Offline. A software restart
// publishes Offline, so an INFO1 from before a reboot (the old version, the old image) is never
// returned: minimal-firmware detection must not rest on it.
func (c *Channel) Info(topic string) (Info, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := c.info[topic]
	if i == nil {
		return Info{}, false
	}
	return Info{Info1: append([]byte(nil), i.Info1...), Info3: append([]byte(nil), i.Info3...), At: i.At}, true
}

func (c *Channel) onInfo(m mqttbus.Message) {
	if m.Retained || len(m.Payload) > maxInfoBytes || (m.Suffix != "INFO1" && m.Suffix != "INFO3") {
		return
	}
	c.mu.Lock()
	_, have := c.info[m.Topic]
	c.mu.Unlock()
	if !have && c.o.Known != nil && !c.o.Known(m) { // outside the lock: may consult the inventory
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !have && len(c.info) >= maxTopics {
		return
	}
	if m.Suffix == "INFO3" {
		c.bootsSeen[m.Topic]++
	}
	i := c.info[m.Topic]
	if i == nil {
		i = &Info{}
		c.info[m.Topic] = i
	}
	if m.Suffix == "INFO1" {
		i.Info1 = append([]byte(nil), m.Payload...)
	} else {
		i.Info3 = append([]byte(nil), m.Payload...)
	}
	i.At = m.At
}
