package templates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/ota"
	"github.com/nofuturekid/tasmofleet/internal/policy"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// tplJSON is a Template reply of an ESP8266 (14 slots) with pin 1 set to code.
func tplJSON(name string, code int) string {
	g := make([]string, 14)
	for i := range g {
		g[i] = "0"
	}
	g[1] = fmt.Sprint(code)
	return fmt.Sprintf(`{"NAME":%q,"GPIO":[%s],"FLAG":0,"BASE":18}`, name, strings.Join(g, ","))
}

// fakeFleet is the devices and what they answer.
type fakeFleet struct {
	mu       sync.Mutex
	devs     []model.Device
	module   map[model.DeviceID]string // Module reply
	template map[model.DeviceID]string // Template reply
	failRead map[model.DeviceID]bool
	cmds     []string
	snap     map[model.DeviceID][]byte // newest snapshot
	powerOn  string
}

func newFleet() *fakeFleet {
	return &fakeFleet{module: map[model.DeviceID]string{}, template: map[model.DeviceID]string{}, failRead: map[model.DeviceID]bool{}, snap: map[model.DeviceID][]byte{}}
}

func (f *fakeFleet) add(name, tplName string, code int) model.DeviceID {
	id := model.DeviceID(len(f.devs) + 1)
	f.devs = append(f.devs, model.Device{ID: id, Name: name, Identity: model.Identity{Family: model.ChipESP8266, Variant: "tasmota", Hardware: "ESP8285N08"}})
	f.module[id] = `{"Module":{"0":"` + tplName + `"}}`
	f.template[id] = tplJSON(tplName, code)
	return id
}

func (f *fakeFleet) ListDevices(context.Context) ([]model.Device, error) { return f.devs, nil }
func (f *fakeFleet) GetDevice(_ context.Context, id model.DeviceID) (model.Device, error) {
	for _, d := range f.devs {
		if d.ID == id {
			return d, nil
		}
	}
	return model.Device{}, errors.New("not found")
}

// Command answers by device id found through the target's address.
func (f *fakeFleet) Command(_ context.Context, t tasmota.Target, cmnd string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := t.DeviceID()
	f.cmds = append(f.cmds, fmt.Sprintf("%d %s", id, cmnd))
	if f.failRead[id] {
		return nil, errors.New("timeout")
	}
	switch cmnd {
	case "Module":
		return json.RawMessage(f.module[id]), nil
	case "Template":
		return json.RawMessage(f.template[id]), nil
	}
	return nil, errors.New("unexpected " + cmnd)
}

func (f *fakeFleet) List(_ context.Context, id model.DeviceID) ([]model.Backup, error) {
	if _, ok := f.snap[id]; !ok {
		return nil, nil
	}
	return []model.Backup{{ID: model.BackupID(id), DeviceID: id, Kind: model.BackupSnapshot, TakenAt: t0.Add(-48 * time.Hour)}}, nil
}
func (f *fakeFleet) Open(_ context.Context, id model.BackupID) ([]byte, model.Backup, error) {
	return f.snap[model.DeviceID(id)], model.Backup{}, nil
}
func (f *fakeFleet) ReadKeys(context.Context, model.DeviceID, []string) (map[string]string, error) {
	return map[string]string{"PowerOnState": f.powerOn}, nil
}

type fakeEngine struct {
	start ota.TemplateStart
	reqs  []ota.TemplateRequest
	skip  []ota.Refusal
	calls int
}

func (e *fakeEngine) SubmitTemplate(_ context.Context, _ string, s ota.TemplateStart, r []ota.TemplateRequest) (model.BatchID, []ota.Refusal, error) {
	e.calls++
	e.start, e.reqs = s, r
	return 7, e.skip, nil
}

func newManager(f *fakeFleet, eng Submitter) *Manager {
	return New(Options{Devices: f, Client: f, Backups: f, Settings: f, Engine: eng, Now: func() time.Time { return t0 }})
}

var key = policy.GroupKey{Family: model.ChipESP8266, Name: "NOUS A1T"}

