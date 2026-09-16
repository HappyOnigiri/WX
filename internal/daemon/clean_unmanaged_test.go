package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/archive"
)

func unmanagedReplyTargets(t *testing.T, reply map[string]any) []unmanagedTarget {
	t.Helper()
	targets, ok := reply["targets"].([]unmanagedTarget)
	if !ok {
		t.Fatalf("targets=%T", reply["targets"])
	}
	return targets
}

// dry-run は状態も実体も変えない。消える範囲を先に確認できることが `--unmanaged` を使える条件である。
func TestCleanUnmanagedDryRunChangesNothing(t *testing.T) {
	ctx, manager, _, _ := unmanagedFixture(t)
	reply, err := manager.CleanUnmanaged(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry, _ := reply["dry_run"].(bool); !dry {
		t.Fatalf("reply=%+v", reply)
	}
	targets := unmanagedReplyTargets(t, reply)
	if len(targets) == 0 {
		t.Fatal("dry run reported no targets")
	}
	for _, target := range targets {
		if target.State != cleanTargetPending {
			t.Fatalf("dry run moved a target: %+v", target)
		}
		if _, err := os.Lstat(target.Path); err != nil {
			t.Fatalf("dry run removed %s: %v", target.Path, err)
		}
	}
}

// 実行は登録外の実体だけを消し、登録済みの slot と archive は残す。
func TestCleanUnmanagedRemovesOnlyTheUnexplainedEntities(t *testing.T) {
	ctx, manager, _, root := unmanagedFixture(t)
	reply, err := manager.CleanUnmanaged(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if errs, _ := reply["errors"].([]string); len(errs) != 0 {
		t.Fatalf("errors=%v", errs)
	}
	for _, target := range unmanagedReplyTargets(t, reply) {
		if target.State != cleanTargetDone {
			t.Fatalf("target=%+v", target)
		}
		if _, err := os.Lstat(target.Path); !os.IsNotExist(err) {
			t.Fatalf("%s survived: %v", target.Path, err)
		}
	}
	snapshots := filepath.Join(root, filepath.FromSlash(archive.WorkspaceSnapshotDirectory))
	if _, err := os.Lstat(filepath.Join(snapshots, "registered.tar")); err != nil {
		t.Fatalf("the registered archive was removed: %v", err)
	}
	// 2 度目は対象が無くなり、同じ要求を繰り返しても失敗にならない。
	again, err := manager.CleanUnmanaged(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(unmanagedReplyTargets(t, again)) != 0 {
		t.Fatalf("targets remained=%+v", again["targets"])
	}
}

// pin できない root 世代は errors へ落とし、他の root の削除は続ける。
func TestCleanUnmanagedContinuesAfterARootItCannotPin(t *testing.T) {
	ctx, manager, store, _ := unmanagedFixture(t)
	missing := filepath.Join(t.TempDir(), "gone")
	if err := os.MkdirAll(missing, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := pathIdentity(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureActiveRoot(ctx, missing, identity); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	reply, err := manager.CleanUnmanaged(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if errs, _ := reply["errors"].([]string); len(errs) == 0 {
		t.Fatal("an unreachable root generation was reported as inspected")
	}
	if len(unmanagedReplyTargets(t, reply)) == 0 {
		t.Fatal("the remaining root was not processed")
	}
}

// slot lock を取った後に登録が増えていれば消さない。並走する準備が作り始めた worktree を消さないための締めである。
func TestCleanUnmanagedSkipsAPathThatBecameRegistered(t *testing.T) {
	ctx, manager, store, root := unmanagedFixture(t)
	registered, err := store.SlotArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 1 {
		t.Fatalf("registered slots=%+v", registered)
	}
	// 登録済みの slot directory を対象として渡し、lock 取得後の再確認だけを働かせる。
	artifact := unmanagedArtifact{Root: root, Path: filepath.Clean(registered[0].Path), Kind: unmanagedSlotDirectory}
	relative, ok := relativeWithinRoot(root, artifact.Path)
	if !ok {
		t.Fatalf("registered slot %s is outside %s", artifact.Path, root)
	}
	artifact.RelPath = relative
	var result, reason string
	if err := manager.withVerifiedRoot(root, func(owner *os.Root) error {
		result, reason = manager.removeUnmanagedArtifact(ctx, owner, artifact)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result != cleanTargetSkipped || reason == "" {
		t.Fatalf("state=%s reason=%q", result, reason)
	}
	if _, err := os.Lstat(artifact.Path); err != nil {
		t.Fatalf("a registered slot directory was removed: %v", err)
	}
}

// 実体を消していない dry-run は使用量の測り直しを要求しない。要求を畳んだ実行中の測定で確認する。
func TestCleanUnmanagedDoesNotRequestUsageRefreshWithoutRemoval(t *testing.T) {
	ctx, manager, _, _ := unmanagedFixture(t)
	manager.usageMu.Lock()
	manager.usageRunning, manager.usageDirty = true, false
	manager.usageMu.Unlock()
	t.Cleanup(func() {
		manager.usageMu.Lock()
		manager.usageRunning, manager.usageDirty = false, false
		manager.usageMu.Unlock()
	})

	if _, err := manager.CleanUnmanaged(ctx, true); err != nil {
		t.Fatal(err)
	}
	manager.usageMu.Lock()
	dirty := manager.usageDirty
	manager.usageMu.Unlock()
	if dirty {
		t.Fatal("dry-run requested a root usage refresh")
	}
}

// 実体を 1 件でも消した clean は、実行中の測定へ次の巡回を要求する。
func TestCleanUnmanagedRequestsUsageRefreshAfterRemoval(t *testing.T) {
	ctx, manager, _, _ := unmanagedFixture(t)
	manager.usageMu.Lock()
	manager.usageRunning, manager.usageDirty = true, false
	manager.usageMu.Unlock()
	t.Cleanup(func() {
		manager.usageMu.Lock()
		manager.usageRunning, manager.usageDirty = false, false
		manager.usageMu.Unlock()
	})

	reply, err := manager.CleanUnmanaged(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	summary, ok := reply["summary"].(map[string]int)
	if !ok || summary[cleanTargetDone] == 0 {
		t.Fatalf("clean summary=%v, want a removed target", reply["summary"])
	}
	manager.usageMu.Lock()
	dirty := manager.usageDirty
	manager.usageMu.Unlock()
	if !dirty {
		t.Fatal("removing unmanaged artifacts did not request a root usage refresh")
	}
}

// 要約は各状態を 1 件ずつ数え、表示件数と終了判定を負数にしない。
func TestUnmanagedSummaryCountsEachTargetState(t *testing.T) {
	targets := []unmanagedTarget{
		{State: cleanTargetPending},
		{State: cleanTargetPending},
		{State: cleanTargetDone},
		{State: cleanTargetFailed},
		{State: cleanTargetSkipped},
	}
	summary := unmanagedSummary(targets)
	want := map[string]int{
		"total":            5,
		cleanTargetPending: 2,
		cleanTargetDone:    1,
		cleanTargetFailed:  1,
		cleanTargetSkipped: 1,
	}
	for key, count := range want {
		if summary[key] != count {
			t.Fatalf("summary[%q]=%d, want %d (all=%v)", key, summary[key], count, summary)
		}
	}
}

// 同じ path が重複しても比較関数を strict に保ち、入力順を崩さずに表示を決定的にする。
func TestSortUnmanagedTargetsKeepsEqualPathOrder(t *testing.T) {
	targets := []unmanagedTarget{
		{Path: "/root/b", Kind: "b"},
		{Path: "/root/same", Kind: "first"},
		{Path: "/root/same", Kind: "second"},
		{Path: "/root/a", Kind: "a"},
	}
	sortUnmanagedTargets(targets)
	want := []struct {
		path string
		kind string
	}{
		{path: "/root/a", kind: "a"},
		{path: "/root/b", kind: "b"},
		{path: "/root/same", kind: "first"},
		{path: "/root/same", kind: "second"},
	}
	for index, item := range want {
		if targets[index].Path != item.path || targets[index].Kind != item.kind {
			t.Fatalf("targets[%d]=%+v, want path=%q kind=%q (all=%+v)", index, targets[index], item.path, item.kind, targets)
		}
	}
}
