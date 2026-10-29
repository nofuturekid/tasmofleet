package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/audit"
	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/settings"
	"github.com/nofuturekid/tasmofleet/internal/sysconfig"
	"github.com/nofuturekid/tasmofleet/internal/web"
)

// setupSettings builds the settings service and the policy report. The rules
// are the ones managed on the policy page; saved rules reach the report live.
// A POLICY_RULES value that is still in the environment is imported once.
func (a *runtime) setupSettings() error {
	a.settings = settings.New(settings.Options{Devices: a.svc.Store, Client: a.svc.Client, Audit: a.svc.Audit})
	rep := &policyReporter{devices: a.svc.Store, backups: a.backups, reader: a.settings, audit: a.svc.Audit}
	a.policyLive = rep
	a.rules = newRulesService(a.svc, rep.Preview, rep.setRules)
	rules, err := a.rules.Active(a.bgContext)
	if err != nil {
		// Rules that were valid when saved can only fail here after a code change;
		// the report then shows no rules and says so in the log.
		a.log.Error("stored policy rules ignored", "err", err)
	}
	rep.setRules(rules)
	a.importLegacyPolicy()
	a.policy = rep
	a.fixer = &policy.Fixer{Client: a.svc.Client, Reader: a.settings}
	if a.livePause, err = sysconfig.NewLivePause(a.bgContext, a.svc.Store, a.svc.Audit); err != nil {
		return err
	}
	a.startPolicyLive()
	return nil
}

// validateWant checks an expected value with the validator of the setting it
// names; commands outside the curated list (SetOptions) are checked by the
// rule parser only.
func validateWant(command, want string) error {
	if f, ok := settings.Lookup(command); ok && f.Validate != nil {
		return f.Validate(want)
	}
	if settings.SetOptionParam(command) {
		return settings.ValidateSetOptionParam(want)
	}
	return nil
}

// settingsDep avoids a typed-nil interface in web.Deps.
func (a *runtime) settingsDep() web.SettingsService {
	if a.settings == nil {
		return nil
	}
	return a.settings
}

type deviceLister interface {
	ListDevices(ctx context.Context) ([]model.Device, error)
}

type keyReader interface {
	ReadKeys(ctx context.Context, id model.DeviceID, keys []string) (map[string]string, error)
}

// policyReporter builds the policy view. Live values are primary: they come
// from the hourly poller (policyPoller), from MQTT stat messages and from the
// on-demand check, and each carries the time it was read. A backup value is
// only a fallback for a setting that has no live value, and is labelled with
// the backup's date.
//
// The on-demand check is rate limited: one run at a time, at least liveGap
// between runs, and a device read within liveDeviceCooldown is not read again.
// Requests over the limit get the cached values and a notice, never a refusal.
type policyReporter struct {
	rmu     sync.RWMutex // guards rules, which the policy editor replaces live
	rules   []policy.Rule
	devices deviceLister
	backups backup.Service
	reader  keyReader
	audit   audit.Recorder // optional
	now     func() time.Time

	vals liveStore

	mu      sync.Mutex // held for a whole on-demand run (single flight)
	lastRun time.Time
}

const (
	liveParallel       = 4
	liveTimeout        = 30 * time.Second
	liveGap            = time.Minute
	liveDeviceCooldown = 5 * time.Minute
)

func (p *policyReporter) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *policyReporter) currentRules() []policy.Rule {
	p.rmu.RLock()
	defer p.rmu.RUnlock()
	return p.rules
}

// setRules replaces the rules the report checks (live reload).
func (p *policyReporter) setRules(rules []policy.Rule) {
	p.rmu.Lock()
	p.rules = rules
	p.rmu.Unlock()
}

// ruleCommands lists the distinct commands of the enabled rules that can be
// read live. A disabled rule judges nothing, so it must not cost device reads.
func ruleCommands(rules []policy.Rule) []string {
	var cmds []string
	seen := map[string]bool{}
	for _, r := range rules {
		if !r.Disabled && !seen[r.Command] && settings.LiveReadable(r.Command) {
			seen[r.Command] = true
			cmds = append(cmds, r.Command)
		}
	}
	return cmds
}

