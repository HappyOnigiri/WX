package archive

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

// TestMutationSnapshotLFSWarningOnDiffFailure は changed LFS 候補の収集に
// 失敗しても snapshot 自体は成功し、診断だけが外部ログへ残ることを確認する。
func TestMutationSnapshotLFSWarningOnDiffFailure(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	manager.Preparer.Log = slog.New(slog.NewTextHandler(&logs, nil))
	installGitFault(t, " diff-tree -r -z --raw ", 1)
	if _, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "lfs-warning", time.Now().Add(time.Hour), nil); err != nil {
		t.Fatalf("snapshot failed after optional LFS diff failure: %v", err)
	}
	if !strings.Contains(logs.String(), "collect changed LFS objects") {
		t.Fatalf("optional LFS failure was not logged: %s", logs.String())
	}
}

// TestMutationArchiveLockSlotKeepsTheNoPreparerPath は recovery ref だけを扱う
// manager が slot lock を要求せず、preparer 付き経路だけが同じ slot を待つことを確認する。
func TestMutationArchiveLockSlotKeepsTheNoPreparerPath(t *testing.T) {
	manager := &Manager{}
	ctx := context.Background()
	got, release, err := manager.lockSlot(ctx)
	if err != nil || got != ctx {
		t.Fatalf("nil preparer lockSlot=(%v,%v) want original context", got, err)
	}
	release()

	locks := &gitx.KeyedLocks{}
	preparer := &workspace.Preparer{RootID: "root-generation", SlotRelPath: "slot", SlotLocks: locks}
	manager.Preparer = preparer
	held, release, err := manager.lockSlot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	blocked, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, _, err := manager.lockSlot(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same slot was not serialized: %v", err)
	}
	if held == ctx {
		t.Fatal("slot lock did not annotate the acquired context")
	}
}

// TestMutationRemoveWorktreeRequiresPinnedDescriptor は descriptor が無い、または
// 別 root に pin された manager を path 名だけの削除へ進めないことを確認する。
func TestMutationRemoveWorktreeRequiresPinnedDescriptor(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	target := filepath.Join(worktreeRoot, "slot", "missing")
	for _, test := range []struct {
		name  string
		setup func(*Manager)
	}{
		{name: "nil descriptor", setup: func(m *Manager) { m.Preparer.OwnedRoot = nil }},
		{name: "different root", setup: func(m *Manager) { m.Preparer.RootPath = filepath.Join(t.TempDir(), "other-root") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copyManager := *manager
			copyPreparer := *manager.Preparer
			copyManager.Preparer = &copyPreparer
			test.setup(&copyManager)
			err := copyManager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head)
			if err == nil || !strings.Contains(err.Error(), "descriptor-bound worktree removal is unavailable") {
				t.Fatalf("descriptor guard error=%v", err)
			}
		})
	}

	// Preparer が無い manager は、登録外の既に消えた path を冪等に扱える。
	withoutPreparer := *manager
	withoutPreparer.Preparer = nil
	if err := withoutPreparer.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head); err != nil {
		t.Fatalf("nil preparer changed the idempotent missing path: %v", err)
	}
}

// TestMutationRemoveWorktreeRechecksExpectedHead は unlock 後に HEAD が差し替わる
// race を実際の Git process で起こし、破壊的 remove の直前に拒否されることを確認する。
func TestMutationRemoveWorktreeRechecksExpectedHead(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	target := filepath.Join(worktreeRoot, "slot", "root")
	mustMkdir(t, filepath.Dir(target))
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	markOwnedWorktree(t, worktreeRoot, target, "slot", repo)
	pointAtSlot(t, manager, worktreeRoot, target)
	commitFile(t, repository, "next", "next\n")
	next := gitCommand(t, repository, "rev-parse", "HEAD")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"if [ \"$*\" = \"-C $WX_MUTATE_TARGET rev-parse HEAD\" ]; then :; fi\n" +
		"if echo \" $* \" | grep -q ' rev-parse HEAD '; then\n" +
		"  count=0; if [ -f \"$WX_MUTATE_MARKER\" ]; then read -r count < \"$WX_MUTATE_MARKER\"; fi\n" +
		"  count=$((count + 1)); printf '%s\\n' \"$count\" > \"$WX_MUTATE_MARKER\"\n" +
		"  if [ \"$count\" -eq 2 ]; then \"$WX_REAL_GIT\" -C \"$WX_MUTATE_TARGET\" reset --hard \"$WX_MUTATE_HEAD\" >/dev/null 2>&1; fi\n" +
		"fi\n" +
		"exec \"$WX_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REAL_GIT", realGit)
	t.Setenv("WX_MUTATE_TARGET", target)
	t.Setenv("WX_MUTATE_HEAD", next)
	t.Setenv("WX_MUTATE_MARKER", filepath.Join(bin, "count"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = manager.RemoveWorktree(context.Background(), repo, worktreeRoot, target, head)
	if err == nil || !strings.Contains(err.Error(), "HEAD changed before removal") {
		t.Fatalf("HEAD replacement was not rejected: %v", err)
	}
	if _, statErr := os.Lstat(target); statErr != nil {
		t.Fatalf("worktree was removed despite HEAD mismatch: %v", statErr)
	}
}
