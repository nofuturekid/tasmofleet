package sysconfig

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Regression tests for the security review of the system settings. Principle:
// a secret only ever goes to the target it was entered for.

func mqttStored() map[string]string {
	return map[string]string{"MQTT_URL": "tcp://broker.lan:1883", "MQTT_USER": "u", "MQTT_PASSWORD": "fake-secret-Zk3"}
}

// F1: changing the broker keeps neither the stored password nor sends it on.
func TestChangingTheBrokerRequiresTheCredentialsAgain(t *testing.T) {
	var seen MQTTParams
	s := mqttTestSvc(t, mqttStored(), nil, &seen)
	ctx := context.Background()
	for _, url := range []string{"tcp://evil.example:1883", "tcp://broker.lan:1884", "ssl://broker.lan:1883"} {
		_, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": url}})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Fields["MQTT_PASSWORD"] == "" {
			t.Errorf("%s: err = %v", url, err)
		}
	}
	if s.Snapshot().MQTTURL != "tcp://broker.lan:1883" {
		t.Fatal("a refused change must not apply")
	}
	// Typing the password with the new broker, or removing it, is allowed.
	if _, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://new.lan:1883", "MQTT_PASSWORD": "typed"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://other.lan:1883"}, Clear: []string{"MQTT_PASSWORD"}}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().MQTTPassword != "" {
		t.Fatal("the password must be gone")
	}
	// Probe A of the review: save then test sends nothing stored anywhere new.
	s = mqttTestSvc(t, mqttStored(), nil, &seen)
	_, _ = s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://evil.example:1883"}})
	seen = MQTTParams{}
	if _, err := s.TestMQTT(ctx, TestRequest{Actor: "a", StepUp: stepped}); err != nil {
		t.Fatal(err)
	}
	if seen.Password != "" && seen.URL != "tcp://broker.lan:1883" {
		t.Fatalf("stored password went to %s", seen.URL)
	}
	// Same host and port, other path or user: the password stays with its broker.
	s = mqttTestSvc(t, mqttStored(), nil, &seen)
	if _, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_USER": "other"}}); err != nil {
		t.Fatal(err)
	}
}

// F5: two saves at once must not run subscriber callbacks concurrently, and the
// last event must match the final state.
func TestConcurrentSavesNotifySubscribersInOrder(t *testing.T) {
	s, _ := newSvc(t, newMem())
	var active, overlaps atomic.Int32
	var mu sync.Mutex
	var last string
	s.Subscribe(func(e Event) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		last = e.New.BackupSchedule.String()
		mu.Unlock()
		active.Add(-1)
	})
	var wg sync.WaitGroup
	for i := 1; i <= 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Apply(context.Background(), Request{Actor: "a", Values: map[string]string{"BACKUP_INTERVAL": fmt.Sprintf("%dh", i+1)}})
		}()
	}
	wg.Wait()
	if overlaps.Load() != 0 {
		t.Fatal("subscriber callbacks ran concurrently")
	}
	mu.Lock()
	defer mu.Unlock()
	if last != s.Snapshot().BackupSchedule.String() {
		t.Fatalf("last event %s, final state %s", last, s.Snapshot().BackupSchedule)
	}
}

