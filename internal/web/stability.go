package web

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/stability"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The Stability card of the device Overview, the badge on the device list and the two reasons in the
// dashboard's "Needs attention" (#325). The verdict is internal/stability's; this file words it.

// stabilityRecent is how many restarts the card lists.
const stabilityRecent = 5

// annotateStability gives every online row its verdict, from two reads for the whole fleet (the
// restarts of the last 7 days, the Wi-Fi samples of the last 24 h). Without the store, or when it
// fails, rows stay without one: a missing history is never a problem.
func (h *handler) annotateStability(ctx context.Context, rows []deviceRow) {
	if h.d.Stability == nil {
		return
	}
	now := h.d.Clock.Now()
	boots, err := h.d.Stability.BootsSince(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		h.log.WarnContext(ctx, "stability: reading restarts", "err", err)
		return
	}
	wifi, err := h.d.Stability.WifiSamplesSince(ctx, now.Add(-24*time.Hour))
	if err != nil {
		h.log.WarnContext(ctx, "stability: reading Wi-Fi samples", "err", err)
		return
	}
	for i := range rows {
		if rows[i].Health.State != model.HealthOnline {
			continue // an offline device's signal is old news, and "Offline" says more
		}
		id := rows[i].Device.ID
		res := stability.Assess(stability.Input{Now: now, Boots: boots[id], Samples: wifi[id],
			SignalDBm: rows[i].Health.SignalDBm, LinkCount: rows[i].Health.LinkCount, UptimeSec: rows[i].Health.UptimeSec})
		rows[i].Stab = &res
	}
}

// stabilityOf is the verdict of one device with its full history, for the Overview.
func (h *handler) stabilityOf(ctx context.Context, id model.DeviceID, hl model.Health) (stability.Result, []model.DeviceBoot, bool) {
	if h.d.Stability == nil {
		return stability.Result{}, nil, false
	}
	now := h.d.Clock.Now()
	boots, err := h.d.Stability.ListBoots(ctx, id, store.MaxBootsPerDevice)
	if err != nil {
		h.log.WarnContext(ctx, "stability: reading restarts", "device", id, "err", err)
		return stability.Result{}, nil, false
	}
	var samples []model.WifiSample
	if all, err := h.d.Stability.WifiSamplesSince(ctx, now.Add(-24*time.Hour)); err == nil {
		samples = all[id]
	} else {
		h.log.WarnContext(ctx, "stability: reading Wi-Fi samples", "device", id, "err", err)
	}
	return stability.Assess(stability.Input{Now: now, Boots: boots, Samples: samples,
		SignalDBm: hl.SignalDBm, LinkCount: hl.LinkCount, UptimeSec: hl.UptimeSec}), boots, true
}

// stabProblem is the verdict when there is a problem, nil for a stable device.
func stabProblem(r *stability.Result) *stability.Result {
	if r == nil || r.Level == stability.Stable {
		return nil
	}
	return r
}

// stabTone is the tone class of a level's badge.
func stabTone(l stability.Level) string {
	switch l {
	case stability.BootLoop:
		return "t-danger"
	case stability.WeakWiFi:
		return "t-warn"
	case stability.Stable:
	}
	return "t-ok"
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return strconv.Itoa(n) + " times"
}

// restartPhrase: "Restarted 5 times in the last hour".
func restartPhrase(r stability.Result) string {
	if r.Restarts1h >= stability.BootLoopPerHour {
		return "restarted " + times(r.Restarts1h) + " in the last hour"
	}
	return "restarted " + times(r.Restarts24h) + " in the last 24 hours"
}