// values merges, per device and setting, the live value (preferred) with the
// newest decoded backup (fallback). It never talks to a device.
func (p *policyReporter) values(ctx context.Context, devs []model.Device, rules []policy.Rule) (map[model.DeviceID]map[string]string, map[model.DeviceID]map[string]web.ValueSource) {
	vals := map[model.DeviceID]map[string]string{}
	prov := map[model.DeviceID]map[string]web.ValueSource{}
	for _, d := range devs {
		live := p.vals.get(d.ID)
		v, pv := map[string]string{}, map[string]web.ValueSource{}
		var backupVals map[string]string
		var backupAt time.Time
		backupLoaded := false
		for _, r := range rules {
			key := strings.ToLower(r.Command)
			if _, done := v[key]; done {
				continue
			}
			if lv, ok := live[r.Command]; ok {
				v[key], pv[key] = lv.value, web.ValueSource{Kind: "live", At: lv.at}
				continue
			}
			if !backupLoaded {
				backupVals, backupAt = p.fromBackup(ctx, d.ID)
				backupLoaded = true
			}
			if bv, ok := backupVals[r.Command]; ok {
				v[key], pv[key] = bv, web.ValueSource{Kind: "backup", At: backupAt}
			}
		}
		vals[d.ID], prov[d.ID] = v, pv
	}
	return vals, prov
}

// Preview says what rules would flag, judged from cached live values and the
// newest decoded backups. It never talks to a device.
func (p *policyReporter) Preview(ctx context.Context, rules []policy.Rule) (sysconfig.PreviewResult, error) {
	devs, err := p.devices.ListDevices(ctx)
	if err != nil {
		return sysconfig.PreviewResult{}, err
	}
	values, _ := p.values(ctx, devs, rules)
	res := sysconfig.PreviewResult{Devices: len(devs), Rules: make([]sysconfig.RulePreview, len(rules))}
	for _, d := range devs {
		if len(values[d.ID]) == 0 {
			res.NoData++
		}
	}
	// One check with precedence: a rule is counted only for the devices where
	// it is the one evaluated, exactly as the policy page reports them.
	for _, f := range policy.Check(rules, devs, values) {
		if f.Unknown {
			res.Rules[f.RuleIndex].Unknown++
		} else {
			res.Rules[f.RuleIndex].Flagged++
		}
	}
	res.Conflicts = policy.Conflicts(rules, devs)
	return res, nil
}

func (p *policyReporter) Report(ctx context.Context, actor string, live bool) (web.PolicyReport, error) {
	devs, err := p.devices.ListDevices(ctx)
	if err != nil {
		return web.PolicyReport{}, err
	}
	throttled := false
	if live {
		throttled = p.refresh(ctx, actor, devs)
	}
	rules := p.currentRules()
	values, prov := p.values(ctx, devs, rules)
	notLive := map[string]bool{}
	for _, r := range rules {
		if !settings.LiveReadable(r.Command) {
			notLive[strings.ToLower(r.Command)] = true
		}
	}
	return web.PolicyReport{
		Rules: rules, Devices: devs, Findings: policy.Check(rules, devs, values),
		Provenance: prov, NotLive: notLive, LiveThrottled: throttled,
	}, nil
}

// fromBackup returns the policy values of the newest decoded backup and its date.
func (p *policyReporter) fromBackup(ctx context.Context, id model.DeviceID) (map[string]string, time.Time) {
	list, err := p.backups.List(ctx, id)
	if err != nil {
		return nil, time.Time{}
	}
	var newest *model.Backup
	for i := range list {
		if list[i].Kind == model.BackupDecoded && (newest == nil || list[i].TakenAt.After(newest.TakenAt)) {
			newest = &list[i]
		}
	}
	if newest == nil {
		return nil, time.Time{}
	}
	data, _, err := p.backups.Open(ctx, newest.ID)
	if err != nil {
		return nil, time.Time{} // an unreadable backup means "unknown", not a failed page
	}
	return policy.ValuesFromDecoded(data), newest.TakenAt
}

