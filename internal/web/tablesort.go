package web

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/auth"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

// Server-side column sorting. A sortable header is a plain link
// (?sort=<column>&dir=asc|desc, filters kept), so sorting works without
// script. The last explicit choice is saved per user and table
// (preference "sort.<table>") and used when the URL names none.

// tableSort is the effective sort of a table.
type tableSort struct{ Key, Dir string }

// sortParams names the query parameters of a table's sort. A page with one
// sortable table uses sort and dir; a page with several prefixes both with the
// table name, so each table keeps its own order in the URL.
func sortParams(table string) (key, dir string) {
	if multiSort[table] {
		return "sort_" + table, "dir_" + table
	}
	return "sort", "dir"
}

// resolveSort is the effective sort of a table: the query, else the saved
// choice, else def. It reads only; sortOf also saves an explicit choice.
func resolveSort(q url.Values, prefs map[string]string, table string, def tableSort) (ts tableSort, explicit bool) {
	td := tableDefs[table]
	valid := func(k string) bool { cd, ok := td.col(k); return ok && cd.Sort }
	var saved sortPref
	_ = json.Unmarshal([]byte(prefs["sort."+table]), &saved) // corrupt row: default
	if !valid(saved.K) || (saved.D != "asc" && saved.D != "desc") {
		saved = sortPref{}
	}
	kp, dp := sortParams(table)
	if k := q.Get(kp); valid(k) {
		d := "asc"
		if q.Get(dp) == "desc" {
			d = "desc"
		}
		return tableSort{k, d}, k != saved.K || d != saved.D
	}
	if saved.K != "" {
		return tableSort{saved.K, saved.D}, false
	}
	return def, false
}

// sortOf resolves the sort of table: the URL, else the saved choice, else def.
// An explicit, valid choice that differs from the saved one is saved here (a
// preference write, no device or inventory effect).
func (h *handler) sortOf(r *http.Request, table string, def tableSort) tableSort {
	ts, changed := resolveSort(r.URL.Query(), h.userPrefs(r), table, def)
	if changed && h.d.Prefs != nil {
		if p, ok := auth.FromContext(r.Context()); ok && p.Subject != "" {
			if v, err := marshalPref(sortPref{K: ts.Key, D: ts.Dir}); err == nil {
				if err := h.d.Prefs.SetUserPref(r.Context(), p.Subject, "sort."+table, v); err != nil {
					h.log.WarnContext(r.Context(), "save sort preference", "err", err)
				}
			}
		}
	}
	return ts
}

// sortLink is what a table's header links need: the page URL without this
// table's sort and without paging, and the sort in effect.
type sortLink struct{ Base, Key, Dir string }

// pageSorts resolves the sort of every sortable table for a page at path
// (empty: none, the page has no URL a header link could return to). It reads
// only; the handler's sortOf saved an explicit choice already.
func pageSorts(r *http.Request, prefs map[string]string, path string) map[string]sortLink {
	if path == "" {
		return nil
	}
	out := make(map[string]sortLink, len(sortableCols))
	for table := range sortableCols {
		ts, _ := resolveSort(r.URL.Query(), prefs, table, defaultSorts[table])
		kp, dp := sortParams(table)
		q := r.URL.Query()
		q.Del(kp)
		q.Del(dp)
		q.Del("page")
		base := path
		if enc := q.Encode(); enc != "" {
			base += "?" + enc
		}
		out[table] = sortLink{Base: base, Key: ts.Key, Dir: ts.Dir}
	}
	return out
}

// sortPath is the URL path header links of a rendered page return to: the
// request's own for a GET, nothing otherwise (a POST result has no page to sort).
func sortPath(r *http.Request) string {
	if r.Method != http.MethodGet {
		return ""
	}
	return httpBase(r) + r.URL.Path
}

// values adds the sort to a query so refreshes and links keep it.
func (s tableSort) values(v url.Values) url.Values {
	v.Set("sort", s.Key)
	v.Set("dir", s.Dir)
	return v
}

// ---- comparators ----

// compareVersions orders Tasmota versions numerically ("15.10.0" after "15.9.0")
// and a pre-release ("16.0.0-rc1", "16.0.0.1") by its numeric parts too; a
// release sorts after its own pre-release suffix.
func compareVersions(a, b string) int {
	pa, sa := versionParts(a)
	pb, sb := versionParts(b)
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := cmp.Compare(x, y); c != 0 {
			return c
		}
	}
	switch {
	case sa == sb:
		return 0
	case sa == "":
		return 1 // 16.0.0 is newer than 16.0.0-rc1
	case sb == "":
		return -1
	}
	return strings.Compare(sa, sb)
}

func versionParts(v string) (nums []int, suffix string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	core, suf, _ := strings.Cut(v, "-")
	parts := strings.Split(core, ".")
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil { // "15.6.0abc": the rest counts as a suffix
			return nums, strings.Join(parts[i:], ".")
		}
		nums = append(nums, n)
	}
	return nums, suf
}

