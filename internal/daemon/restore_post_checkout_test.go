package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 復元でも submodule は post-checkout より前に実体化し、include の配置はその後に行う。
// hook が submodule の中身を前提にする運用は cold start でだけ成立していたので、この順序を復元側でも固定する。
func TestRestoreMaterializesSubmoduleBeforePostCheckout(t *testing.T) {
	t.Parallel()
	f := newRestoreHookFixture(t, restoreGuardHook)
	restored := f.leaseSnapshotAndRestore(t)
	if _, err := os.Stat(filepath.Join(restored, daemonSubmodulePath, "tracked.txt")); err != nil {
		t.Fatalf("restored submodule content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restored, restoreIncludeName)); err != nil {
		t.Fatalf("restored include %s: %v", restoreIncludeName, err)
	}
	// hook 自身が submodule の中身と include の未配置を検査するため、復元が成功した時点で前後関係は満たされている。
	// 残るのは実行回数で、1 回の復元につき post-checkout がちょうど 1 回であることを数える。
	if runs := f.hookRuns(t); runs != 2 {
		t.Fatalf("post-checkout ran %d times, want one for the cold start and one for the restore", runs)
	}
}

// hook を置かない同じ fixture では復元が成功する。
// hook 有効の失敗が hook の実行位置に由来し、submodule を持つ復元そのものではないことを分離する。
func TestRestoreWithoutPostCheckoutHookSucceeds(t *testing.T) {
	t.Parallel()
	f := newRestoreHookFixture(t, nil)
	restored := f.leaseSnapshotAndRestore(t)
	if _, err := os.Stat(filepath.Join(restored, daemonSubmodulePath, "tracked.txt")); err != nil {
		t.Fatalf("restored submodule content: %v", err)
	}
}

// exit 0 の hook が出した出力は、復元でも捨てずに daemon log の warn として残す。
func TestRestoreReportsOutputOfASuccessfulHook(t *testing.T) {
	t.Parallel()
	f := newRestoreHookFixture(t, restoreNoticeHook)
	f.leaseSnapshotAndRestore(t)
	logs := f.logs.tail()
	notices := strings.Count(logs, "prepare produced output without failing")
	if notices != 2 {
		t.Fatalf("notice log lines = %d, want one for the cold start and one for the restore:\n%s", notices, logs)
	}
	if !strings.Contains(logs, "phase=post-checkout") || !strings.Contains(logs, "submodule update skipped") {
		t.Fatalf("notice log lines do not carry the hook output:\n%s", logs)
	}
}

// restoreIncludeName は `.worktreeinclude` だけで配置される ignore 対象の file である。
// 先行配置の候補に入る名前を使うと cold start では post-checkout より前に置かれ、復元との比較にならない。
const restoreIncludeName = "late.cfg"

// restoreGuardHook は submodule が空、または include が先に置かれた状態で post-checkout に到達したら失敗する hook を組む。
// 実行のたびに worktree の外の count file へ 1 行足し、復元 1 回あたりの実行回数を数えられるようにする。
func restoreGuardHook(countPath string) string {
	return "#!/bin/sh\n" +
		"echo run >> '" + countPath + "'\n" +
		"test -e " + daemonSubmodulePath + "/tracked.txt || { echo 'submodule is empty at post-checkout' >&2; exit 1; }\n" +
		"test ! -e " + restoreIncludeName + " || { echo 'include was placed before post-checkout' >&2; exit 1; }\n"
}

// restoreNoticeHook は失敗せずに出力だけを残す hook である。
func restoreNoticeHook(string) string {
	return "#!/bin/sh\nprintf 'submodule update skipped\\n' >&2\nexit 0\n"
}

// restoreHookFixture は submodule と include を持つ repository を、貸出から復元まで通せる Manager と一緒に持つ。
type restoreHookFixture struct {
	*managerFixture
	repository string
	countPath  string
}

// newRestoreHookFixture は submodule 1 件と `.worktreeinclude` を持つ repository を作り、hook を common directory へ置く。
// hook が nil の場合は置かない。hook は count file の path を受け取って script を組む。
func newRestoreHookFixture(t *testing.T, hook func(countPath string) string) *restoreHookFixture {
	t.Helper()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Retention.EndedWorktree.Duration = 0
		s.Config.Readiness.Timeout.Duration = 10 * time.Second
	})
	repository := filepath.Join(f.Root, "repo")
	initGitRepoWithSubmodule(t, repository)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte(restoreIncludeName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte(restoreIncludeName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".gitignore", ".worktreeinclude")
	gitRun(t, repository, "commit", "-m", "worktree metadata")
	if err := os.WriteFile(filepath.Join(repository, restoreIncludeName), []byte("late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	countPath := filepath.Join(f.Root, "post-checkout.log")
	if hook != nil {
		common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err := os.WriteFile(filepath.Join(common, "hooks", "post-checkout"), []byte(hook(countPath)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &restoreHookFixture{managerFixture: f, repository: repository, countPath: countPath}
}

// leaseSnapshotAndRestore は貸出・snapshot・復元を一巡し、復元された worktree の path を返す。
func (f *restoreHookFixture) leaseSnapshotAndRestore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	m := f.Manager
	lease, err := m.ResolveAndLease(ctx, f.repository, nil, "codex", os.Getpid(), leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 60*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatalf("wait for the cold start: %v", err)
	}
	if err := m.BindAgentSession(ctx, lease.SessionID, lease.Token, "agent-session-1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "untracked.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(ctx, lease.SessionID, lease.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		snaps, _ := f.Store.Snapshots(ctx, lease.SessionID)
		return len(snaps) == 1
	})
	resumed, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 60*time.Second, resumed.SessionID, resumed.Token); err != nil {
		details, detailsErr := f.Store.StatusDiagnostics(ctx)
		t.Fatalf("wait for the restore: %v; diagnostics=%+v diagnostics_error=%v", err, details, detailsErr)
	}
	if _, err := os.Stat(filepath.Join(resumed.Path, "untracked.txt")); err != nil {
		t.Fatalf("restored snapshot content: %v", err)
	}
	return resumed.Path
}

// hookRuns は count file の行数から post-checkout の実行回数を返す。
func (f *restoreHookFixture) hookRuns(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.countPath)
	if err != nil {
		t.Fatalf("post-checkout count file: %v", err)
	}
	return len(strings.Fields(string(data)))
}
