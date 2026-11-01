package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/simulator"
	"github.com/nofuturekid/tasmofleet/internal/store"
	"github.com/nofuturekid/tasmofleet/internal/tasmota"
	"github.com/nofuturekid/tasmofleet/internal/transport"
)

// secretToken stands for what Berry scripts routinely embed. It must never reach a log line, an
// audit record or a job detail: the pages show names and sizes only.
const secretToken = "tok_SECRET_do_not_log_123"

func fsFiles(m map[string]string) map[string]simulator.FSFile {
	out := map[string]simulator.FSFile{}
	for p, c := range m {
		out[p] = simulator.FSFile{Data: []byte(c)}
	}
	return out
}

// backupFiles is what the device holds when the backup is taken.
var backupFiles = map[string]string{
	"/autoexec.be": "print('" + secretToken + "')",
	"/lib/a.be":    "# library a",
	"/tlskey":      "FAKE-TLS-KEY",
}

// filesFlow is a flow with an ESP32 device that holds backupFiles, a backup of it (with its files
// archive), and a device that then drifted: every file changed and one file the backup lacks.
func filesFlow(t *testing.T) (*flowEnv, model.Backup) {
	t.Helper()
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: fsFiles(backupFiles)})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	for p := range backupFiles {
		e.setDevFile(t, p, "drifted "+p)
	}
	e.setDevFile(t, "/extra.txt", "not in the backup")
	return e, a
}

func (e *flowEnv) target() tasmota.Target { return tasmota.TargetFor(e.device) }

func (e *flowEnv) devFile(t *testing.T, p string) string {
	t.Helper()
	b, err := e.cl.fs.DownloadFile(t.Context(), e.target(), p, 0)
	if err != nil {
		t.Fatalf("read %s from the device: %v", p, err)
	}
	return string(b)
}

func (e *flowEnv) setDevFile(t *testing.T, p, content string) {
	t.Helper()
	if err := e.cl.fs.UploadFile(t.Context(), e.target(), p, []byte(content)); err != nil {
		t.Fatalf("seed %s: %v", p, err)
	}
}

func progressOf(t *testing.T, e *flowEnv, id store.RestoreID) FilesProgress {
	t.Helper()
	p, ok := e.m.FilesProgress(id)
	if !ok {
		t.Fatal("no files progress for the job")
	}
	return p
}

func stateOf(p FilesProgress, name string) FileState {
	for _, f := range p.Files {
		if f.Name() == name {
			return f.State
		}
	}
	return "?"
}

func assertNoSecret(t *testing.T, e *flowEnv, j Job) {
	t.Helper()
	if strings.Contains(j.Detail, secretToken) {
		t.Errorf("file content in the job detail: %q", j.Detail)
	}
	for _, ev := range e.audit.events() {
		if strings.Contains(ev.Detail, secretToken) || strings.Contains(ev.Target, secretToken) || strings.Contains(ev.Result, secretToken) {
			t.Errorf("file content in an audit record: %+v", ev)
		}
	}
}

// Why: Berry scripts and autoexec.be load at the restart of the settings restore, so the files
// must be on the device before the settings go over, in one sequence; a file the device already
// holds in the backup's version costs no flash write; and nothing the backup lacks is touched.
func TestRestoreWritesTheFilesBeforeTheSettings(t *testing.T) {
	e, a := filesFlow(t)
	e.setDevFile(t, "/tlskey", backupFiles["/tlskey"]) // the key is already right: no write needed

	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified {
		t.Fatalf("job: %+v", done)
	}
	for _, p := range []string{"/autoexec.be", "/lib/a.be"} {
		if got := e.devFile(t, p); got != backupFiles[p] {
			t.Errorf("%s on the device: %q, want the backup's content", p, got)
		}
	}
	if got := e.devFile(t, "/extra.txt"); got != "not in the backup" {
		t.Errorf("a file the backup lacks was touched: %q", got)
	}
	if up := e.cl.uploads(); strings.Join(up, " ") != "/autoexec.be /lib/a.be" {
		t.Errorf("uploads %v: want the two changed files only, in path order", up)
	}
	e.cl.mu.Lock()
	order := append([]string(nil), e.cl.order...)
	e.cl.mu.Unlock()
	if len(order) < 3 || strings.Join(order[len(order)-3:], " ") != "upload:/autoexec.be upload:/lib/a.be WebGetConfig" {
		t.Errorf("files must reach the device before WebGetConfig: %v", order)
	}
	pr := progressOf(t, e, job.ID)
	if pr.Writes != 2 || stateOf(pr, "autoexec.be") != FileWritten || stateOf(pr, "tlskey") != FileSame {
		t.Errorf("progress: %+v", pr)
	}
	files, rest := SplitDetail(done.Detail)
	if !strings.Contains(files, "2 files written") || !strings.Contains(files, "2 flash writes") || !strings.Contains(files, "1 already") {
		t.Errorf("files summary %q", files)
	}
	if !strings.Contains(rest, "3 files match") {
		t.Errorf("the verification must compare the files with the backup: %q", rest)
	}
	assertNoSecret(t, e, done)
}