// Why: the point of the tab. Three plugs share a template name; two agree, one deviates. The group
// preselects the most common layout as the reference, the deviating device gets a plan with the diff
// and the exact command, and the matching one is marked as matching.
func TestOverviewGroupsByNameAndPlansAgainstTheMostCommonLayout(t *testing.T) {
	f := newFleet()
	f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	f.add("c", "NOUS A1T", 52)
	m := newManager(f, nil)
	ov, err := m.Overview(context.Background(), nil, true)
	if err != nil || len(ov.Groups) != 1 {
		t.Fatalf("%v %+v", err, ov)
	}
	g := ov.Groups[0]
	if g.Reference == nil || g.Reference.Device.ID != 1 || g.Reference.How != policy.RefMostCommon || g.NeedsReference {
		t.Fatalf("reference %+v", g.Reference)
	}
	by := map[model.DeviceID]*policy.ApplyPlan{}
	for _, d := range g.Devices {
		by[d.ID] = d.Plan
	}
	if !by[2].Matches || by[3].Matches || by[3].Refusal != policy.RefuseNone || len(by[3].Commands) != 1 || len(by[3].Diff) != 1 {
		t.Fatalf("plans: b %+v c %+v", by[2], by[3])
	}
	if !ov.ReadAt.Equal(t0) {
		t.Fatalf("ReadAt %v", ov.ReadAt)
	}
}

// Why (decision Q3): with a tie nothing is preselected, and a chosen reference is used for that request only.
func TestOverviewTieNeedsAReferenceAndAChosenOneIsUsed(t *testing.T) {
	f := newFleet()
	f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 52)
	m := newManager(f, nil)
	ov, _ := m.Overview(context.Background(), nil, true)
	if g := ov.Groups[0]; !g.NeedsReference || g.Reference != nil || g.Devices[0].Plan != nil {
		t.Fatalf("a tie must not guess: %+v", g)
	}
	ov, _ = m.Overview(context.Background(), Refs{key: 2}, false)
	if g := ov.Groups[0]; g.Reference == nil || g.Reference.Device.ID != 2 || g.Reference.How != policy.RefChosen || g.Devices[0].Plan.Matches {
		t.Fatalf("chosen: %+v", g)
	}
	ov, _ = m.Overview(context.Background(), Refs{key: 99}, false)
	if !ov.Groups[0].NeedsReference {
		t.Fatal("a forged reference id must not become the reference")
	}
}

// Why (Q2, E5): a built-in module is only listed; a device without a live reading shows its backup with the
// backup's date and is never planned; a device never read is "not judged" with a reason.
func TestOverviewFoldsBuiltInBackupAndUnreadDevices(t *testing.T) {
	f := newFleet()
	f.add("a", "NOUS A1T", 21)
	f.add("b", "NOUS A1T", 21)
	builtin := f.add("builtin", "x", 0)
	f.module[builtin] = `{"Module":{"1":"Sonoff Basic"}}`
	fromBackup := f.add("old", "NOUS A1T", 21)
	f.failRead[fromBackup] = true
	f.snap[fromBackup] = []byte(`{"Module":` + f.module[fromBackup] + `,"Template":` + f.template[fromBackup] + `}`)
	unread := f.add("unread", "NOUS A1T", 21)
	f.failRead[unread] = true
	m := newManager(f, nil)
	ov, _ := m.Overview(context.Background(), nil, true)
	if len(ov.BuiltIn) != 1 || ov.BuiltIn[0].Key.Name != "Sonoff Basic" || ov.BuiltIn[0].Devices[0].Plan != nil {
		t.Fatalf("built-in %+v", ov.BuiltIn)
	}
	var old DeviceView
	for _, d := range ov.Groups[0].Devices {
		if d.ID == fromBackup {
			old = d
		}
	}
	if old.Source != SourceBackup || !old.ReadAt.Equal(t0.Add(-48*time.Hour)) || old.Plan == nil || old.Plan.Refusal != policy.RefuseNotLive {
		t.Fatalf("backup device %+v plan %+v", old, old.Plan)
	}
	if len(ov.NotJudged) != 1 || ov.NotJudged[0].ID != unread || ov.NotJudged[0].NotJudgedReason == "" {
		t.Fatalf("not judged %+v", ov.NotJudged)
	}
}

