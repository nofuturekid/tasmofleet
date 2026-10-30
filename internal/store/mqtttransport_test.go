package store

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/secretbox"
)

// Why: live databases hold devices, finished and queued jobs and restores. After the upgrade they
// must behave exactly as before MQTT transport: the device password is "not set" (we never assume
// the fleet convention), the firmware server is "unknown" (so it is tried, not refused), the
// language is not a fact until the next HTTP probe, and old jobs are HTTP jobs without the
// experimental upload (a resumed old job must not suddenly be allowed to push over MQTT).
func TestMigration0036KeepsExistingRowsAsBefore(t *testing.T) {
	db, path := rawDB(t)
	upTo, all := fstest.MapFS{}, fstest.MapFS{}
	err := fs.WalkDir(Migrations, "migrations", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(Migrations, p)
		if rerr != nil {
			return rerr
		}
		all[p] = &fstest.MapFile{Data: b}
		if p < "migrations/0036" {
			upTo[p] = &fstest.MapFile{Data: b}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(db, path, upTo); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO devices (name, address, mac, topic, fw_language, created_at, updated_at) VALUES ('old','10.0.0.1','aa:bb:cc:00:00:09','t','DE',0,0)`,
		`INSERT INTO ota_batches (created_by, created_at, retry_of) VALUES ('u',0,0)`,
		`INSERT INTO ota_jobs (batch_id, device_id, state, stage, plan, policy, target_release, target_variant, identity_before, delivery, created_by, created_at, updated_at) VALUES (1,1,'queued','final','direct','only-newer','16.0.0','tasmota','{}','','u',0,0)`,
		`INSERT INTO restore_jobs (device_id, backup_id, state, actor, created_at, updated_at) VALUES (1,1,'pending','u',0,0)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(db, path, all); err != nil {
		t.Fatal(err)
	}
	var mode, reach, sealed string
	var bad, reachAt *int64
	var lang, upload int
	if err := db.QueryRowContext(ctx, `SELECT mqtt_password, mqtt_password_mode, mqtt_password_bad_at, fw_reach, fw_reach_at, identity_lang_known FROM devices WHERE id=1`).
		Scan(&sealed, &mode, &bad, &reach, &reachAt, &lang); err != nil {
		t.Fatal(err)
	}
	if sealed != "" || mode != "" || bad != nil || reach != "" || reachAt != nil || lang != 0 {
		t.Fatalf("old device: password %q mode %q bad %v reach %q/%v lang %d", sealed, mode, bad, reach, reachAt, lang)
	}
	var jt, rt string
	if err := db.QueryRowContext(ctx, `SELECT transport, mqtt_upload FROM ota_jobs WHERE id=1`).Scan(&jt, &upload); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT transport FROM restore_jobs WHERE id=1`).Scan(&rt); err != nil {
		t.Fatal(err)
	}
	if jt != "" || upload != 0 || rt != "" {
		t.Fatalf("old jobs: transport %q upload %d, restore transport %q", jt, upload, rt)
	}
	// Through the store: the language the device had stays, but is not yet a fact.
	box, err := secretbox.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() }) //nolint:errcheck // cleanup of a temp DB
	d, err := s.GetDevice(ctx, 1)
	if err != nil || d.Identity.Language != "DE" || d.Identity.LanguageKnown || d.MQTTPasswordMode != model.MQTTPasswordNone ||
		d.FWReach != (model.FWReach{}) || !d.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("old device = %+v, %v", d, err)
	}
	j, err := s.GetJob(ctx, 1)
	if err != nil || j.Transport != model.TransportHTTP || j.MQTTUpload {
		t.Fatalf("old job transport %q upload %v, %v", j.Transport, j.MQTTUpload, err)
	}
}

// Why: the device's MqttPassword is a secret that goes into FileDownload/FileUpload payloads. It
// must come back exactly, and the database file must never hold it in clear (a copied backup of
// the DB must not leak it). Switching to the broker mode must also drop a stored own value.
func TestMQTTPasswordIsSealedAtRest(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, "s3cret-mq"); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil || d.MQTTPassword != "s3cret-mq" || d.MQTTPasswordMode != model.MQTTPasswordOwn {
		t.Fatalf("round trip = %q/%q, %v", d.MQTTPassword, d.MQTTPasswordMode, err)
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, "SELECT mqtt_password FROM devices WHERE id=?", id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw == "" || strings.Contains(raw, "s3cret-mq") {
		t.Fatalf("column holds %q", raw)
	}
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordBroker, "ignored"); err != nil {
		t.Fatal(err)
	}
	d, _ = s.GetDevice(ctx, id)
	if d.MQTTPassword != "" || d.MQTTPasswordMode != model.MQTTPasswordBroker {
		t.Fatalf("broker mode kept %q/%q", d.MQTTPassword, d.MQTTPasswordMode)
	}
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, ""); err == nil {
		t.Fatal("an empty own password was accepted")
	}
	if err := s.SetMQTTPassword(ctx, id, "bogus", "x"); err == nil {
		t.Fatal("an unknown mode was accepted")
	}
	if err := s.SetMQTTPassword(ctx, 999, model.MQTTPasswordNone, ""); err == nil {
		t.Fatal("unknown device accepted")
	}
}

// Why: an inventory edit holds a copy of the device from when the form opened. If it wrote the
// MQTT password, FW reach or language flag back, an edit of the notes would wipe a password the
// admin just set, or "forget" what a failed upgrade taught us.
func TestUpdateDeviceKeepsMQTTFields(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	stale, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordOwn, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFWReach(ctx, id, model.FWReach{State: model.FWReachNo, At: at, Detail: "connection failed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateIdentity(ctx, id, model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", LanguageKnown: true}); err != nil {
		t.Fatal(err)
	}
	stale.Notes = "edited"
	if err := s.UpdateDevice(ctx, stale); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDevice(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Notes != "edited" || d.MQTTPassword != "pw" || d.MQTTPasswordMode != model.MQTTPasswordOwn ||
		d.FWReach != (model.FWReach{State: model.FWReachNo, At: at, Detail: "connection failed"}) || !d.Identity.LanguageKnown {
		t.Fatalf("after edit: %+v", d)
	}
}

// Why: "Error 1" must be remembered (so the UI asks for the password instead of retrying), and
// saving a password must clear it (the admin fixed the cause). FWReach "Forget" resets to unknown.
func TestMQTTPasswordBadMarkAndFWReachReset(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	if err := s.MarkMQTTPasswordBad(ctx, id, at); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordBadAt.Equal(at) {
		t.Fatalf("bad at = %v", d.MQTTPasswordBadAt)
	}
	if err := s.SetMQTTPassword(ctx, id, model.MQTTPasswordBroker, ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.MQTTPasswordBadAt.IsZero() {
		t.Fatalf("saving the password kept the bad mark %v", d.MQTTPasswordBadAt)
	}
	if err := s.SetFWReach(ctx, id, model.FWReach{State: model.FWReachYes, At: at}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFWReach(ctx, id, model.FWReach{}); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); d.FWReach != (model.FWReach{}) {
		t.Fatalf("forget left %+v", d.FWReach)
	}
	if err := s.MarkMQTTPasswordBad(ctx, 999, at); err == nil {
		t.Fatal("unknown device accepted")
	}
}

// Why: whether a job went over MQTT and whether the experimental upload was allowed must survive
// a restart of tasmofleet, or a resumed job would pick another path than the one the person agreed
// to in the preview; the same for a restore.
func TestJobAndRestoreTransportRoundTrip(t *testing.T) {
	s, _, _ := newStore(t)
	dev := mustDevice(t, s, "a", "10.0.0.1")
	j := job(dev)
	j.MQTTUpload = true
	_, ids, err := s.CreateBatch(ctx, "u", []model.Job{j})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetJob(ctx, ids[0])
	if err != nil || !got.MQTTUpload || got.Transport != model.TransportHTTP {
		t.Fatalf("created: upload %v transport %q, %v", got.MQTTUpload, got.Transport, err)
	}
	// The transport is recorded at preflight, a later transition.
	prev := got.State
	got.State, got.Transport = model.JobUpgradeIssued, model.TransportMQTT
	if err := s.SaveTransition(ctx, got, model.JobEvent{JobID: got.ID, From: prev, To: got.State, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetJob(ctx, ids[0]); err != nil || got.Transport != model.TransportMQTT || !got.MQTTUpload {
		t.Fatalf("after transition: transport %q upload %v, %v", got.Transport, got.MQTTUpload, err)
	}
	// An upload that ended up uncertain must stay so across a restart (it decides the failure text).
	prev, got.MQTTUploadState = got.State, model.MQTTUploadUnconfirmed
	if err := s.SaveTransition(ctx, got, model.JobEvent{JobID: got.ID, From: prev, To: got.State, Event: "x"}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.GetJob(ctx, ids[0]); err != nil || got.MQTTUploadState != model.MQTTUploadUnconfirmed {
		t.Fatalf("upload state %q, %v", got.MQTTUploadState, err)
	}
	rid, err := s.CreateRestore(ctx, RestoreJob{DeviceID: dev, BackupID: 1, Actor: "u"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRestore(ctx, rid)
	if err != nil || r.Transport != model.TransportHTTP {
		t.Fatalf("restore transport %q, %v", r.Transport, err)
	}
	// Recorded when the restore starts, so it is the update that must carry it.
	r.State, r.Transport = RestoreSending, model.TransportMQTT
	if err := s.UpdateRestore(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r, _ = s.GetRestore(ctx, rid); r.Transport != model.TransportMQTT {
		t.Fatalf("update lost transport: %q", r.Transport)
	}
	// A restore created over MQTT keeps it too.
	rid2, err := s.CreateRestore(ctx, RestoreJob{DeviceID: mustDevice(t, s, "b", "10.0.0.2"), BackupID: 1, Actor: "u", Transport: model.TransportMQTT})
	if err != nil {
		t.Fatal(err)
	}
	if r2, _ := s.GetRestore(ctx, rid2); r2.Transport != model.TransportMQTT {
		t.Fatalf("created restore transport %q", r2.Transport)
	}
}

// Why: the probe runs before every OTA and is what turns the language into a fact; if it did not
// store the flag, every update would keep asking for a language choice.
func TestProbeStoresLanguageKnown(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	err := s.UpdateProbe(ctx, id, ProbeGuard{Address: "10.0.0.1"}, ProbeResult{
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", Language: "DE", LanguageKnown: true}})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.Identity.LanguageKnown || d.Identity.Language != "DE" {
		t.Fatalf("identity = %+v", d.Identity)
	}
}

// Why: the language flag says "an HTTP probe read this language". A device created or moved onto
// new hardware with a probed identity must keep that fact, or the next update asks for a language
// choice that was already answered.
func TestCreateAndSwapStoreLanguageKnown(t *testing.T) {
	s, _, _ := newStore(t)
	id, err := s.CreateDevice(ctx, model.Device{Name: "c", Address: "10.0.0.5", MAC: "aa:bb:cc:00:00:21",
		Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", LanguageKnown: true}})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDevice(ctx, id); !d.Identity.LanguageKnown {
		t.Fatal("CreateDevice lost LanguageKnown")
	}
	// The swap writes the identity of the new board and forgets what the old one reached.
	if err := s.SetFWReach(ctx, id, model.FWReach{State: model.FWReachYes, At: time.Unix(100, 0), Detail: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SwapHardware(ctx, id, HardwareSwap{ExpectMACs: []string{"aa:bb:cc:00:00:21"}, MAC: "aa:bb:cc:00:00:22",
		Address: "10.0.0.6", Port: 80, Identity: model.Identity{Family: model.ChipESP32, Version: "16.0.0", Variant: "tasmota", LanguageKnown: true}}); err != nil {
		t.Fatal(err)
	}
	d, _ := s.GetDevice(ctx, id)
	if !d.Identity.LanguageKnown {
		t.Fatal("SwapHardware lost LanguageKnown")
	}
	// Why: "yes" is tied to the device's IP (D11); new hardware may sit elsewhere.
	if d.FWReach != (model.FWReach{}) {
		t.Fatalf("swap kept FWReach %+v", d.FWReach)
	}
}

// Why: a typo must not store a state the chooser then treats as neither yes nor no; and a bad
// mark without a time would read as "never marked".
func TestMQTTFieldWritersRejectBadInput(t *testing.T) {
	s, _, _ := newStore(t)
	id := mustDevice(t, s, "a", "10.0.0.1")
	if err := s.SetFWReach(ctx, id, model.FWReach{State: "maybe"}); err == nil {
		t.Fatal("unknown FWReach state accepted")
	}
	if err := s.MarkMQTTPasswordBad(ctx, id, time.Time{}); err == nil {
		t.Fatal("zero bad time accepted")
	}
}
