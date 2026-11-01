package ui

import (
	"strings"
	"testing"
	"time"
)

// Why: "Today"/"Yesterday" is a calendar-day decision. The server text is only the fallback
// without JavaScript, but it must agree with time.js on the boundaries: just after midnight
// yesterday evening is still "Yesterday", the minute before midnight two days ago is a date, and
// a time later today or tomorrow is never called "Today" by accident.
func TestStampDayText(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 5, 0, 0, time.UTC) // just after midnight
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	for _, c := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"a minute ago, same day", at(2026, 10, 22, 0, 4), "Today 00:04"},
		{"first minute of today", at(2026, 10, 22, 0, 0), "Today 00:00"},
		{"last minute of yesterday", at(2026, 10, 21, 23, 59), "Yesterday 23:59"},
		{"first minute of yesterday", at(2026, 10, 21, 0, 0), "Yesterday 00:00"},
		{"last minute of the day before", at(2026, 10, 20, 23, 59), "20 Oct, 23:59"},
		{"this year", at(2026, 8, 19, 20, 2), "19 Aug, 20:02"},
		{"an earlier year", at(2025, 8, 19, 20, 2), "19 Aug 2025, 20:02"},
		{"later today", at(2026, 10, 22, 23, 0), "Today 23:00"},
		{"tomorrow is a date, not Today", at(2026, 10, 23, 0, 30), "23 Oct, 00:30"},
	} {
		if got := StampDayText(now, c.t); got != c.want {
			t.Errorf("%s: StampDayText(%s) = %q, want %q", c.name, c.t.Format(time.RFC3339), got, c.want)
		}
	}
}

// Why: the day is the server's calendar day in the zone of "now"; an instant stored in another
// zone must not shift the answer.
func TestStampDayTextUsesNowsZone(t *testing.T) {
	berlin := time.FixedZone("CEST", 2*3600)
	now := time.Date(2026, 10, 22, 0, 30, 0, 0, berlin)    // 21 Oct 22:30 UTC
	utc := time.Date(2026, 10, 21, 22, 10, 0, 0, time.UTC) // 22 Oct 00:10 in Berlin
	if got := StampDayText(now, utc); got != "Today 00:10" {
		t.Errorf("got %q, want the time in now's zone", got)
	}
}

// Why: the attributes are what time.js reads; "day" must be the key, with the instant in UTC.
func TestStampDayAttrs(t *testing.T) {
	at := time.Date(2026, 10, 22, 9, 41, 0, 0, time.FixedZone("x", 7200))
	a := StampAttrs(at, StampDay)
	if a["data-ts"] != "day" || a["datetime"] != "2026-10-22T07:41:00Z" {
		t.Errorf("attrs = %v", a)
	}
	if !strings.Contains(StampLog.Layout, "15:04:05") || StampLog.Key != "clock-24" {
		t.Errorf("StampLog = %+v: the log column is 24-hour with seconds", StampLog)
	}
}

// Why (#422): a list of recent events spans days. Today's entry is read against the clock on the
// wall, so it keeps the bare clock time; an older one with only a clock time reads as if it were
// today (three days ago looked like "12:31"), so it must carry its date.
func TestStampClockOrDayText(t *testing.T) {
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, time.UTC) }
	for _, c := range []struct {
		name string
		t    time.Time
		want string
	}{
		{"earlier today keeps the clock", at(2026, 10, 22, 0, 4), "00:04"},
		{"yesterday evening carries its day", at(2026, 10, 21, 23, 59), "Yesterday 23:59"},
		{"three days ago carries the date", at(2026, 10, 19, 12, 31), "19 Oct, 12:31"},
		{"an earlier year carries the year", at(2025, 10, 19, 12, 31), "19 Oct 2025, 12:31"},
	} {
		if got := StampClockOrDayText(now, c.t); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
