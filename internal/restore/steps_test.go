package restore

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/store"
)

// Why: the progress page ticks the steps a job has passed and marks the one it is at or failed
// at; a wrong step would tell the user the device was changed when nothing was (or the
// reverse). The failure texts are the ones the run code writes (see the flow tests, which
// check Step on real failed jobs).
func TestStepOfRunningDoneAndFailedJobs(t *testing.T) {
	for _, c := range []struct {
		state, detail string
		want          int
	}{
		{store.RestorePending, "", StepBackup},
		{store.RestoreBackup, "taking a backup", StepBackup},
		{store.RestoreFiles, "writing lib/x.be (2 of 3)", StepFiles},
		{store.RestoreSending, "", StepUpload},
		{store.RestoreWaiting, "", StepRestart},
		{store.RestoreVerifying, "", StepVerify},
		{store.RestoreDone, "restored", StepVerify + 1},
		{store.RestoreFailed, failPreBackup + "timeout", StepBackup},
		{store.RestoreFailed, failPreBackupReplace + "timeout", StepBackup},
		// #324: a failure of the files step is the files step, not the upload; it must not be read as
		// "nothing was changed" (which maps to the upload) when the text says files were written.
		{store.RestoreFailed, failFilesNone + "device busy", StepFiles},
		{store.RestoreFailed, failFilesPartly + "lib/x.be: upload refused", StepFiles},
		{store.RestoreFailed, "replacement " + failUnreachable + "x", StepUpload},
		{store.RestoreFailed, failRestoreUpload + "x", StepUpload},
		// jobs stored before #430 keep their step on the progress page
		{store.RestoreFailed, failTokenBefore430 + "x", StepUpload},
		{store.RestoreFailed, failWebGetConfigBefore430 + "x", StepUpload},
		{store.RestoreFailed, "the device downloaded the configuration but did not come back in time", StepRestart},
		{store.RestoreFailed, failAfterRestore + "verification failed: x", StepVerify},
		{store.RestoreFailed, failTakeOver + "the inventory entry could not take it over", StepVerify},
	} {
		if got := Step(Job{State: c.state, Detail: c.detail}); got != c.want {
			t.Errorf("%s %q: step %d, want %d", c.state, c.detail, got, c.want)
		}
	}
}

// Why: the progress page offers the rollback only when the device was changed. A files failure
// before the first write changed nothing; one after it did, and the restore of the backup taken
// first puts the old files back too.
func TestFilesChangedOnlyForAPartlyWrittenFailure(t *testing.T) {
	if FilesChanged(failFilesNone + "x") {
		t.Error("a files failure before any write must not offer a rollback")
	}
	if !FilesChanged(failFilesPartly + "x") {
		t.Error("a partly written files step must offer a rollback")
	}
}

// Why: the finished job keeps what the files step did in the same text as the verification, so
// the page can show both after a restart of the service.
func TestSplitDetailSeparatesTheFilesSummary(t *testing.T) {
	if f, r := SplitDetail("6 files written (6 flash writes)\nrestored; identical"); f != "6 files written (6 flash writes)" || r != "restored; identical" {
		t.Errorf("%q %q", f, r)
	}
	if f, r := SplitDetail("restored; identical"); f != "" || r != "restored; identical" {
		t.Errorf("no files part: %q %q", f, r)
	}
}