// Why: a rapid second refresh must not hammer the fleet; it answers from the cache and says so.
func TestRefreshIsRateLimited(t *testing.T) {
	f := newFleet()
	f.add("a", "NOUS A1T", 21)
	m := newManager(f, nil)
	_, _ = m.Overview(context.Background(), nil, true)
	n := len(f.cmds)
	ov, _ := m.Overview(context.Background(), nil, true)
	if !ov.Throttled || len(f.cmds) != n {
		t.Fatalf("throttled %v, %d new commands", ov.Throttled, len(f.cmds)-n)
	}
}

func fleet3(t *testing.T) (*fakeFleet, *Manager, *fakeEngine) {
	f := newFleet()
	f.add("ref", "NOUS A1T", 21)
	f.add("dev", "NOUS A1T", 52)
	f.add("same", "NOUS A1T", 21)
	f.powerOn = "3"
	eng := &fakeEngine{}
	return f, newManager(f, eng), eng
}

// Why: the preview is judged on fresh reads, not on the cache: the device changed since the overview.
func TestPreviewReadsTheDevicesAgain(t *testing.T) {
	f, m, _ := fleet3(t)
	_, _ = m.Overview(context.Background(), nil, true)
	f.template[2] = tplJSON("NOUS A1T", 99) // changed since the overview
	pv, err := m.Preview(context.Background(), Request{Group: key, Reference: 1, Devices: []model.DeviceID{2, 3, 1}})
	if err != nil || len(pv.Rows) != 1 || pv.Rows[0].Device.Reading.Template.GPIO[1] != 99 {
		t.Fatalf("%v %+v", err, pv.Rows)
	}
	if pv.Rows[0].Device.PowerOnStateText == "" || !strings.Contains(pv.Rows[0].Plan.Commands[0], `"GPIO":[0,21,`) {
		t.Fatalf("row %+v", pv.Rows[0])
	}
	why := map[model.DeviceID]string{}
	for _, e := range pv.Excluded {
		why[e.Device.ID] = e.Why
	}
	if why[1] != "is the reference" || why[3] != "matches the reference" || pv.Applicable != 1 || pv.Digest == "" {
		t.Fatalf("excluded %v applicable %d", why, pv.Applicable)
	}
}

// Why: forged ids must not reach the engine: a device id that is not in the inventory is dropped, a
// reference that is not on the group's template is refused, a built-in group has no template to apply.
func TestPreviewDropsForgedIDsAndRefusesABadReference(t *testing.T) {
	_, m, _ := fleet3(t)
	pv, err := m.Preview(context.Background(), Request{Group: key, Reference: 1, Devices: []model.DeviceID{2, 4242}})
	if err != nil || len(pv.Rows) != 1 || len(pv.Excluded) != 0 {
		t.Fatalf("%v %+v", err, pv)
	}
	for _, r := range []Request{
		{Group: key, Reference: 4242},
		{Group: policy.GroupKey{Family: model.ChipESP8266, Name: "Other"}, Reference: 1},
		{Group: policy.GroupKey{Family: model.ChipESP32, Name: "NOUS A1T"}, Reference: 1},
		{Group: policy.GroupKey{Family: model.ChipESP8266, Name: "NOUS A1T", BuiltIn: true}, Reference: 1},
	} {
		if _, err := m.Preview(context.Background(), r); err == nil {
			t.Errorf("%+v accepted", r)
		}
	}
}

// Why: a device from another group among the ticked ones is never sent this group's template.
func TestPreviewExcludesADeviceOfAnotherGroup(t *testing.T) {
	f, m, _ := fleet3(t)
	f.template[2] = tplJSON("Other name", 52)
	pv, _ := m.Preview(context.Background(), Request{Group: key, Reference: 1, Devices: []model.DeviceID{2}})
	if len(pv.Rows) != 0 || len(pv.Excluded) != 1 || pv.Excluded[0].Refusal != policy.RefuseOtherGroup {
		t.Fatalf("%+v", pv)
	}
}