// S14: a request that both sets and clears one key (or names it twice) has no single meaning: a
// clear means "back to the default", a value means "this one". Applying either would silently pick
// a winner, so the request is refused as a whole and nothing is stored or announced.
func TestSetAndClearOfOneKeyIsRefusedAsAWhole(t *testing.T) {
	cases := map[string]Request{
		"value and clear":  {Actor: "a", Values: map[string]string{"BACKUP_KEEP": "9"}, Clear: []string{"BACKUP_KEEP"}},
		"clear twice":      {Actor: "a", Clear: []string{"BACKUP_KEEP", "BACKUP_KEEP"}},
		"other key stored": {Actor: "a", Values: map[string]string{"BACKUP_KEEP": "9", "OTA_PARALLEL": "3"}, Clear: []string{"BACKUP_KEEP"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			st := newMem()
			s, _ := newSvc(t, st)
			before, rowsBefore := s.Snapshot(), len(st.rows) // the first-start marker row is already there
			events := 0
			s.Subscribe(func(Event) { events++ })
			_, err := s.Apply(context.Background(), req)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Fields["BACKUP_KEEP"] == "" {
				t.Fatalf("err = %v, want a validation error naming BACKUP_KEEP", err)
			}
			if len(st.rows) != rowsBefore || events != 0 || !reflect.DeepEqual(s.Snapshot(), before) {
				t.Fatalf("a refused request changed something: rows %v, events %d", st.rows, events)
			}
		})
	}
}

func TestUndecryptableStoredSecretDoesNotStopTheStart(t *testing.T) {
	st := newMem()
	st.rows["MQTT_URL"] = store.SystemSetting{Key: "MQTT_URL", Value: "tcp://broker.lan:1883"}
	st.rows["MQTT_PASSWORD"] = store.SystemSetting{Key: "MQTT_PASSWORD", Secret: true, Unreadable: true}
	s, _ := newSvc(t, st)
	if s.Snapshot().MQTTURL != "" {
		t.Fatal("the feature that needs the lost secret must stay off")
	}
	if !strings.Contains(strings.Join(mapValues(s.Skipped()), " "), "enter it again") {
		t.Fatalf("skipped %v", s.Skipped())
	}
	for _, f := range s.State() {
		if f.Key == "MQTT_URL" && f.Value != "tcp://broker.lan:1883" {
			t.Fatalf("the form must still show the stored URL: %+v", f)
		}
	}
	if _, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_PASSWORD": "typed-again"}}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().MQTTURL != "tcp://broker.lan:1883" || s.Snapshot().MQTTPassword != "typed-again" || len(s.Skipped()) != 0 {
		t.Fatalf("after re-entry MQTT is on again: %+v skipped %v", s.Snapshot().MQTTURL, s.Skipped())
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// F8
func TestScheduledScanSurvivesARestart(t *testing.T) {
	st := newMem()
	s, _ := newSvc(t, st)
	if _, err := s.Apply(context.Background(), Request{Actor: "a", Values: map[string]string{"DISCOVERY_SCAN": "true", "DISCOVERY_SCAN_CIDRS": "172.31.13.0/24"}}); err != nil {
		t.Fatal(err)
	}
	s2, _ := newSvc(t, st)
	if !s2.Snapshot().DiscoveryScan || len(s2.Skipped()) != 0 {
		t.Fatalf("scan=%v skipped=%v", s2.Snapshot().DiscoveryScan, s2.Skipped())
	}
}
func boundRows() map[string]store.SystemSetting {
	return map[string]store.SystemSetting{
		"MQTT_URL":      {Key: "MQTT_URL", Value: "tcp://broker.lan:1883"},
		"MQTT_PASSWORD": {Key: "MQTT_PASSWORD", Value: "fake-secret-Zk3", Secret: true, Target: "tcp://broker.lan:1883"},
	}
}

func TestSavedPasswordRecordsItsTarget(t *testing.T) {
	st := newMem()
	s, _ := newSvc(t, st)
	_, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://Broker.lan:1883/x", "MQTT_PASSWORD": "typed"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := st.rows["MQTT_PASSWORD"].Target; got != "tcp://broker.lan:1883" {
		t.Fatalf("target %q", got)
	}
	if s.Snapshot().MQTTPassword != "typed" {
		t.Fatal("the password must be in use for its own broker")
	}
}

