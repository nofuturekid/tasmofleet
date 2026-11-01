package model

import "testing"

// Why: the job and transaction views show Target(); an ESP32-S2 update has two builds that look
// the same otherwise, so the chosen one must be visible there (#88).
func TestJobTargetShowsTheChosenBuild(t *testing.T) {
	for _, tt := range []struct {
		j    Job
		want string
	}{
		{Job{TargetRelease: "16.0.0", TargetVariant: "tasmota"}, "16.0.0 tasmota"},
		{Job{TargetRelease: "16.0.0", TargetVariant: "tasmota", TargetBuild: "s2cdc"}, "16.0.0 tasmota (s2cdc)"},
		{Job{Kind: KindPolicy, TargetBuild: "s2"}, "Make compliant"},
	} {
		if got := tt.j.Target(); got != tt.want {
			t.Errorf("Target() = %q, want %q", got, tt.want)
		}
	}
}

// Why: IsUpdate() is the fall-through of every job-kind switch ("not policy, command or backup").
// A GPIO template job that fell into it would be handed to the OTA state machine and flashed like
// firmware, so the new kind must be excluded explicitly.
func TestTemplateJobIsNotAnUpdate(t *testing.T) {
	j := Job{Kind: KindTemplate}
	if !j.IsTemplate() || j.IsUpdate() || j.IsPolicy() || j.IsCommand() || j.IsBackup() {
		t.Fatalf("template job misclassified: template=%v update=%v", j.IsTemplate(), j.IsUpdate())
	}
	if got := j.Target(); got != "GPIO template" {
		t.Fatalf("Target() = %q", got)
	}
	if !(Job{}).IsUpdate() {
		t.Fatal("the zero kind must stay a firmware update")
	}
}
