package web

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/web/ui"
)

// The status bar (#307) says three things beyond the version: the MQTT broker connection, the
// running jobs, and the last scheduled backup run. It shows counts, times and names only, never
// an error text or a credential, so what it reads is safe for every role that can open a page.
// Every page it links to (/transactions, /whats-new) is open to the operator role.

// statusJobsShown is how many running jobs the bar names; the rest is "+N more".
const statusJobsShown = 2

// BrokerInfo is the MQTT broker connection as the status bar shows it.
type BrokerInfo struct {
	Enabled   bool      // a broker is configured; false = MQTT is off and the bar says nothing about it
	Connected bool      // the connection is up and subscribed (mqttbus Connected)
	Host      string    // host and port only: never the user, the password or the scheme
	Since     time.Time // when Connected last changed
}

// BackupRunInfo is the scheduled backup run (BACKUP_INTERVAL) as the status bar shows it. It is
// kept in memory: after a restart there is no last run until the next one finishes.
type BackupRunInfo struct {
	Running     bool // a scheduled run is in progress: Done of Total devices so far
	Done, Total int
	Finished    time.Time // when the last finished run ended; zero = none since the start
	LastTotal   int       // devices of the last finished run
	LastFailed  int       // of which failed
}

// statusBar reads what the bar shows. live says whether the page holds an event stream: only then
// the bar asks to be refreshed on events (a page without a stream is a snapshot, like the rest of it).
func (h *handler) statusBar(ctx context.Context, base string, live bool) ui.StatusBar {
	sb := ui.StatusBar{Now: h.d.Clock.Now()}
	if live {
		sb.Fragment = base + "/statusbar"
	}
	if h.d.OTA != nil { // the transaction pages exist only with updates
		sb.JobsHref = base + "/transactions"
	}
	if h.d.MQTTStatus != nil {
		if b := h.d.MQTTStatus(); b.Enabled {
			sb.Broker = &ui.StatusBroker{Host: b.Host, Connected: b.Connected, Since: b.Since}
		}
	}
	sb.Jobs, sb.More = h.statusJobs(ctx, base)
	if h.d.ScheduledBackup != nil {
		r := h.d.ScheduledBackup()
		if r.Running {
			if len(sb.Jobs) < statusJobsShown {
				sb.Jobs = append(sb.Jobs, ui.StatusJob{Kind: "Backup", Done: r.Done, Total: r.Total})
			} else {
				sb.More++
			}
		}
		if !r.Finished.IsZero() {
			sb.Backup = &ui.StatusBackup{At: r.Finished, Failed: r.LastFailed, Total: r.LastTotal}
		}
	}
	return sb
}

// statusJobs lists the running transactions, oldest first, at most statusJobsShown of them, and
// how many more run. A failed read shows no jobs: the bar must never take a page down.
func (h *handler) statusJobs(ctx context.Context, base string) ([]ui.StatusJob, int) {
	if h.d.Jobs == nil {
		return nil, 0
	}
	active, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{States: activeJobStates})
	if err != nil || len(active) == 0 {
		return nil, 0
	}
	var batches []model.BatchID
	for _, j := range active {
		if !slices.Contains(batches, j.BatchID) {
			batches = append(batches, j.BatchID)
		}
	}
	slices.Sort(batches)
	var out []ui.StatusJob
	for _, b := range batches[:min(len(batches), statusJobsShown)] {
		jobs, err := h.d.Jobs.ListJobs(ctx, store.JobFilter{BatchID: b})
		if err != nil || len(jobs) == 0 {
			continue
		}
		t := groupTransactions(jobs)[0]
		j := ui.StatusJob{Kind: statusKind(t.first), Done: t.Done, Total: t.Total}
		if t.first.IsCommand() {
			j.Detail = statusCommand(t.first.Commands)
		}
		if h.d.OTA != nil {
			j.ID, j.Href = txID(b), base+"/transactions/"+bid(b)
		}
		out = append(out, j)
	}
	return out, len(batches) - len(out)
}

func statusKind(j model.Job) string {
	switch {
	case j.IsPolicy():
		return "Make compliant"
	case j.IsCommand():
		return "Command"
	case j.IsBackup():
		return "Backup"
	case j.IsTemplate():
		return "GPIO template"
	}
	return "Update"
}

// statusCommand is the name of a command as the audit log words it: the first word of the redacted
// text (the arguments may hold a secret), nothing for a refused one.
func statusCommand(cmd string) string {
	red := cmdpolicy.Redacted(strings.TrimSpace(cmd))
	if red == cmdpolicy.RejectedText {
		return ""
	}
	f := strings.Fields(red)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// statusBarFragment is GET /statusbar: the bar's parts as out-of-band swaps, fetched on events.
func (h *handler) statusBarFragment(w http.ResponseWriter, r *http.Request) {
	base := httpBase(r)
	d := ui.ShellData{Base: base, Live: true, Version: h.d.Version, Status: h.statusBar(r.Context(), base, true)}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := ui.StatusBarOOB(d).Render(r.Context(), w); err != nil {
		h.log.WarnContext(r.Context(), "render status bar", "err", err)
	}
}
