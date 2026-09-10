package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

// READY standby は submodule ごと実体化され、post-checkout hook より前に揃っている。
// hook が submodule の中身を前提にする運用を成立させるため、挿入位置も hook 自身で固定する。
func TestReadyStandbyMaterializesSubmoduleBeforePostCheckout(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, func(t *testing.T, repository string) {
		initGitRepoWithSubmodule(t, repository)
		writeSubmoduleGuardHook(t, repository)
	})
	standby := f.readyStandby(t)
	worktree := f.standbyWorktree(t, standby)
	submodule := filepath.Join(worktree, daemonSubmodulePath)
	if _, err := os.Stat(filepath.Join(submodule, "tracked.txt")); err != nil {
		t.Fatalf("standby submodule content: %v", err)
	}
	gitlink := gitOutput(t, worktree, "rev-parse", "HEAD:"+daemonSubmodulePath)
	if head := gitOutput(t, submodule, "rev-parse", "HEAD"); head != gitlink {
		t.Fatalf("standby submodule HEAD=%s, want gitlink %s", head, gitlink)
	}
	if status := gitOutput(t, worktree, "status", "--porcelain", "--ignore-submodules=none"); status != "" {
		t.Fatalf("standby status=%q, want clean", status)
	}
}

// slot を削除すると per-worktree の管理ディレクトリごと回収され、submodule の gitdir も残らない。
// wx は `.git` 配下を個別に削除する経路を持たないため、この一手だけが後始末になる。
func TestRemoveRegisteredSlotReclaimsSubmoduleGitdir(t *testing.T) {
	f := newReuseStandbyFixtureWith(t, initGitRepoWithSubmodule)
	ctx := context.Background()
	standby := f.readyStandby(t)
	worktree := f.standbyWorktree(t, standby)
	common := gitOutput(t, f.repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	name := worktreeAdminName(t, common, worktree)
	if _, err := os.Stat(filepath.Join(common, "worktrees", name, "modules", daemonSubmoduleName)); err != nil {
		t.Fatalf("per-worktree submodule gitdir: %v", err)
	}
	if _, scheduled, err := f.store.ScheduleRemoval(ctx, standby.ID, ""); err != nil || !scheduled {
		t.Fatalf("schedule removal scheduled=%t err=%v", scheduled, err)
	}
	f.runPendingJobs(t)
	if _, err := os.Stat(filepath.Join(common, "worktrees", name)); !os.IsNotExist(err) {
		t.Fatalf("worktree admin directory stat err=%v, want it removed with the slot", err)
	}
}

// writeSubmoduleGuardHook は submodule が空のまま post-checkout に到達したら失敗する hook を置く。
func writeSubmoduleGuardHook(t *testing.T, repository string) {
	t.Helper()
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	hook := filepath.Join(common, "hooks", "post-checkout")
	script := "#!/bin/sh\ntest -e " + daemonSubmodulePath + "/tracked.txt || { echo 'submodule is empty at post-checkout' >&2; exit 1; }\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func (f *reuseStandbyFixture) standbyWorktree(t *testing.T, slot state.Slot) string {
	t.Helper()
	repositoryState, err := f.store.SlotRepository(context.Background(), slot.ID, string(f.workspace.Repositories[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	return repositoryState.WorktreePath
}

// worktreeAdminName は target を指す `<common dir>/worktrees/<name>` の name を backlink から引く。
func worktreeAdminName(t *testing.T, common, target string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(common, "worktrees", entry.Name(), "gitdir"))
		if err != nil {
			continue
		}
		if filepath.Clean(string(data[:len(data)-1])) == filepath.Join(target, ".git") {
			return entry.Name()
		}
	}
	t.Fatalf("no worktree admin directory points at %s", target)
	return ""
}
