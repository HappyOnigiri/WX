package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestEnsureOwnershipMarkerAtDirectFaultInjectionは、descriptorに束縛されたmarker writerの防御検査を直接確認する。
// 公開入口はnil/不正ownerを先に拒否するため、marker親の権限障害でのみ残るMkdirAll・Lstat・OpenFileの失敗分岐へ到達できる。
func TestEnsureOwnershipMarkerAtDirectFaultInjection(t *testing.T) {
	marker := ownershipMarker{Version: ownershipMarkerVersion, SlotID: "slot", RootID: testRootID, RepositoryID: testRepositoryID, CommonDir: "common"}

	if err := ensureOwnershipMarkerAt(nil, "marker", marker); err == nil {
		t.Fatal("nil ownership root was accepted")
	}

	t.Run("parent through regular file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "blocker"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("blocker", "marker"), marker); err == nil {
			t.Fatal("marker parent traversing a regular file was accepted")
		}
	})

	t.Run("lookup blocked by unsearchable ancestor", func(t *testing.T) {
		root := t.TempDir()
		blocked := filepath.Join(root, "blocked")
		if err := os.Mkdir(blocked, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(blocked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("blocked", "marker"), marker); err == nil {
			t.Fatal("marker lookup below an unsearchable directory was accepted")
		}
	})

	t.Run("creation blocked by unwritable parent", func(t *testing.T) {
		root := t.TempDir()
		readOnly := filepath.Join(root, "readonly")
		if err := os.Mkdir(readOnly, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(readOnly, 0o755) })
		owner, err := os.OpenRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if err := ensureOwnershipMarkerAt(owner, filepath.Join("readonly", "marker"), marker); err == nil {
			t.Fatal("marker creation inside a read-only directory was accepted")
		}
	})
}

// TestValidateOwnershipMarkerRejectsNonDirectoryTargetWithRequiredLeafは、検証で使うallowMissingTarget=falseのディレクトリ検査を確認する。
// domain.ValidatePhysicalPathはsymlink成分だけを拒否するため、通常ファイルを通過させ、後段のos.Lstat/IsDir検査で捕捉する。
func TestValidateOwnershipMarkerRejectsNonDirectoryTargetWithRequiredLeaf(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target-file")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := ValidateOwnershipMarkerAt(owner, root, target, markerFor("slot"), common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("regular file target validation error=%v", err)
	}
}