// compareAddresses orders IP addresses numerically, names after them.
func compareAddresses(a, b string) int {
	ia, ea := netip.ParseAddr(a)
	ib, eb := netip.ParseAddr(b)
	switch {
	case ea == nil && eb == nil:
		return ia.Compare(ib)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(strings.ToLower(a), strings.ToLower(b))
}

// healthSeverity: 0 is fine, higher is worse.
func healthSeverity(s model.HealthState) int {
	switch s {
	case model.HealthOnline:
		return 0
	case model.HealthStale:
		return 1
	case model.HealthUnknown:
		return 2
	case model.HealthOffline:
		return 3
	case model.HealthAuthFailed:
		return 4
	}
	return 2
}

// powerRank: any relay on, else off, else unknown.
func powerRank(p powerCell) int {
	rank := 0
	for _, r := range p.Relays {
		switch r.State {
		case "ON":
			return 2
		case "OFF":
			rank = 1
		}
	}
	return rank
}

// restartAt is when the device last restarted, as far as its report tells.
func restartAt(h model.Health) time.Time {
	if h.UptimeSec <= 0 || h.LastSeen.IsZero() {
		return time.Time{}
	}
	return h.LastSeen.Add(-time.Duration(h.UptimeSec) * time.Second)
}

// sortDevices orders rows by spec. A row without a value for the column (no
// uptime reported, unknown firmware) goes last in both directions; ties keep
// the name order.
func sortDevices(rows []deviceRow, s tableSort) {
	// cmpRows returns the order of two rows with values, and whether each has one.
	type keyFn func(a, b deviceRow) (c int, hasA, hasB bool)
	byStr := func(f func(deviceRow) string, c func(a, b string) int) keyFn {
		return func(a, b deviceRow) (int, bool, bool) {
			x, y := f(a), f(b)
			return c(x, y), x != "", y != ""
		}
	}
	byInt := func(f func(deviceRow) int64) keyFn {
		return func(a, b deviceRow) (int, bool, bool) {
			x, y := f(a), f(b)
			return cmp.Compare(x, y), x > 0, y > 0
		}
	}
	lower := func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) }
	var key keyFn
	switch s.Key {
	case "status":
		key = func(a, b deviceRow) (int, bool, bool) {
			return cmp.Compare(healthSeverity(a.Health.State), healthSeverity(b.Health.State)), true, true
		}
	case "firmware":
		key = byStr(func(r deviceRow) string { return r.Device.Identity.Version }, compareVersions)
	case "family":
		key = byStr(func(r deviceRow) string { return string(r.Device.Identity.Family) }, lower)
	case "address":
		key = byStr(func(r deviceRow) string { return r.Device.Address }, compareAddresses)
	case "uptime":
		key = byInt(func(r deviceRow) int64 { return r.Health.UptimeSec })
	case "wifi":
		key = byInt(func(r deviceRow) int64 { return int64(r.Health.RSSI) })
	case "restart":
		key = byInt(func(r deviceRow) int64 { return restartAt(r.Health).Unix() })
	case "otaurl":
		key = byStr(func(r deviceRow) string { return r.Device.OTAURL }, lower)
	case "grouptopic":
		key = byStr(func(r deviceRow) string { return r.Device.GroupTopic }, lower)
	case "power":
		key = func(a, b deviceRow) (int, bool, bool) {
			x, y := powerRank(a.Power), powerRank(b.Power)
			return cmp.Compare(x, y), x > 0, y > 0
		}
	default: // name
		key = byStr(func(r deviceRow) string { return r.Device.Name }, lower)
	}
	slices.SortStableFunc(rows, func(a, b deviceRow) int {
		c, hasA, hasB := key(a, b)
		switch {
		case hasA != hasB:
			if hasA {
				return -1
			}
			return 1
		case !hasA:
			c = 0
		case s.Dir == "desc":
			c = -c
		}
		if c != 0 {
			return c
		}
		return lower(a.Device.Name, b.Device.Name)
	})
}

// sortAudit orders audit events; ties keep the incoming (newest first) order.
func sortAudit(ev []model.AuditEvent, s tableSort) {
	lower := func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) }
	var key func(a, b model.AuditEvent) int
	switch s.Key {
	case "actor":
		key = func(a, b model.AuditEvent) int { return lower(a.Actor, b.Actor) }
	case "action":
		key = func(a, b model.AuditEvent) int { return lower(auditLabel(a.Action), auditLabel(b.Action)) } // the column shows the label
	case "target":
		key = func(a, b model.AuditEvent) int { return lower(a.Target, b.Target) }
	case "result": // ok first, then by text
		key = func(a, b model.AuditEvent) int {
			if x, y := a.Result == "ok", b.Result == "ok"; x != y {
				if x {
					return -1
				}
				return 1
			}
			return lower(a.Result, b.Result)
		}
	default:
		key = func(a, b model.AuditEvent) int { return a.At.Compare(b.At) }
	}
	slices.SortStableFunc(ev, func(a, b model.AuditEvent) int {
		if s.Dir == "desc" {
			return key(b, a)
		}
		return key(a, b)
	})
}
