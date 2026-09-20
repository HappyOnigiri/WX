package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// shallow module は clone を止めず、object 共有不可の事実だけを warn に残す。
func TestPrepareMaterializesShallowSubmoduleWithWarning(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := os.WriteFile(filepath.Join(f.moduleDir(), "shallow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.submoduleTarget(), "kid.txt")); err != nil {
		t.Fatalf("shallow submodule content: %v", err)
	}
	logged := f.logged.String()
	if !strings.Contains(logged, "object sharing is unavailable") || !strings.Contains(logged, "shallow") {
		t.Fatalf("logged=%q, want a shallow object-sharing warning", logged)
	}
}

func TestPrepareReportsShallowSubmoduleWithoutOrigin(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := os.WriteFile(filepath.Join(f.moduleDir(), "shallow"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.moduleDir(), "--git-dir=.", "config", "--unset-all", "remote.origin.url")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	logged := f.logged.String()
	if !strings.Contains(logged, "no origin url") || !strings.Contains(logged, "shallow") {
		t.Fatalf("logged=%q, want missing-origin and shallow diagnostics", logged)
	}
}

// promisor module に要求 OID が無い場合は clone を始めず、per-worktree の module gitdir も作らない。
func TestPrepareSkipsMissingPromisorObjectBeforeWriting(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	if err := os.WriteFile(filepath.Join(f.child, "kid.txt"), []byte("ahead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, f.child, "add", ".")
	gitCommand(t, f.child, "commit", "-m", "child ahead")
	ahead := gitOutput(t, f.child, "rev-parse", "HEAD")
	gitCommand(t, f.repository, "update-index", "--cacheinfo", "160000,"+ahead+","+submodulePath)
	gitCommand(t, f.repository, "commit", "-m", "advance gitlink")
	head := gitOutput(t, f.repository, "rev-parse", "HEAD")
	gitCommand(t, f.moduleDir(), "--git-dir=.", "config", "remote.origin.promisor", "true")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	gitDir := gitOutput(t, f.target, "rev-parse", "--path-format=absolute", "--git-dir")
	if _, err := os.Stat(filepath.Join(gitDir, "modules", submoduleName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("per-worktree module gitdir stat error=%v, want not exist", err)
	}
	if !strings.Contains(f.logged.String(), "promisor local module") {
		t.Fatalf("logged=%q, want a promisor skip warning", f.logged.String())
	}
}

// object が揃った promisor module は従来どおり実体化する。
func TestPrepareMaterializesCompletePromisorSubmodule(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	gitCommand(t, f.moduleDir(), "--git-dir=.", "config", "remote.origin.promisor", "true")
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.submoduleTarget(), "kid.txt")); err != nil {
		t.Fatalf("promisor submodule content: %v", err)
	}
}

func TestSubmodulesAtRevisionReadsGitlinkFromTree(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	modules, err := f.preparer.SubmodulesAtRevision(context.Background(), f.repository, f.head)
	if err != nil {
		t.Fatal(err)
	}
	if len(modules) != 1 || modules[0].Name != submoduleName || modules[0].Path != submodulePath || modules[0].OID != submoduleGitlink(t, f.repository, f.head) {
		t.Fatalf("modules=%+v, want the gitlink from %s", modules, f.head)
	}
}

// exit status 0 の Git エラーは、要求 object が無いという観測結果として扱い、実行障害に昇格させない。
func TestSubmoduleInspectionExecutionErrorAcceptsZeroExitStatus(t *testing.T) {
	t.Parallel()
	err := &gitx.Error{Result: gitx.Result{ExitCode: 0}}
	if got := submoduleInspectionExecutionError(context.Background(), err); got != nil {
		t.Fatalf("zero-exit inspection error=%v, want nil", got)
	}
	if got := submoduleInspectionExecutionError(context.Background(), &gitx.Error{Result: gitx.Result{ExitCode: -1}}); got == nil {
		t.Fatal("missing exit status was treated as an object-missing result")
	}
}

// testlint:allow-serial -- PATH を一時 wrapper へ差し替え、InspectSubmodule の object 検査だけを期限切れにするため
func TestInspectSubmodulePropagatesObjectInspectionCancellation(t *testing.T) {
	root := t.TempDir()
	initTestRepository(t, root)
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, root, "add", "file")
	gitCommand(t, root, "commit", "-m", "initial")
	oid := gitOutput(t, root, "rev-parse", "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" cat-file -e \"*) sleep 2;; esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := InspectSubmodule(ctx, &gitx.Runner{}, root, oid); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("InspectSubmodule() error=%v, want context deadline", err)
	}
}
