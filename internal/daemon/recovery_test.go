package daemon

import (
	"errors"
	"testing"
)

func TestRecoveryUnavailableCoversRestoreFailuresOnly(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]bool{
		"RESTORE_FAILED":                true,
		"RESTORE_FAILED:prepare-id":     true,
		"RESTORE_AMBIGUOUS":             true,
		"SNAPSHOT_INCOMPLETE":           true,
		"SNAPSHOT_UNAVAILABLE":          true,
		"PREPARE_FAILED":                false,
		"PREPARE_FAILED:RESTORE_FAILED": false,
		"JOB_RETRY_EXHAUSTED":           false,
		"WORKTREE_OWNERSHIP_UNCERTAIN":  false,
		"":                              false,
	} {
		if got := recoveryUnavailable(code); got != want {
			t.Errorf("recoveryUnavailable(%q)=%t, want %t", code, got, want)
		}
	}
}

func TestIsRecoveryUnavailableReadsTheMarker(t *testing.T) {
	t.Parallel()
	if IsRecoveryUnavailable(nil) {
		t.Fatal("a missing error must not report an unavailable recovery")
	}
	if IsRecoveryUnavailable(errors.New("workspace readiness failed: state=FAILED failure_id=PREPARE_FAILED")) {
		t.Fatal("a prepare failure must not report an unavailable recovery")
	}
	if !IsRecoveryUnavailable(errors.New("readiness failed " + RecoveryUnavailableMarker + " detail_path=unavailable")) {
		t.Fatal("the marker must be recognized inside a wrapped RPC message")
	}
}