// Why: a failed upload can lose the file (the device deletes the old one first). The restore keeps
// the file it read before writing and puts it back, writes nothing of the settings, and says that
// files were changed so the page offers the rollback.
func TestFailedUploadPutsTheOldFileBack(t *testing.T) {
	e, a := filesFlow(t)
	e.cl.uploadHook = func(p string, attempt int) error {
		if p == "/lib/a.be" && attempt == 1 {
			// what the device does: the old file is gone, the new one is cut short
			if err := e.cl.fs.UploadFile(t.Context(), e.target(), p, []byte("PARTIAL")); err != nil {
				t.Error(err)
			}
			return errors.New("link lost")
		}
		return nil
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepFiles || !FilesChanged(done.Detail) {
		t.Fatalf("job: %+v", done)
	}
	if got := e.devFile(t, "/lib/a.be"); got != "drifted /lib/a.be" {
		t.Errorf("the old file was not put back: %q", got)
	}
	if got := e.devFile(t, "/autoexec.be"); got != backupFiles["/autoexec.be"] {
		t.Errorf("a file written before the failure keeps the backup's content: %q", got)
	}
	if got := e.devFile(t, "/tlskey"); got != "drifted /tlskey" {
		t.Errorf("a file after the failure must not be touched: %q", got)
	}
	if got := e.cl.sent("WebGetConfig"); len(got) != 0 {
		t.Errorf("settings sent although the files failed: %v", got)
	}
	if !bytes.Equal(e.deviceDump(t), e.dumpB) {
		t.Error("the settings changed")
	}
	// autoexec.be, the failed a.be, the put-back: three flash writes, no retry loop
	if n := len(e.cl.uploads()); n != 3 {
		t.Errorf("uploads %v: want exactly 3 (one file, the failed attempt, one put-back)", e.cl.uploads())
	}
	pr := progressOf(t, e, job.ID)
	if pr.Writes != 3 || stateOf(pr, "lib/a.be") != FileFailed || stateOf(pr, "tlskey") != FileWaiting || len(pr.Lost) != 0 {
		t.Errorf("progress: %+v", pr)
	}
	assertNoSecret(t, e, done)
}

// Why: if the old file cannot be put back either, the person must be told loudly which file is
// gone, not "failed" in general.
func TestFileThatCannotBePutBackIsReportedAsLost(t *testing.T) {
	e, a := filesFlow(t)
	e.cl.uploadHook = func(p string, attempt int) error {
		if p != "/lib/a.be" {
			return nil
		}
		if attempt == 1 {
			_ = e.cl.fs.UploadFile(t.Context(), e.target(), p, []byte("PARTIAL"))
		}
		return errors.New("device refuses")
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || !FilesChanged(done.Detail) || !strings.Contains(done.Detail, "lib/a.be") || !strings.Contains(strings.ToLower(done.Detail), "lost") {
		t.Fatalf("the lost file must be named: %q", done.Detail)
	}
	if pr := progressOf(t, e, job.ID); len(pr.Lost) != 1 || pr.Lost[0] != "lib/a.be" {
		t.Errorf("lost: %+v", pr.Lost)
	}
}

// Why: the confirmation page says before the person confirms what will be written, what stays
// because the backup does not hold it, and the size.
func TestAssessListsTheFilesAndWhatStaysAsItIs(t *testing.T) {
	files := fsFiles(backupFiles)
	files["/big.bin"] = simulator.FSFile{Data: bytes.Repeat([]byte{1}, 1100*1024)}
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: files, FSSizeKB: 4096})
	a := e.dumpBackup(t, e.dumpA)
	as, err := e.m.Assess(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !as.Files.Restores() || len(as.Files.Files) != 3 || as.Files.Files[0].Name() != "autoexec.be" {
		t.Fatalf("files plan: %+v", as.Files)
	}
	var key bool
	for _, f := range as.Files.Files {
		key = key || (f.Name() == "tlskey" && f.PrivateKey)
	}
	if !key {
		t.Error("tlskey must be marked as the private key")
	}
	if len(as.Files.NotStored) != 1 || as.Files.NotStored[0].Path != "/big.bin" {
		t.Errorf("a file over the limit stays on the device and is listed: %+v", as.Files.NotStored)
	}
}

// Why: a device without a file system (ESP8266 1 MB, minimal) restores its settings as before,
// without a single file system request, and the page says why there are no files.
func TestDeviceWithoutAFileSystemRestoresSettingsOnly(t *testing.T) {
	e := newFlow(t, "15.6.0")
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	as, err := e.m.Assess(t.Context(), a.ID)
	if err != nil || as.Files.Restores() || as.Files.Note == "" {
		t.Fatalf("assessment %+v %v: want no files and the reason", as.Files, err)
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified || !bytes.Equal(e.deviceDump(t), e.dumpA) {
		t.Fatalf("job: %+v", done)
	}
	if up := e.cl.uploads(); len(up) != 0 {
		t.Errorf("uploads on a device without a file system: %v", up)
	}
}

// Why: files cannot travel over MQTT. A restore routed over MQTT puts the settings back, writes no
// file, and the confirmation page says so before the person confirms.
func TestRestoreOverMQTTPutsBackSettingsOnlyAndSaysSo(t *testing.T) {
	e, a := filesFlow(t)
	u := &fakeUploader{e: e, apply: true}
	m := e.mqttManager(t, u, transport.MQTT)
	as, err := m.Assess(t.Context(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if as.Files.Restores() || !strings.Contains(as.Files.Why, "HTTP") || len(as.Files.Files) != 3 {
		t.Fatalf("assessment: %+v", as.Files)
	}
	job, err := m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !bytes.Equal(e.deviceDump(t), e.dumpA) {
		t.Fatalf("job: %+v", done)
	}
	if up := e.cl.uploads(); len(up) != 0 {
		t.Errorf("a file was written over MQTT: %v", up)
	}
	if files, _ := SplitDetail(done.Detail); !strings.Contains(files, "HTTP") {
		t.Errorf("the finished job must say the files were not restored: %q", done.Detail)
	}
	if got := e.devFile(t, "/autoexec.be"); got != "drifted /autoexec.be" {
		t.Errorf("file changed over MQTT: %q", got)
	}
}

// replaceFilesFlow is an ESP32 entry with a backup that has files and a fresh ESP32 whose file
// system holds only what the spec names.
func replaceFilesFlow(t *testing.T, fresh map[string]string) *replaceEnv {
	t.Helper()
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: fsFiles(backupFiles)})
	sd, err := e.fleet.Add(t.Context(), simulator.DeviceSpec{Name: "spare", Profile: simulator.ProfileESP32, Version: "16.0.0", Files: fsFiles(fresh)})
	if err != nil {
		t.Fatal(err)
	}
	ctl, _ := simulator.Control(sd)
	host, portStr, _ := net.SplitHostPort(sd.Addr())
	port, _ := strconv.Atoi(portStr)
	return &replaceEnv{flowEnv: e, newSim: ctl, newHost: host, newPort: port, newMAC: ctl.MAC()}
}

// Why: Replace is the main use of file backups: the new board gets autoexec.be and the scripts
// with the settings. A fresh board lacks the /lib folder and no binary-safe folder creation
// exists, so those files are skipped with the reason instead of failing the replacement.
func TestReplaceOntoAFreshDeviceWritesFilesAndReportsMissingFolders(t *testing.T) {
	e := replaceFilesFlow(t, map[string]string{"/autoexec.be": "factory"})
	a := e.dumpBackup(t, e.dumpA)
	e.newSim.SetConfigDump(e.dumpB)
	e.breakOld(t)
	newTarget := tasmota.TargetFor(model.Device{Address: e.newHost, Port: e.newPort})

	job, err := e.m.StartReplace(t.Context(), "alice", e.req(a.ID), StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || !done.Verified {
		t.Fatalf("job: %+v", done)
	}
	for p, want := range map[string]string{"/autoexec.be": backupFiles["/autoexec.be"], "/tlskey": backupFiles["/tlskey"]} {
		got, err := e.cl.fs.DownloadFile(t.Context(), newTarget, p, 0)
		if err != nil || string(got) != want {
			t.Errorf("%s on the new board: %q %v", p, got, err)
		}
	}
	pr := progressOf(t, e.flowEnv, job.ID)
	var why string
	for _, f := range pr.Files {
		if f.Name() == "lib/a.be" {
			why = f.Why
		}
	}
	if stateOf(pr, "lib/a.be") != FileSkipped || !strings.Contains(why, "folder") {
		t.Errorf("lib/a.be must be skipped with the missing folder named: %+v", pr)
	}
	if files, _ := SplitDetail(done.Detail); !strings.Contains(files, "1 skipped") {
		t.Errorf("summary %q", files)
	}
	assertNoSecret(t, e.flowEnv, done)
}

// Why: a replace that fails while writing files must leave the entry on the old hardware record.
func TestReplaceFilesFailureLeavesTheEntryAlone(t *testing.T) {
	e := replaceFilesFlow(t, map[string]string{"/autoexec.be": "factory", "/lib/x.be": "x"})
	a := e.dumpBackup(t, e.dumpA)
	e.breakOld(t)
	e.cl.uploadHook = func(p string, _ int) error {
		if p == "/lib/a.be" {
			return errors.New("link lost")
		}
		return nil
	}
	job, err := e.m.StartReplace(t.Context(), "alice", e.req(a.ID), StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepFiles {
		t.Fatalf("job: %+v", done)
	}
	d, err := e.st.GetDevice(t.Context(), e.dev)
	if err != nil || d.MAC != e.device.MAC {
		t.Errorf("the entry must still stand for the old hardware: %+v %v", d, err)
	}
	if got := e.cl.sent("WebGetConfig"); len(got) != 0 {
		t.Errorf("settings sent: %v", got)
	}
}

// Why: the identity is checked right before the first file, as it is before the token: if the
// address changed hands after the start, no file of this device may land on the other host.
func TestFilesAreNotWrittenToAnotherDeviceAtTheAddress(t *testing.T) {
	e, a := filesFlow(t)
	e.hb.hook = func(call int) {
		if call == 1 { // the safety backup is the last step before the files
			e.cl.mu.Lock()
			e.cl.status5 = "aa:bb:cc:99:99:99"
			e.cl.mu.Unlock()
		}
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepFiles || FilesChanged(done.Detail) {
		t.Fatalf("job: %+v", done)
	}
	if up := e.cl.uploads(); len(up) != 0 {
		t.Errorf("files went to the wrong device: %v", up)
	}
}

// badArchives stands for a files archive that was edited or damaged on disk.
type badArchives struct{ FileArchives }

func (badArchives) OpenFilesArchive(context.Context, model.BackupID) (*backup.FilesArchive, error) {
	return nil, backup.ErrBadArchive
}

// Why: nothing of a damaged archive may be written to a device (it could carry a path outside the
// rules), but the settings of the same backup are still worth restoring, and the page says so.
func TestDamagedFilesArchiveRestoresSettingsOnly(t *testing.T) {
	e, a := filesFlow(t)
	e.m.o.Files = badArchives{e.bk}
	as, err := e.m.Assess(t.Context(), a.ID)
	if err != nil || as.Files.Restores() || !strings.Contains(as.Files.Why, "damaged") {
		t.Fatalf("assessment %+v %v", as.Files, err)
	}
	job, err := e.m.StartWith(t.Context(), "alice", a.ID, StartOptions{ConfirmedFiles: 3})
	if err != nil {
		t.Fatal(err)
	}
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || len(e.cl.uploads()) != 0 {
		t.Fatalf("job %+v, uploads %v", done, e.cl.uploads())
	}
	if files, _ := SplitDetail(done.Detail); !strings.Contains(files, "damaged") {
		t.Errorf("summary %q", files)
	}
}

// --- review of #427 ---------------------------------------------------------------------------

func (e *flowEnv) startFiles(t *testing.T, id model.BackupID, confirmed int) Job {
	t.Helper()
	job, err := e.m.StartWith(t.Context(), "alice", id, StartOptions{ConfirmedFiles: confirmed})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// Why (H1): once a file was written the device is not "unchanged". A later failure before the
// settings go over (here: the address changed hands after the files) must say so, keep the step,
// and offer the rollback to the backup taken first, which also puts the old files back.
func TestFailureAfterTheFilesWereWrittenDoesNotClaimNothingChanged(t *testing.T) {
	e, a := filesFlow(t)
	e.cl.uploadHook = func(p string, _ int) error {
		if p == "/lib/a.be" { // the second file: from now on another host answers at the address
			e.cl.mu.Lock()
			e.cl.status5 = "aa:bb:cc:99:99:99"
			e.cl.mu.Unlock()
		}
		return nil
	}
	done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
	if done.State != store.RestoreFailed || !FilesChanged(done.Detail) || FailedStep(done.Detail) != StepUpload {
		t.Fatalf("job: %+v", done)
	}
	if strings.Contains(done.Detail, "nothing was changed") || !strings.HasPrefix(done.Detail, failFilesWritten) {
		t.Errorf("a device with written files is not unchanged: %q", done.Detail)
	}
	if got := e.cl.sent("WebGetConfig"); len(got) != 0 {
		t.Errorf("settings sent: %v", got)
	}
}

// Why (H2): a restore must be undoable. A file is overwritten only when the backup taken just
// before holds exactly its current content; the safety backup caps files at 1 MiB, so a bigger
// file on the device is not in it, and overwriting it would lose it for good. It is skipped and
// said loudly, and the job is not "verified".
func TestFileThePreBackupCannotHoldIsNotOverwritten(t *testing.T) {
	files := fsFiles(backupFiles)
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: files, FSSizeKB: 4096})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	big := strings.Repeat("B", 1100*1024) // over the 1 MiB cap of a backup
	e.setDevFile(t, "/lib/a.be", big)
	e.setDevFile(t, "/autoexec.be", "drifted")
	e.setDevFile(t, "/tlskey", backupFiles["/tlskey"])

	job := e.startFiles(t, a.ID, 3)
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone || done.Verified {
		t.Fatalf("job: %+v (a restore that left a file out is not verified)", done)
	}
	if got, err := e.cl.fs.DownloadFile(t.Context(), e.target(), "/lib/a.be", 2<<20); err != nil || string(got) != big {
		t.Errorf("a file the backup taken before cannot hold was overwritten (%v)", err)
	}
	if got := e.devFile(t, "/autoexec.be"); got != backupFiles["/autoexec.be"] {
		t.Errorf("a file that can be undone must still be written: %q", got)
	}
	pr := progressOf(t, e, job.ID)
	if stateOf(pr, "lib/a.be") != FileSkipped || len(pr.NotUndone) != 1 {
		t.Errorf("progress: %+v", pr)
	}
	files2, rest := SplitDetail(done.Detail)
	if !strings.Contains(files2, "could not be undone") || !strings.Contains(rest, "not written") || !strings.Contains(rest, "lib/a.be") {
		t.Errorf("must be said loudly: %q | %q", files2, rest)
	}
}

// Why (M1): an upload that never reached its POST (link down while the folder was selected)
// left the old file untouched: no put-back, no flash write counted, and the text says it was not
// written instead of "partly written".
func TestUploadThatNeverStartedIsNotCountedOrPutBack(t *testing.T) {
	e, a := filesFlow(t)
	e.cl.uploadHook = func(p string, _ int) error {
		if p == "/lib/a.be" {
			return fmt.Errorf("%w: link down: %w", tasmota.ErrNotSent, tasmota.ErrUnreachable)
		}
		return nil
	}
	job := e.startFiles(t, a.ID, 3)
	done := e.finish(t, job.ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepFiles || !strings.Contains(done.Detail, "was not written") || !strings.Contains(done.Detail, "untouched") {
		t.Fatalf("job: %+v", done)
	}
	if up := e.cl.uploads(); strings.Join(up, " ") != "/autoexec.be /lib/a.be" {
		t.Errorf("uploads %v: no put-back for a file that was never touched", up)
	}
	if pr := progressOf(t, e, job.ID); pr.Writes != 1 {
		t.Errorf("writes %d: only the first file reached the device", pr.Writes)
	}
	if got := e.devFile(t, "/lib/a.be"); got != "drifted /lib/a.be" {
		t.Errorf("file changed: %q", got)
	}
}

// Why (M1): the device cuts folder, slash and name at 49 bytes. The longest root file name is 47
// bytes; a 48 byte name must not be read or written (it would hit another file) and is skipped
// with the reason, while the 47 byte one is restored.
func TestNamesTheDeviceWouldCutAreSkipped(t *testing.T) {
	ok47, bad48 := "/"+strings.Repeat("a", 47), "/"+strings.Repeat("b", 48)
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: fsFiles(map[string]string{ok47: "backup", bad48: "backup"})})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.setDevFile(t, ok47, "drifted")
	if err := e.cl.fs.UploadFile(t.Context(), e.target(), bad48, []byte("drifted")); err == nil {
		t.Fatal("the client must refuse a 49 byte root path itself")
	}
	job := e.startFiles(t, a.ID, 2)
	done := e.finish(t, job.ID)
	if done.State != store.RestoreDone {
		t.Fatalf("job: %+v", done)
	}
	if got := e.devFile(t, ok47); got != "backup" {
		t.Errorf("the 47 byte name: %q", got)
	}
	pr := progressOf(t, e, job.ID)
	if stateOf(pr, bad48[1:]) != FileSkipped || len(e.cl.uploads()) != 1 {
		t.Errorf("progress %+v uploads %v", pr, e.cl.uploads())
	}
}

// Why (M2): a restore cancelled in the middle of an upload (shutdown) must still put the old file
// back; a half-written file is worse than a slow shutdown.
func TestPutBackSurvivesACancelledRestore(t *testing.T) {
	e, a := filesFlow(t)
	ctx, cancel := context.WithCancel(t.Context())
	e.m.o.Context = ctx
	e.cl.uploadHook = func(p string, attempt int) error {
		if p == "/lib/a.be" && attempt == 1 {
			_ = e.cl.fs.UploadFile(t.Context(), e.target(), p, []byte("PARTIAL"))
			cancel() // the shutdown arrives while this upload is in flight
			return context.Canceled
		}
		return nil
	}
	done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
	if done.State != store.RestoreFailed || FailedStep(done.Detail) != StepFiles {
		t.Fatalf("job: %+v", done)
	}
	if got := e.devFile(t, "/lib/a.be"); got != "drifted /lib/a.be" {
		t.Errorf("the old file was not put back after the cancel: %q", got)
	}
}

// Why (M3): the person confirms a plan. If the page showed no files (or MQTT) and the run then
// finds HTTP and files, nothing but the settings may be written; the same when the count differs.
func TestFilesAreWrittenOnlyForThePlanThatWasConfirmed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed int
	}{{"page showed no files", 0}, {"page showed another list", 2}} {
		t.Run(tc.name, func(t *testing.T) {
			e, a := filesFlow(t)
			done := e.finish(t, e.startFiles(t, a.ID, tc.confirmed).ID)
			if done.State != store.RestoreDone || len(e.cl.uploads()) != 0 {
				t.Fatalf("job %+v uploads %v", done, e.cl.uploads())
			}
			if files, _ := SplitDetail(done.Detail); !strings.Contains(files, "only the settings are restored") {
				t.Errorf("summary %q", files)
			}
			if got := e.devFile(t, "/autoexec.be"); got != "drifted /autoexec.be" {
				t.Errorf("a file was written: %q", got)
			}
		})
	}
}

// Why (M4): the files line must stay while the later steps run (Detail is overwritten by each
// step), so the summary lives with the progress.
func TestFilesSummaryStaysAvailableForTheLaterSteps(t *testing.T) {
	e, a := filesFlow(t)
	done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
	pr := progressOf(t, e, done.ID)
	if !strings.Contains(pr.Summary, "3 files written") {
		t.Errorf("summary %q", pr.Summary)
	}
}

// Why (L2): "differs" and "could not be read back" are different statements: the first says the
// device holds other content, the second only that the check could not look.
func TestVerificationSeparatesDifferingFromUnreadableFiles(t *testing.T) {
	files := fsFiles(backupFiles)
	e := newFlowSpec(t, "16.0.0", esp32Decoder{}, simulator.DeviceSpec{Profile: simulator.ProfileESP32, Files: files, FSSizeKB: 4096})
	a := e.dumpBackup(t, e.dumpA)
	e.sim.SetConfigDump(e.dumpB)
	e.setDevFile(t, "/autoexec.be", "drifted")
	e.hb.hook = func(call int) {
		if call == 2 { // the verification backup: the device rewrote one file and grew another past the cap
			e.setDevFile(t, "/lib/a.be", strings.Repeat("G", 1100*1024))
			e.setDevFile(t, "/autoexec.be", "rewritten by the device")
		}
	}
	done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
	_, rest := SplitDetail(done.Detail)
	if done.State != store.RestoreDone || done.Verified || !strings.Contains(rest, "1 file differ from the backup: autoexec.be") || !strings.Contains(rest, "1 file could not be read back to compare: lib/a.be") {
		t.Errorf("job %+v", done)
	}
}

// Why (L1): a job interrupted while it wrote files has probably changed files; the generic
// "interrupted" text would not say so and the page would hide the rollback.
func TestInterruptedFilesStepSaysFilesMayBeWritten(t *testing.T) {
	d := InterruptedDetails()[store.RestoreFiles]
	if !FilesChanged(d) || FailedStep(d) != StepFiles || !strings.Contains(d, "restart") {
		t.Errorf("%q", d)
	}
}

// emptyPre answers "no files" for every run but one: a safety backup whose files step failed.
type emptyPre struct {
	FileArchives
	keep model.BackupID
}

func (p emptyPre) RunFiles(ctx context.Context, id model.BackupID) (backup.RunFiles, error) {
	if id == p.keep {
		return p.FileArchives.RunFiles(ctx, id)
	}
	return backup.RunFiles{Note: "the device took too long to answer"}, nil
}

// Why (H2): when the backup taken before holds no files at all (its files step failed), no file
// that differs may be overwritten, because nothing could be undone; files the device does not
// have yet are still written.
func TestNoFileIsOverwrittenWhenThePreBackupHasNoFiles(t *testing.T) {
	e, a := filesFlow(t)
	e.m.o.Files = emptyPre{e.bk, a.ID}
	done := e.finish(t, e.startFiles(t, a.ID, 3).ID)
	if done.State != store.RestoreDone || done.Verified || len(e.cl.uploads()) != 0 {
		t.Fatalf("job %+v uploads %v", done, e.cl.uploads())
	}
	if got := e.devFile(t, "/autoexec.be"); got != "drifted /autoexec.be" {
		t.Errorf("overwritten without a way back: %q", got)
	}
	if pr := progressOf(t, e, done.ID); len(pr.NotUndone) != 3 {
		t.Errorf("not undone: %v", pr.NotUndone)
	}
}