// stabBadgeTitle is the hover text of the list badge.
func stabBadgeTitle(r stability.Result) string {
	if r.Level == stability.BootLoop {
		t := strings.ToUpper(restartPhrase(r)[:1]) + restartPhrase(r)[1:]
		if r.Last != nil {
			t += "; last reason: " + strings.ToLower(r.LastReason.Label)
		}
		return t
	}
	parts := []string{}
	if r.WeakBySignal {
		parts = append(parts, fmt.Sprintf("signal averaged %d dBm over 24 h", r.AvgDBm))
	}
	if r.WeakByReconnects {
		parts = append(parts, fmt.Sprintf("%d Wi-Fi reconnects since boot", r.Reconnects))
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// stabAttention is the "Needs attention" text of a device with a problem. A weak signal is the 24 h
// average that made it weak, the number the card names, not the reading of this second.
func stabAttention(r stability.Result) string {
	if r.Level == stability.BootLoop {
		t := "Boot loop: " + restartPhrase(r)
		if r.DominantReason.Known && r.DominantReason.Attention {
			t += " (" + r.DominantReason.Short + ")"
		}
		return t
	}
	var parts []string
	if r.WeakBySignal {
		parts = append(parts, strconv.Itoa(r.AvgDBm)+" dBm")
	}
	if r.Reconnects > 0 {
		parts = append(parts, strconv.Itoa(r.Reconnects)+" "+noun(r.Reconnects, "reconnect", "reconnects")+" since boot")
	}
	return "Weak Wi-Fi: " + strings.Join(parts, ", ")
}

// ---- the card ----

type noteSeg struct {
	Text string
	Bold bool
}

type stabRow struct {
	Key         string
	Main, Class string // the first part of the value; Class colours it (num-bad, num-warn)
	Rest        string // the plain rest of the value
	Sub         string
	SubAt       time.Time // when set the sub line begins with this instant (time.js shows it in the browser's zone)
	SubAtText   string
	Spark       *stabSpark
}

type stabSpark struct {
	Tone   string // t-ok or t-warn
	Ref    string // y of the -75 dBm line
	Points string
}

type stabBoot struct {
	At         time.Time
	AtText     string
	Label, Raw string
	Ran        string
}

type stabCard struct {
	Level stability.Level
	Tone  string
	Meta  string
	Note  []noteSeg
	Rows  []stabRow
	Boots []stabBoot
}

// signalWord: how good a reading is. The edges are the ones of the Wi-Fi tile's bar.
func signalWord(dbm int) string {
	switch {
	case dbm > -60:
		return "good"
	case dbm >= stability.WeakDBm:
		return "fair"
	}
	return "weak"
}

// dbmText formats a reading: Go's %d prints the minus sign the way the page wants it.
func dbmText(dbm int) string { return strconv.Itoa(dbm) + " dBm" }

func minutesText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return plural(int(d.Seconds()), "second")
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute).Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

// tasmotaDowntime writes a duration the way Tasmota prints Wifi.Downtime: 0T00:03:41.
func tasmotaDowntime(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%dT%02d:%02d:%02d", s/86400, s%86400/3600, s%3600/60, s%60)
}

func exactDuration(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 60:
		return plural(s, "second")
	case s < 3600:
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	}
	return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
}

// byPhrase finishes "every time by ...".
func byPhrase(r stability.Reason) string {
	switch r.Short {
	case "crash":
		return "a crash"
	case "watchdog reset":
		return "a watchdog reset"
	case "brown-out":
		return "a brown-out"
	case "power-on":
		return "power coming back"
	}
	return strings.ToLower(r.Label)
}

func loopAdvice(r stability.Reason) string {
	switch r.Short {
	case "crash":
		return "A device that keeps crashing usually has an unstable power supply, a faulty rule or script, or a bad firmware build. Check the supply first."
	case "watchdog reset":
		return "A device that keeps hanging usually has a faulty rule or script, a bad firmware build or a weak power supply. Check the supply first."
	case "brown-out":
		return "The supply voltage keeps dropping under load. Check the power supply and its cable first."
	case "power-on":
		return "The device keeps losing power. Check the supply, the plug and whatever switches it. A light behind a wall switch that is flicked also reports this reason."
	}
	return "Check the power supply first, then rules, scripts and the firmware build."
}