// TestRegisteredWorktreeLockStatusSkipsSymlinkAliasedRegistrationsは、symlink祖先を通るGit登録を物理path検査が拒否する境界を確認する。
// 問い合わせ先を解決すれば同じ実体でも照合せず、Gitが作成時に解決するため、管理ファイルをsymlink別名へ書き換えて再現する。
func TestRegisteredWorktreeLockStatusSkipsSymlinkAliasedRegistrations(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	actualParent := filepath.Join(repository, "actual-parent")
	if err := os.Mkdir(actualParent, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(repository, "alias-parent")
	if err := os.Symlink(actualParent, aliasParent); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "worktree", "add", "--detach", filepath.Join(actualParent, "wt"), gitOutput(t, repository, "rev-parse", "HEAD"))

	gitdirFiles, err := filepath.Glob(filepath.Join(repository, ".git", "worktrees", "*", "gitdir"))
	if err != nil || len(gitdirFiles) != 1 {
		t.Fatalf("locate worktree gitdir admin file: files=%v err=%v", gitdirFiles, err)
	}
	aliasedTarget := filepath.Join(aliasParent, "wt")
	if err := os.WriteFile(gitdirFiles[0], []byte(filepath.Join(aliasedTarget, ".git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, found, err := RegisteredWorktreeLockReason(ctx, runner, repository, aliasedTarget); err != nil || found {
		t.Fatalf("symlink-aliased registration was treated as a match: found=%v err=%v", found, err)
	}
}

// TestRegisteredWorktreeLockReasonPropagatesUnresolvableTargetSymlinkLoopは、問い合わせ先のsymlink loopを解決できないエラーが「未検出」に変換されず伝播することを確認する。
func TestRegisteredWorktreeLockReasonPropagatesUnresolvableTargetSymlinkLoop(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	loopA := filepath.Join(repository, "loop-a")
	loopB := filepath.Join(repository, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, _, err := RegisteredWorktreeLockReason(ctx, runner, repository, loopA); err == nil {
		t.Fatal("symlink loop at the queried target was treated as resolvable")
	}
}

// TestRegisteredWorktreeLockStatusAtSkipsRemovedWorktreeRegistrationは、削除途中で実体が消えたworktreeをGitが列挙できる場合のdescriptor open失敗を確認する。
// stale登録はクラッシュや誤照合を起こさずスキップする。
func TestRegisteredWorktreeLockStatusAtSkipsRemovedWorktreeRegistration(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")

	root := t.TempDir()
	removedTarget := filepath.Join(root, "removed")
	keptTarget := filepath.Join(root, "kept")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "worktree", "add", "--detach", removedTarget, head)
	gitCommand(t, repository, "worktree", "add", "--detach", keptTarget, head)

	if err := os.RemoveAll(removedTarget); err != nil {
		t.Fatal(err)
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if _, _, found, err := RegisteredWorktreeLockStatusAt(ctx, runner, repository, owner, root, "removed", "any-identity"); err != nil || found {
		t.Fatalf("removed worktree registration was treated as a match: found=%v err=%v", found, err)
	}
}

// TestRemoveOwnershipMarkerAtPropagatesPermissionFailuresは、「削除済み」と異なるLstat失敗分岐を確認する。
// markerのディレクトリを検索不能にし、os.ErrNotExistへの変換ではなく実際のエラーを返させる。
func TestRemoveOwnershipMarkerAtPropagatesPermissionFailures(t *testing.T) {
	root := t.TempDir()
	slotDirectory := filepath.Join(root, testSlotRelPath)
	target := filepath.Join(slotDirectory, testRepositoryID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := EnsureOwnershipMarkerAt(owner, root, target, markerFor("slot"), common); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotDirectory, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	if err := removeOwnershipMarkerAt(owner, root, target, testRepositoryID); err == nil {
		t.Fatal("descriptor-bound marker removal below an unsearchable directory was accepted")
	}
}

// TestNewOwnershipMarkerAtPropagatesDirectoryOpenFailureForExistingTargetは、対象自身のLstat後にディレクトリopenが失敗する分岐を確認する。
// 親の検索は許可されても対象自身が検索を拒否する場合を扱う。
func TestNewOwnershipMarkerAtPropagatesDirectoryOpenFailureForExistingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, testSlotRelPath, testRepositoryID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	common := t.TempDir()
	if _, err := newOwnershipMarkerAt(owner, root, target, markerFor("slot"), common, true); err == nil {
		t.Fatal("unsearchable existing marker target was accepted")
	}
}

// TestValidateRemovalOwnershipRejectsMalformedMarkerContentsは、ValidateRemovalOwnershipとdescriptor版がreadOwnershipMarkerの内容検証を伝播することを確認する。
// 別テストの対象・common directoryのidentity検査だけで終わらないことを確認する。
func TestValidateRemovalOwnershipRejectsMalformedMarkerContents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, testSlotRelPath, testRepositoryID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := EnsureOwnershipMarkerAt(owner, root, target, markerFor("slot"), common); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(filepath.Dir(target), ownershipMarkerPrefix+testRepositoryID)
	if err := os.WriteFile(markerPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRemovalOwnership(root, target, markerFor("slot"), common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("malformed marker removal proof error=%v", err)
	}

	descriptorTarget := filepath.Join(root, testWorkspaceID, "slot02", testRepositoryID)
	if err := os.MkdirAll(descriptorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, descriptorTarget, markerFor("slot02"), common); err != nil {
		t.Fatal(err)
	}
	descriptorMarkerRelative, err := ownershipMarkerRelative(root, descriptorTarget, testRepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := owner.OpenFile(descriptorMarkerRelative, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateRemovalOwnershipAt(owner, root, descriptorTarget, markerFor("slot02"), common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("malformed descriptor-bound marker removal proof error=%v", err)
	}
}

// TestOpenPhysicalRootPropagatesOwnedRootFailureOnUnsearchableParentは、OpenPhysicalRoot内のdomain.OpenOwnedRootエラー分岐を確認する。
// 物理path検査は祖先の親の検索だけで済むが、直近の親をRootとして開くには親自身の検索権限も必要である。
func TestOpenPhysicalRootPropagatesOwnedRootFailureOnUnsearchableParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "parent")
	target := filepath.Join(parent, "target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	if _, err := OpenPhysicalRoot(target); err == nil {
		t.Fatal("physical root behind an unsearchable parent was opened")
	}
}

// TestOpenPhysicalRootPropagatesReopenFailureOnUnsearchableTargetは、OpenPhysicalRoot内のOpenRootエラー分岐を確認する。
// 物理pathとLstatは対象の親の検索だけで済むが、対象をRootとして再openするには対象自身の検索権限も必要である。
func TestOpenPhysicalRootPropagatesReopenFailureOnUnsearchableTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "blocked")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })
	if _, err := OpenPhysicalRoot(target); err == nil {
		t.Fatal("unsearchable physical root was opened")
	}
}
