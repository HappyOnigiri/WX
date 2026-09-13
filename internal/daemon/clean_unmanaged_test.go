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
