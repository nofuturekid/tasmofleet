package ui

import (
	"time"

	"github.com/a-h/templ"
)

// StampFmt is one way to show a point in time. Layout is the server-side fallback (the server's
// zone); Key names the Intl.DateTimeFormat options static/time.js uses to show the same instant in
// the browser's locale and time zone. The two stay close on purpose: same fields, same precision.
type StampFmt struct{ Key, Layout string }

var (
	StampDateTimeSec = StampFmt{"datetime-s", "2006-01-02 15:04:05"}
	StampDateTime    = StampFmt{"datetime", "2006-01-02 15:04"}
	StampClockSec    = StampFmt{"time-s", "15:04:05"}
	StampClock       = StampFmt{"time", "15:04"}
	StampShort       = StampFmt{"short", "2 Jan 2006, 15:04"}
	// StampStore is the storage table's cell: time.js picks "23 Oct, 05:45" or "23 Oct 2025" itself
	// (the server's text, storeDate, is the fallback and has no fixed layout).
	StampStore = StampFmt{"store", ""}
	// StampDay is a relative day: "Today 09:41", "Yesterday 21:10", else "19 Aug, 20:02" (with the
	// year when it is not this year). time.js decides today and yesterday in the browser's zone and
	// repaints across midnight; the fallback text comes from StampDayText, not from a Layout.
	StampDay = StampFmt{"day", ""}
	// StampClockOrDay is the stamp of a list that spans days (#422): today's entry is the bare clock time,
	// any other day is what StampDay writes ("Yesterday 21:10", "19 Aug, 20:02"). The fallback text comes
	// from StampClockOrDayText.
	StampClockOrDay = StampFmt{"clock-day", ""}
	// StampLog is a log column: 24-hour with seconds in any locale (time.js "clock-24").
	StampLog = StampFmt{"clock-24", "15:04:05"}
	// StampTitle changes only the title of the element (full date, time and zone), not its text:
	// for a <time> whose text is relative ("5 min ago") or already worded.
	StampTitle = StampFmt{"title", "2006-01-02 15:04:05 MST"}
)

// StampText is the fallback text of a timestamp: what the page shows without JavaScript. It is
// in the server's zone (t.Local()), never in the zone the store happens to hand the time out in.
func StampText(t time.Time, f StampFmt) string { return t.Local().Format(f.Layout) }

// StampAttrs are the attributes of a <time> element that time.js rewrites in the browser's locale
// and zone: datetime (RFC 3339, the instant) and data-ts (the format key). Spread them:
// <time { ui.StampAttrs(t, ui.StampClock)... }>{ ui.StampText(t, ui.StampClock) }</time>.
func StampAttrs(t time.Time, f StampFmt) templ.Attributes {
	return templ.Attributes{"datetime": t.UTC().Format(time.RFC3339), "data-ts": f.Key}
}

// StampISO is the instant as RFC 3339 in UTC, for data attributes that time.js reads.
func StampISO(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// StampDayText is the fallback text of a StampDay: t as a calendar day relative to now, in now's
// zone. Today and Yesterday carry the time; other days are a date (and time), with the year only
// when it is not now's. A later day (an expiry) is a date, never "Today".
func StampDayText(now, t time.Time) string {
	t = t.In(now.Location())
	const day = "2006-01-02"
	switch t.Format(day) {
	case now.Format(day):
		return "Today " + t.Format("15:04")
	case now.AddDate(0, 0, -1).Format(day):
		return "Yesterday " + t.Format("15:04")
	}
	if t.Year() == now.Year() {
		return t.Format("2 Jan, 15:04")
	}
	return t.Format("2 Jan 2006, 15:04")
}

// StampClockOrDayText is the fallback text of a StampClockOrDay: the clock time for today, else
// StampDayText. Same boundaries as time.js.
func StampClockOrDayText(now, t time.Time) string {
	const day = "2006-01-02"
	if t = t.In(now.Location()); t.Format(day) == now.Format(day) {
		return t.Format("15:04")
	}
	return StampDayText(now, t)
}
