package ui

import (
	"strconv"
	"strings"
	"time"
)

// StatusBar is what the bar fixed to the bottom of every page shows (#307). It carries counts,
// times and names only: no credentials, no error texts, nothing per device. The live state of the
// browser's own event stream is not in here; live.js sets data-live on <html> and the stylesheet
// shows the matching label.
type StatusBar struct {
	// Broker is the MQTT broker connection; nil when MQTT is off (there is nothing to show).
	Broker *StatusBroker
	// Jobs are the running jobs, at most two; More counts the rest ("+N more").
	Jobs []StatusJob
	More int
	// JobsHref is the transactions list; empty when the user cannot open it (no link then).
	JobsHref string
	// Backup is the last scheduled backup run; nil before the first run since the start.
	Backup *StatusBackup
	// Now is the time the page is built at: "Today 03:00" and "Yesterday 21:10" count from it.
	Now time.Time
	// Fragment is where the bar re-reads itself on events; empty on a page without a stream.
	Fragment string
}

// StatusBroker is the broker connection: host and port only, never the user or the password.
type StatusBroker struct {
	Host      string
	Connected bool
	Since     time.Time // when the state last changed
}

// StatusJob is one running job (a transaction, or the scheduled backup run). Href is empty for a
// job without a page of its own.
type StatusJob struct {
	Kind   string // "Update", "Command", "Backup", "Make compliant"
	Detail string // the command word of a command; empty otherwise
	Done   int
	Total  int
	ID     string // "TX-0143", empty without Href
	Href   string
}

// StatusBackup is the result of the last scheduled backup run.
type StatusBackup struct {
	At     time.Time
	Failed int
	Total  int
}

// statusLevel orders what the phone's one line says: the worst fact wins.
type statusLevel int

const (
	statusOK statusLevel = iota
	statusRun
	statusWarn
	statusBad
)

func (s StatusBar) brokerDown() bool { return s.Broker != nil && !s.Broker.Connected }

func (s StatusBar) level() statusLevel {
	switch {
	case s.Backup != nil && s.Backup.none():
		return statusBad
	case s.brokerDown(), s.Backup != nil && s.Backup.partial():
		return statusWarn
	case len(s.Jobs) > 0:
		return statusRun
	}
	return statusOK
}

// edgeClass colours the thin line on the bar's top edge, so a problem is seen without reading.
func (s StatusBar) edgeClass() string {
	switch s.level() { //nolint:exhaustive // the other levels are the default
	case statusBad:
		return "sb-edge sb-bad"
	case statusWarn:
		return "sb-edge sb-warn"
	}
	return "sb-edge"
}

// dotClass is the phone line's dot.
func (s StatusBar) dotClass() string {
	switch s.level() { //nolint:exhaustive // the other levels are the default
	case statusBad:
		return "sb-dot bad"
	case statusWarn:
		return "sb-dot warn"
	case statusRun:
		return "sb-dot run"
	}
	return "sb-dot"
}

// summary is the phone line's text, and sub the muted rest of it ("All good", "Update 2 of 5 + 1 more").
// A disconnected broker adds "since HH:MM" in the template (a time element).
func (s StatusBar) summary() (text, sub string) {
	switch s.level() { //nolint:exhaustive // the other levels are the default
	case statusBad:
		return "Last backup failed", " · " + s.Backup.countText()
	case statusWarn:
		if s.brokerDown() {
			return "Broker disconnected", ""
		}
		return "Backup partly failed", " · " + s.Backup.countText()
	case statusRun:
		j := s.Jobs[0]
		switch n := len(s.Jobs) + s.More - 1; {
		case n == 1 && len(s.Jobs) == 2: // "Update 2 of 5 + 1 command"
			sub = " + 1 " + strings.ToLower(s.Jobs[1].Kind)
		case n > 0:
			sub = " + " + strconv.Itoa(n) + " more"
		}
		return j.Kind + " " + strconv.Itoa(j.Done) + " of " + strconv.Itoa(j.Total), sub
	}
	return "All good", ""
}

// progress is the share of a job done, for the bar's progress element.
func (j StatusJob) progress() string { return strconv.Itoa(j.Done) + " of " + strconv.Itoa(j.Total) }

// jobText is "Restart 8 of 12" for a command, "2 of 5 devices" for the rest.
func (j StatusJob) jobText() string {
	if j.Detail != "" {
		return j.Detail + " " + j.progress()
	}
	return j.progress() + " devices"
}

// sheetText is the phone sheet's wording: "Restart, 8 of 12 devices", "2 of 5 devices".
func (j StatusJob) sheetText() string {
	if j.Detail != "" {
		return j.Detail + ", " + j.progress() + " devices"
	}
	return j.progress() + " devices"
}

// none is a run that backed up no device at all: the only case that reads "Backup failed" (#406).
func (b StatusBackup) none() bool { return b.Total > 0 && b.Failed >= b.Total }

// partial is a run where some devices were backed up and some were not.
func (b StatusBackup) partial() bool { return b.Failed > 0 && !b.none() }

// countText is "19 of 27 backed up", with " · 8 failed" when some failed.
func (b StatusBackup) countText() string {
	t := strconv.Itoa(b.Total-b.Failed) + " of " + strconv.Itoa(b.Total) + " backed up"
	if b.Failed > 0 {
		t += " · " + strconv.Itoa(b.Failed) + " failed"
	}
	return t
}

// dayText is a time as the bar words it: "Today 03:00", "Yesterday 21:10", else a date.
func (s StatusBar) dayText(t time.Time) string { return StampDayText(s.Now, t) }