// Why (decision: confirm tick + digest): Apply re-reads, and when the devices differ from what the
// preview showed it refuses and sends nothing.
func TestApplyRefusesAStaleDigestAndSendsNothing(t *testing.T) {
	f, m, eng := fleet3(t)
	req := Request{Group: key, Reference: 1, Devices: []model.DeviceID{2}}
	pv, _ := m.Preview(context.Background(), req)
	f.template[2] = tplJSON("NOUS A1T", 77)
	if _, _, err := m.Apply(context.Background(), "alice", req, pv.Digest); !errors.Is(err, ErrStaleDigest) || eng.calls != 0 {
		t.Fatalf("err %v, engine calls %d", err, eng.calls)
	}
	pv, _ = m.Preview(context.Background(), req)
	if _, _, err := m.Apply(context.Background(), "alice", req, pv.Digest); err != nil || eng.calls != 1 {
		t.Fatalf("err %v, calls %d", err, eng.calls)
	}
}

// Why: what the engine gets is exactly the plan: the command text, the layout the device had, the
// layout it must end with, and the audit facts of how the reference was chosen.
func TestApplyHandsThePlanToTheEngine(t *testing.T) {
	_, m, eng := fleet3(t)
	req := Request{Group: key, Reference: 1, Devices: []model.DeviceID{2, 3}, Chosen: true}
	pv, _ := m.Preview(context.Background(), req)
	id, refused, err := m.Apply(context.Background(), "alice", req, pv.Digest)
	if err != nil || id != 7 || len(refused) != 0 || len(eng.reqs) != 1 {
		t.Fatalf("%v %v %v %+v", id, refused, err, eng.reqs)
	}
	r := eng.reqs[0]
	if r.DeviceID != 2 || r.Command != pv.Rows[0].Plan.Commands[0] || r.Have != pv.Rows[0].Device.Reading.Template.Fingerprint() ||
		r.Want != pv.ReferenceTemplate.Fingerprint() || r.Have == r.Want {
		t.Fatalf("request %+v", r)
	}
	if eng.start.Reference != 1 || eng.start.How != "chosen" || !strings.Contains(eng.start.Group, "NOUS A1T") {
		t.Fatalf("start %+v", eng.start)
	}
}

// Why: with nothing to send there is no empty transaction, and a server without an engine says so.
func TestApplyWithNothingToSendOrNoEngine(t *testing.T) {
	_, m, _ := fleet3(t)
	req := Request{Group: key, Reference: 1, Devices: []model.DeviceID{3}}
	pv, _ := m.Preview(context.Background(), req)
	if _, _, err := m.Apply(context.Background(), "alice", req, pv.Digest); !errors.Is(err, ErrNothingToApply) {
		t.Fatalf("err %v", err)
	}
	m.o.Engine = nil
	if _, _, err := m.Apply(context.Background(), "alice", req, pv.Digest); !errors.Is(err, ErrNoEngine) {
		t.Fatalf("err %v", err)
	}
}

// Why: the digest must change with anything the apply depends on, or it protects nothing.
func TestDigestChangesWithTheReferenceTheDeviceAndTheSelection(t *testing.T) {
	f, m, _ := fleet3(t)
	base := Request{Group: key, Reference: 1, Devices: []model.DeviceID{2}}
	pv, _ := m.Preview(context.Background(), base)
	again, _ := m.Preview(context.Background(), base)
	if pv.Digest != again.Digest {
		t.Fatal("the digest of an unchanged fleet must be stable")
	}
	f.template[1] = tplJSON("NOUS A1T", 22)
	changedRef, _ := m.Preview(context.Background(), base)
	if changedRef.Digest == pv.Digest {
		t.Error("a changed reference layout must change the digest")
	}
	f.template[1] = tplJSON("NOUS A1T", 21)
	f.template[3] = tplJSON("NOUS A1T", 52)
	more, _ := m.Preview(context.Background(), Request{Group: key, Reference: 1, Devices: []model.DeviceID{2, 3}})
	if more.Digest == pv.Digest {
		t.Error("another selection must change the digest")
	}
}
