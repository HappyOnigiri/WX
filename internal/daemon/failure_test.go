package daemon

import (
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestGitFailureInfoUsesTheProcessingKindAndDetailPath(t *testing.T) {
	t.Parallel()
	err := &gitx.Error{FailureID: "git-failure"}
	code, detail, ok := gitFailureInfo("RESTORE", err, "/var/log/wx/details")
	if !ok {
		t.Fatal("git failure was not recognized")
	}
	if code != "RESTORE_FAILED:git-failure" {
		t.Fatalf("failure code=%q", code)
	}
	if detail != filepath.Join("/var/log/wx/details", "git-failure.log") {
		t.Fatalf("detail path=%q", detail)
	}
}
