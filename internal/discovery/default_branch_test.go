package discovery

import (
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

func TestMissingRefErrorClassificationKeepsExecutionFailuresDistinct(t *testing.T) {
	missing := &gitx.Error{Result: gitx.Result{ExitCode: 1}}
	if !isMissingRefError(missing) {
		t.Fatal("exit 1 with empty stderr was not treated as a missing ref")
	}
	withStderr := &gitx.Error{Result: gitx.Result{ExitCode: 1, Stderr: "fatal: repository is corrupt"}}
	if isMissingRefError(withStderr) {
		t.Fatal("Git failure with stderr was treated as a missing ref")
	}
	execution := &gitx.Error{Result: gitx.Result{ExitCode: -1}}
	if isMissingRefError(execution) {
		t.Fatal("Git execution failure was treated as a missing ref")
	}
}