// stabCardOf words the verdict. boots are the stored restarts, newest first.
func stabCardOf(now time.Time, r stability.Result, boots []model.DeviceBoot, hl model.Health, ssid string) stabCard {
	c := stabCard{Level: r.Level, Tone: stabTone(r.Level), Meta: "last 24 hours"}
	if r.Level == stability.Stable {
		c.Meta = "last 7 days"
	}
	switch r.Level {
	case stability.BootLoop:
		seg := []noteSeg{{Text: strings.ToUpper(restartPhrase(r)[:1]) + restartPhrase(r)[1:], Bold: true}}
		tail := ". "
		if d := r.DominantReason; d.Known && d.Attention {
			how := "every time"
			if r.DominantCount < r.WindowBoots {
				how = "mostly"
			}
			tail = ", " + how + " by " + byPhrase(d) + ". "
		}
		c.Note = append(seg, noteSeg{Text: tail + loopAdvice(r.DominantReason)})
	case stability.WeakWiFi:
		var parts []noteSeg
		if r.WeakBySignal {
			parts = append(parts, noteSeg{Text: "Signal averaged "}, noteSeg{Text: dbmText(r.AvgDBm), Bold: true},
				noteSeg{Text: fmt.Sprintf(" over the last 24 hours (weak is below %d dBm)", stability.WeakDBm)})
		}
		if r.WeakByReconnects {
			if len(parts) > 0 {
				parts = append(parts, noteSeg{Text: " and the device reconnected "})
			} else {
				parts = append(parts, noteSeg{Text: "The device reconnected "})
			}
			parts = append(parts, noteSeg{Text: times(r.Reconnects), Bold: true}, noteSeg{Text: " since it started"})
		}
		parts = append(parts, noteSeg{Text: ". Move the device or the access point closer, or add an access point."})
		c.Note = parts
	case stability.Stable:
	}

	// Last restart
	row := stabRow{Key: "Last restart", Main: "Not seen yet", Sub: "TasmoFleet notes a restart when it sees one."}
	if r.Last != nil {
		row = stabRow{Key: "Last restart", Main: r.LastReason.Label, SubAt: r.Last.At, SubAtText: ui.StampDayText(now, r.Last.At),
			Sub: " · " + agoWords(now, r.Last.At)}
		if r.Last.Reason != "" {
			row.Sub += " · Tasmota: “" + r.Last.Reason + "”"
		}
		if r.LastReason.Attention {
			row.Class = "num-bad"
		}
	}
	c.Rows = append(c.Rows, row)

	// Restarts
	var rs stabRow
	rs.Key = "Restarts"
	d24, d7 := fmt.Sprintf("%d in 24 h", r.Restarts24h), fmt.Sprintf("%d in %s", r.Restarts7d, "7 days")
	switch {
	case r.Restarts1h > 0:
		rs.Main, rs.Rest = fmt.Sprintf("%d in 1 h", r.Restarts1h), " · "+d24+" · "+d7
		if r.Restarts1h >= stability.BootLoopPerHour {
			rs.Class = "num-bad"
		}
	default:
		rs.Main, rs.Rest = d24, " · "+d7
		if r.Restarts24h >= stability.BootLoopPerDay {
			rs.Class = "num-bad"
		}
	}
	if r.Level == stability.BootLoop {
		rs.Sub = "counted from BootCount: includes restarts TasmoFleet did not see"
	}
	c.Rows = append(c.Rows, rs)
	if r.Level == stability.BootLoop {
		c.Rows = append(c.Rows, stabRow{Key: "Longest uptime", Main: minutesText(r.LongestUptime) + " in the last 24 h", Sub: "a healthy device stays up for days"})
	}

	// Wi-Fi signal
	if hl.SignalDBm != 0 || r.HasAvg {
		dbm := hl.SignalDBm
		if dbm == 0 {
			dbm = r.AvgDBm
		}
		sig := stabRow{Key: "Wi-Fi signal", Main: dbmText(dbm), Rest: " · " + signalWord(dbm)}
		if dbm < stability.WeakDBm {
			sig.Class = "num-warn"
		}
		var sub []string
		if r.Level == stability.WeakWiFi {
			if hl.RSSI > 0 {
				sub = append(sub, strconv.Itoa(hl.RSSI)+" %")
			}
			if ssid != "" {
				sub = append(sub, ssid)
			}
		}
		if len(r.Spark) >= 2 {
			switch r.Level {
			case stability.WeakWiFi:
				sub = append(sub, fmt.Sprintf("24 h average %d dBm, best %d, worst %d", r.AvgDBm, r.BestDBm, r.WorstDBm))
			case stability.Stable:
				sub = append(sub, fmt.Sprintf("24 h range %d to %d dBm", r.WorstDBm, r.BestDBm))
			case stability.BootLoop:
			}
			sig.Spark = sparkOf(r)
		}
		if r.Level != stability.Stable || len(r.Spark) >= 2 {
			sig.Sub = strings.Join(sub, " · ")
		}
		c.Rows = append(c.Rows, sig)
	}

	// Wi-Fi reconnects
	if hl.LinkCount > 0 {
		rc := stabRow{Key: "Wi-Fi reconnects", Main: strconv.Itoa(r.Reconnects) + " since boot"}
		if r.WeakByReconnects {
			rc.Class = "num-warn"
		}
		// A few seconds of downtime is the connect after boot: only a minute or more, or a weak link,
		// is worth the sentence; a stable device then says there was none to speak of.
		switch {
		case hl.WifiDowntime >= time.Minute || hl.WifiDowntime > 0 && r.Level == stability.WeakWiFi:
			rc.Sub = fmt.Sprintf("Wi-Fi down for %s in total (Tasmota: LinkCount %d, Downtime %s)", exactDuration(hl.WifiDowntime), hl.LinkCount, tasmotaDowntime(hl.WifiDowntime))
		case r.Level == stability.Stable && hl.WifiDowntime < time.Minute:
			rc.Sub = "no Wi-Fi downtime"
		}
		c.Rows = append(c.Rows, rc)
	}

	if len(boots) > 0 {
		for i, b := range boots {
			if i == stabilityRecent {
				break
			}
			sb := stabBoot{At: b.At, AtText: ui.StampClockOrDayText(now, b.At), Label: stability.Classify(b.Reason).Label}
			if b.Reason != "" && b.Reason != sb.Label {
				sb.Raw = b.Reason
			}
			if b.UptimeBefore > 0 {
				sb.Ran = "after " + minutesText(b.UptimeBefore)
			}
			c.Boots = append(c.Boots, sb)
		}
	}
	return c
}

// sparkOf draws the last 24 hours of samples: 240 x 36, the -75 dBm line in the middle, 0.68 px per dB.
func sparkOf(r stability.Result) *stabSpark {
	y := func(dbm int) float64 { return max(2, min(34, 18-float64(dbm-stability.WeakDBm)*0.68)) }
	n := len(r.Spark)
	pts := make([]string, n)
	for i, v := range r.Spark {
		pts[i] = fmt.Sprintf("%.1f,%.1f", float64(i)*240/float64(n-1), y(v))
	}
	tone := "t-ok"
	if r.Weak {
		tone = "t-warn"
	}
	return &stabSpark{Tone: tone, Ref: fmt.Sprintf("%.1f", y(stability.WeakDBm)), Points: strings.Join(pts, " ")}
}