// refresh is the on-demand live check. It reports whether the rate limit
// stopped fresh reads.
func (p *policyReporter) refresh(ctx context.Context, actor string, devs []model.Device) bool {
	if !p.mu.TryLock() { // single flight: wait for the run in progress, then use what it read
		p.mu.Lock()
		defer p.mu.Unlock()
		return true
	}
	defer p.mu.Unlock()
	t := p.clock()
	throttled := !p.lastRun.IsZero() && t.Sub(p.lastRun) < liveGap
	var fresh []model.Device
	if !throttled {
		for _, d := range devs {
			if at := p.vals.newest(d.ID); at.IsZero() || t.Sub(at) >= liveDeviceCooldown {
				fresh = append(fresh, d)
			}
		}
	}
	if len(fresh) > 0 {
		p.lastRun = t
		p.readMany(ctx, fresh, liveParallel)
	}
	if p.audit != nil {
		detail, _ := json.Marshal(map[string]any{"devices_read": len(fresh), "throttled": throttled}) // a plain map always encodes
		p.audit.Record(ctx, model.AuditEvent{Actor: actor, Action: "policy.live", Target: "fleet", Detail: string(detail)})
	}
	return throttled
}

// readMany reads the rule settings of the devices, parallel at a time.
func (p *policyReporter) readMany(ctx context.Context, devs []model.Device, parallel int) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for _, d := range devs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			p.readDevice(ctx, d.ID)
		}()
	}
	wg.Wait()
}

// readDevice reads the rule settings of one device with side-effect-free
// queries and stores what it got. A dead device keeps what it answered; the
// rest stays as it was.
func (p *policyReporter) readDevice(ctx context.Context, id model.DeviceID) bool {
	cmds := ruleCommands(p.currentRules())
	if len(cmds) == 0 {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, liveTimeout)
	defer cancel()
	v, err := p.reader.ReadKeys(cctx, id, cmds)
	if len(v) > 0 {
		p.vals.put(id, v, p.clock())
	}
	return err == nil
}

// liveStore caches the last live value of every setting with its read time.
type liveStore struct {
	mu sync.Mutex
	m  map[model.DeviceID]map[string]liveVal
}

type liveVal struct {
	value string
	at    time.Time
}

func (s *liveStore) put(id model.DeviceID, vals map[string]string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[model.DeviceID]map[string]liveVal{}
	}
	if s.m[id] == nil {
		s.m[id] = map[string]liveVal{}
	}
	for k, v := range vals {
		s.m[id][k] = liveVal{v, at}
	}
}

func (s *liveStore) get(id model.DeviceID) map[string]liveVal {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]liveVal, len(s.m[id]))
	for k, v := range s.m[id] {
		out[k] = v
	}
	return out
}

// newest is the time of the most recent read of any setting of the device.
func (s *liveStore) newest(id model.DeviceID) time.Time {
	var t time.Time
	for _, v := range s.get(id) {
		if v.at.After(t) {
			t = v.at
		}
	}
	return t
}

// importLegacyPolicy imports POLICY_RULES (inline JSON or a file path) from
// the environment as the first managed version, if no rules were ever saved.
// The variable is not read after that.
func (a *runtime) importLegacyPolicy() {
	raw := strings.TrimSpace(a.svc.Sys.LegacyPolicyRules())
	if raw == "" {
		return
	}
	ctx := a.bgContext
	if has, err := a.rules.HasVersions(ctx); err != nil || has {
		a.log.Warn("ignoring TASMOFLEET_POLICY_RULES: managed rules exist; remove it from the container configuration", "err", err)
		return
	}
	if !strings.HasPrefix(raw, "[") {
		b, err := os.ReadFile(raw) //nolint:gosec // G304: the path is operator configuration (a legacy variable), not request input
		if err != nil {
			a.log.Error("TASMOFLEET_POLICY_RULES not imported: cannot read the file", "err", err)
			return
		}
		raw = string(b)
	}
	if _, err := a.rules.ImportLegacy(ctx, "env-import", []byte(raw), a.svc.Audit); err != nil {
		a.log.Error("TASMOFLEET_POLICY_RULES not imported", "err", err)
		return
	}
	a.log.Warn("imported the policy rules from TASMOFLEET_POLICY_RULES as the managed rules; remove it from the container configuration")
}

// newRulesService builds the managed policy rules over the shared services; the
// server adds the live preview and the live push, the CLI needs neither.
func newRulesService(svc *Services, preview func(context.Context, []policy.Rule) (sysconfig.PreviewResult, error), onChange func([]policy.Rule)) *sysconfig.Rules {
	return sysconfig.NewRules(sysconfig.RulesOptions{
		Store: svc.Store, Audit: svc.Audit, Queryable: settings.Queryable, ValidateWant: validateWant,
		Preview: preview, OnChange: onChange,
	})
}