func TestEditedUrlRowDoesNotCarryThePasswordAlong(t *testing.T) {
	st := newMem()
	st.rows = boundRows()
	st.rows["MQTT_URL"] = store.SystemSetting{Key: "MQTT_URL", Value: "tcp://evil.example:1883"} // edited in the database
	var seen MQTTParams
	s := mqttTestSvcRows(t, st, nil, &seen)
	if s.Snapshot().MQTTPassword != "" {
		t.Fatal("the runtime must not get the password for another broker")
	}
	if _, err := s.TestMQTT(context.Background(), TestRequest{Actor: "a", StepUp: stepped}); err != nil {
		t.Fatal(err)
	}
	if seen.Password != "" {
		t.Fatalf("test connection sent the password to %s", seen.URL)
	}
	if !strings.Contains(strings.Join(mapValues(s.Skipped()), " "), "enter it again") {
		t.Fatalf("the page must say so: %v", s.Skipped())
	}
	var need bool
	for _, f := range s.State() {
		if f.Key == "MQTT_PASSWORD" && f.NeedsReentry {
			need = true
		}
	}
	if !need {
		t.Fatal("State must mark the password as needing re-entry")
	}
	// Pointing the URL back at the bound broker makes the stored password usable again.
	if _, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_URL": "tcp://broker.lan:1883"}}); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().MQTTPassword != "fake-secret-Zk3" || len(s.Skipped()) != 0 {
		t.Fatalf("password %q skipped %v", s.Snapshot().MQTTPassword, s.Skipped())
	}
}

// Once the environment was imported it is not a source any more: a changed
// environment URL cannot drag the stored password to another broker.
func TestEnvironmentUrlAfterTheImportIsIgnored(t *testing.T) {
	st := newMem()
	st.rows = boundRows()
	st.rows[importMarker] = store.SystemSetting{Key: importMarker, Value: "1"}
	var seen MQTTParams
	s := mqttTestSvcRows(t, st, []string{"TASMOFLEET_MQTT_URL=tcp://new-broker.lan:1883"}, &seen)
	if s.Snapshot().MQTTURL != "tcp://broker.lan:1883" || s.Snapshot().MQTTPassword != "fake-secret-Zk3" {
		t.Fatalf("url %s password %q", s.Snapshot().MQTTURL, s.Snapshot().MQTTPassword)
	}
}

func TestPasswordWithoutABindingFailsClosed(t *testing.T) {
	st := newMem()
	st.rows = boundRows()
	r := st.rows["MQTT_PASSWORD"]
	r.Target = "" // a row written by hand or by an older build
	st.rows["MQTT_PASSWORD"] = r
	var seen MQTTParams
	s := mqttTestSvcRows(t, st, nil, &seen)
	if s.Snapshot().MQTTPassword != "" {
		t.Fatal("a password that cannot prove its target must not be used")
	}
}

