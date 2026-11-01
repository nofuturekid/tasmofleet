package web

import (
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/backup"
	"github.com/nofuturekid/tasmofleet/internal/model"
)

func TestRunDetailSaysTheFilesReasonOnce(t *testing.T) {
	// Why (#324): the run's dump note ends with "files not backed up: ..." as data; the row prints
	// that reason in words, once, not the raw sentence next to its own wording.
	r := backupRun{Backup: model.Backup{Version: "16.0.0", Size: 10, Note: "Nightly; " + backup.NoteFilesPrefix + "no filesystem"}}
	got := runDetail(r)
	if strings.Count(got, "settings only: this build has no filesystem") != 1 || strings.Contains(got, "files not backed up") || !strings.Contains(got, "Nightly") {
		t.Errorf("detail %q", got)
	}
}
