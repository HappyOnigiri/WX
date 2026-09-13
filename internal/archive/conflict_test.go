package archive

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestConflictStateRoundTripRestoresStages(t *testing.T) {
	repository, _, _, _ := archiveFixture(t)
	gitCommand(t, repository, "checkout", "-q", "-b", "side")
	if err := os.WriteFile(repository+"/tracked", []byte("ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "ours")
	gitCommand(t, repository, "checkout", "-q", "main")
	if err := os.WriteFile(repository+"/tracked", []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "theirs")
	gitCommand(t, repository, "checkout", "-q", "side")
	runnerValue, runner := directGitAccess(t, repository)
	if _, err := runner(nil, nil, "merge", "main"); err == nil {
		t.Fatal("merge unexpectedly succeeded")
	}
	want, err := runnerValue(nil, "ls-files", "--stage")
	if err != nil {
		t.Fatal(err)
	}
	head, err := runnerValue(nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	indexTree, conflictOID, err := captureConflictState(runnerValue, runner, head)
	if err != nil {
		t.Fatal(err)
	}
	if indexTree == "" || conflictOID == "" {
		t.Fatalf("captured index=%q conflict=%q", indexTree, conflictOID)
	}
	conflictTree, err := runnerValue(nil, "rev-parse", conflictOID+"^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	paths := mustValue(t, runnerValue, "ls-tree", "-r", "--name-only", conflictTree)
	for _, wantPath := range []string{"stages/1/tracked", "stages/2/tracked"} {
		if !strings.Contains(paths, wantPath) {
			t.Fatalf("conflict tree paths=%q missing %s", paths, wantPath)
		}
	}
	replayedIndex, replayedConflict, err := captureConflictState(runnerValue, runner, head)
	if err != nil || replayedIndex != indexTree || replayedConflict != conflictOID {
		t.Fatalf("replayed conflict index=%q conflict=%q err=%v want %q %q", replayedIndex, replayedConflict, err, indexTree, conflictOID)
	}
	if got, err := runnerValue(nil, "ls-files", "--stage"); err != nil || got != want {
		t.Fatalf("capture changed live index=%q want %q err=%v", got, want, err)
	}
	if _, err := runner(nil, nil, "read-tree", indexTree); err != nil {
		t.Fatal(err)
	}
	if err := restoreConflictIndex(runnerValue, runner, conflictTree); err != nil {
		var gitErr *gitx.Error
		if errors.As(err, &gitErr) {
			t.Logf("git stderr: %s", gitErr.Result.Stderr)
		}
		t.Fatal(err)
	}
	if got := mustValue(t, runnerValue, "ls-files", "--stage"); got != want {
		t.Fatalf("restored index=%q want %q", got, want)
	}
}

func TestConflictStateSupportsModifyDeleteStages(t *testing.T) {
	repository, _, _, _ := archiveFixture(t)
	gitCommand(t, repository, "checkout", "-q", "-b", "deleted")
	gitCommand(t, repository, "rm", "-q", "tracked")
	gitCommand(t, repository, "commit", "-m", "delete")
	gitCommand(t, repository, "checkout", "-q", "main")
	if err := os.WriteFile(repository+"/tracked", []byte("theirs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "modify")
	gitCommand(t, repository, "checkout", "-q", "deleted")
	runnerValue, runner := directGitAccess(t, repository)
	if _, err := runner(nil, nil, "merge", "main"); err == nil {
		t.Fatal("modify/delete merge unexpectedly succeeded")
	}
	head := mustValue(t, runnerValue, "rev-parse", "HEAD")
	if _, conflict, err := captureConflictState(runnerValue, runner, head); err != nil || conflict == "" {
		t.Fatalf("capture modify/delete conflict=%q err=%v", conflict, err)
	}
}

func mustValue(t *testing.T, value gitValueFunc, args ...string) string {
	t.Helper()
	out, err := value(nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