func mqttTestSvcRows(t *testing.T, st *memStore, env []string, seen *MQTTParams) *Service {
	t.Helper()
	cfg, err := loadCfg(append(baseEnv(), env...))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(context.Background(), Options{Base: cfg, Store: st, Audit: &memAudit{}, Now: func() time.Time { return time.Unix(5000, 0) },
		TestMQTT: func(_ context.Context, p MQTTParams) error { *seen = p; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Why: proportionate security. Routine settings (networks, MQTT, firmware,
// retention, audit retention) and harmless sign-in cosmetics (button name, user
// name claim, scopes, client ID; over-restriction audit #19) need no
// confirmation. Only what decides who signs in or with which rights does: mode,
// issuer, client ID, client secret, user name and groups claim, admin and
// operator groups. An account
// that cannot confirm (header authentication) cannot change those.
func TestOnlySignInSettingsNeedAStepUp(t *testing.T) {
	need := map[string]bool{}
	for _, f := range Fields() {
		need[f.Key] = f.StepUp != StepUpNever
	}
	for _, k := range []string{"AUTH_MODE", "OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_GROUPS_CLAIM", "OIDC_USERNAME_CLAIM", "ADMIN_GROUPS", "OPERATOR_GROUPS"} {
		if !need[k] {
			t.Errorf("%s must need a step-up", k)
		}
	}
	for _, k := range []string{"DEVICE_NETWORKS", "OIDC_SCOPES", "OIDC_DISPLAY_NAME", "MQTT_URL", "MQTT_USER", "MQTT_PASSWORD", "FIRMWARE_SOURCES", "FIRMWARE_ALLOW_SINGLE_SOURCE", "BACKUP_KEEP", "BACKUP_MAX_AGE", "AUDIT_RETENTION"} {
		if need[k] {
			t.Errorf("%s is routine and needs no step-up", k)
		}
	}
	s, _ := newSvc(t, newMem())
	_, err := s.Apply(context.Background(), Request{Actor: "a", Values: map[string]string{"OIDC_GROUPS_CLAIM": "x"}})
	var ne *StepUpRequiredError
	if !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: StepUp{Possible: false}, Values: map[string]string{"OIDC_GROUPS_CLAIM": "x"}}); err == nil {
		t.Fatal("an account that cannot confirm must be refused")
	}
	if _, err := s.Apply(context.Background(), Request{Actor: "a", Values: map[string]string{"DEVICE_NETWORKS": "10.0.0.0/8", "BACKUP_KEEP": "1", "MQTT_URL": "tcp://b:1883"}}); err != nil {
		t.Fatalf("routine settings: %v", err)
	}
}

// Why (basic rule kept): a sign-in secret goes only to the issuer it was entered for.
func TestChangingTheIssuerRequiresTheClientSecretAgain(t *testing.T) {
	st := newMem()
	st.rows["OIDC_ISSUER"] = store.SystemSetting{Key: "OIDC_ISSUER", Value: "https://idp.lan/application/o/x/"}
	st.rows["OIDC_CLIENT_SECRET"] = store.SystemSetting{Key: "OIDC_CLIENT_SECRET", Value: "fake-oidc-secret", Secret: true, Target: "https://idp.lan:443"}
	s, _ := newSvc(t, st, "TASMOFLEET_PUBLIC_URL=https://tf.example")
	if s.Snapshot().OIDCClientSecret != "fake-oidc-secret" {
		t.Fatal("the secret belongs to its issuer")
	}
	ctx := context.Background()
	_, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"OIDC_ISSUER": "https://evil.example/o/"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["OIDC_CLIENT_SECRET"] == "" {
		t.Fatalf("err = %v", err)
	}
	// An edited row does not carry the secret along either.
	st.rows["OIDC_ISSUER"] = store.SystemSetting{Key: "OIDC_ISSUER", Value: "https://evil.example/o/"}
	s2, _ := newSvc(t, st, "TASMOFLEET_PUBLIC_URL=https://tf.example")
	if s2.Snapshot().OIDCClientSecret != "" {
		t.Fatal("the secret must not follow an edited issuer")
	}
}

func TestEnvironmentLockedSignInSettingsCannotBeChanged(t *testing.T) {
	st := newMem()
	s, aud := newSvc(t, st, "TASMOFLEET_OIDC_CLIENT_ID=envid")
	_, err := s.Apply(context.Background(), Request{Actor: "m", StepUp: stepped, Values: map[string]string{"OIDC_CLIENT_ID": "x", "BACKUP_KEEP": "5"}})
	if !errors.Is(err, ErrLocked) || len(st.userRows()) != 0 || s.Snapshot().OIDCClientID != "envid" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(aud.last().Result, "denied") {
		t.Fatalf("a refused attempt must be audited: %+v", aud.last())
	}
	for _, f := range s.State() {
		if f.Key == "OIDC_CLIENT_ID" && (!f.Locked || f.Source != SourceEnv) {
			t.Fatalf("%+v", f)
		}
	}
}

func TestAuthModeAcceptsOnlyLocalAndOIDC(t *testing.T) {
	s, _ := newSvc(t, newMem(), "TASMOFLEET_PUBLIC_URL=https://tf.example")
	_, err := s.Apply(context.Background(), Request{Actor: "a", StepUp: stepped, Values: map[string]string{"AUTH_MODE": "header"}})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["AUTH_MODE"] == "" {
		t.Fatalf("header mode is environment-only: %v", err)
	}
}

func TestSSOTestIsRateLimitedAndAudited(t *testing.T) {
	cfg, err := loadCfg(baseEnv())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	var asked []string
	aud := &memAudit{}
	s, err := New(context.Background(), Options{Base: cfg, Store: newMem(), Audit: aud, Now: func() time.Time { return now },
		TestSSO: func(_ context.Context, issuer string) (string, error) {
			asked = append(asked, issuer)
			return "discovery OK", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.TestSSO(ctx, TestRequest{Actor: "a"}); err == nil {
		t.Fatal("no issuer: nothing to test")
	}
	msg, err := s.TestSSO(ctx, TestRequest{Actor: "a", Values: map[string]string{"OIDC_ISSUER": "https://user:pw@idp.example/"}})
	if err != nil || msg != "discovery OK" || aud.last().Action != "sysconfig.sso_test" || strings.Contains(aud.last().Detail, "pw") {
		t.Fatalf("%q %v %+v", msg, err, aud.last())
	}
	if _, err := s.TestSSO(ctx, TestRequest{Actor: "a", Values: map[string]string{"OIDC_ISSUER": "https://idp.example/"}}); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupMarkerIsRememberedAcrossRestarts(t *testing.T) {
	st := newMem()
	s, _ := newSvc(t, st)
	if s.SetupDone() {
		t.Fatal("a fresh install has not done the setup")
	}
	if err := s.MarkSetupDone(context.Background(), "admin"); err != nil || !s.SetupDone() {
		t.Fatalf("%v", err)
	}
	s2, _ := newSvc(t, st)
	if !s2.SetupDone() {
		t.Fatal("the marker must survive a restart")
	}
}

// Why (audit row 16, 2026-10-21): when the broker changes, the admin can say
// "keep the stored MQTT password" instead of typing it again. Without that
// choice the old refusal stands (the secret only follows its target when asked
// to), and the secret is bound to the new broker, so it is used there. Only secrets
// in reusableSecrets may follow (the OIDC client secret never does).
func TestStoredMQTTPasswordFollowsAChangedBrokerOnlyWhenAsked(t *testing.T) {
	st := newMem()
	st.rows["MQTT_URL"] = store.SystemSetting{Key: "MQTT_URL", Value: "tcp://broker.lan:1883"}
	s, _ := newSvc(t, st)
	ctx := context.Background()
	if _, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: map[string]string{"MQTT_PASSWORD": "stored-pw"}}); err != nil {
		t.Fatal(err)
	}
	move := map[string]string{"MQTT_URL": "tcp://other.lan:1883"}
	_, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: move})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["MQTT_PASSWORD"] == "" {
		t.Fatalf("without the choice the password must be asked for again: %v", err)
	}
	if _, err := s.Apply(ctx, Request{Actor: "a", StepUp: stepped, Values: move, Reuse: []string{"MQTT_PASSWORD"}}); err != nil {
		t.Fatalf("with the choice: %v", err)
	}
	if got := s.Snapshot(); got.MQTTURL != "tcp://other.lan:1883" || got.MQTTPassword != "stored-pw" {
		t.Fatalf("the password must be used at the new broker: %+v", got)
	}
	if row := st.rows["MQTT_PASSWORD"]; !strings.Contains(row.Target, "other.lan") {
		t.Fatalf("the stored secret must be bound to the new broker, target %q", row.Target)
	}
}

// Why: reuse is limited to the MQTT password; asking for it on another secret
// is refused rather than ignored.
func TestOnlyReusableSecretsMayFollowATarget(t *testing.T) {
	if !reusableSecrets["MQTT_PASSWORD"] || reusableSecrets["OIDC_CLIENT_SECRET"] {
		t.Fatalf("reusable secrets: %v", reusableSecrets)
	}
}
