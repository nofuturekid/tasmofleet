package web

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

func TestRunDetailDoesNotShowTheFilesSentenceYet(t *testing.T) {
	// Why (#324): the run's dump note ends with "files not backed up: ..." as data for the
	// coming Files block; the backup list waits for the approved mockup and must not print it.
	r := backupRun{Backup: model.Backup{Version: "16.0.0", Size: 10, Note: "Nightly; " + backup.NoteFilesPrefix + "no filesystem"}}
	got := runDetail(r)
	if strings.Contains(got, "files not backed up") || !strings.Contains(got, "Nightly") {
		t.Errorf("detail %q", got)
	}
}
