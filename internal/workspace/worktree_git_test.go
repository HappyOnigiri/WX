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

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// NEW-2: Git の add syscall は記述子で予約した対象名前空間を使う必要がある。
// 親を開いた後 Git 開始前にルートを置き換えても、ファイルと登録は逃げない。
func TestAddWorktreeUsesReservedNamespaceAcrossRootReplacement(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	root := filepath.Join(base, "worktrees")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", "tracked")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := owner.MkdirAll("slot", 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	p := Preparer{Git: runner, Config: func() config.Config { cfg := config.Defaults(); cfg.Storage.WorktreeRoot = root; return cfg }(), OwnedRoot: owner, RootPath: root}
	target := filepath.Join(root, "slot", "root")
	runner.SetBeforeRunAtHook(func(args []string) {
		if len(args) < 3 || args[0] != "--git-dir" {
			return
		}
		old := root + "-old"
		if err := os.Rename(root, old); err != nil {
			t.Fatalf("replace worktree root: %v", err)
		}
		if err := os.Symlink(outside, root); err != nil {
			t.Fatalf("install replacement root: %v", err)
		}
	})
	if err := p.addWorktree(context.Background(), repo, owner, target, filepath.Join("slot", "root"), head); err != nil {
		t.Fatalf("descriptor-bound worktree add failed: %v", err)
	}
	oldTarget := filepath.Join(root+"-old", "slot", "root")
	if info, err := os.Stat(oldTarget); err != nil || !info.IsDir() {
		t.Fatalf("worktree was not created in reserved namespace: info=%v err=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "slot", "root")); !os.IsNotExist(err) {
		t.Fatalf("Git created worktree outside wx root: %v", err)
	}
	registered := gitOutput(t, repository, "worktree", "list", "--porcelain")
	if strings.Contains(registered, outside) || !strings.Contains(registered, oldTarget) {
		t.Fatalf("Git registration escaped reserved namespace: %q", registered)
	}
}

// TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaultsは、descriptor処理を完全に省くunpinned/no-identity経路を確認する。
// identityまたはpinned rootが関与した場合だけ発生するdescriptor束縛の障害も確認する。
func TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}

	if _, err := preparer.RunGitInWorktree(ctx, target, "", nil, nil, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("unpinned no-identity fast path: %v", err)
	}

	preparer.RootPath = root
	preparer.OwnedRoot = nil
	if _, err := preparer.RunGitInWorktree(ctx, target, "identity", nil, nil, "rev-parse", "HEAD"); err == nil {
		t.Fatal("pinned command with a missing root descriptor succeeded")
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	if _, err := preparer.RunGitInWorktree(ctx, target, "not-the-real-identity", nil, nil, "rev-parse", "HEAD"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-command identity error=%v", err)
	}
}

// TestWorktreeIdentityPropagatesDescriptorAndOpenFailuresは、WorktreeIdentityのdescriptor open失敗と後続のdirectory open失敗を確認する。
// 前者は設定root、後者は祖先でなくtarget自身を検索不能にして再現する。
func TestWorktreeIdentityPropagatesDescriptorAndOpenFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.WorktreeIdentity(target); err == nil {
		_ = os.Chmod(root, 0o700)
		t.Fatal("worktree identity behind an unsearchable configured root succeeded")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	if _, err := preparer.WorktreeIdentity(target); err == nil {
		t.Fatal("worktree identity for an unsearchable target succeeded")
	}
}

// TestRemoveWorktreeAtRequiresAPinnedRootDescriptorは、pinned mode外のdescriptor-bound削除を拒否するguardを確認する。
// 他のRemoveWorktreeAtテストは常にpinnedで実行するため、この分岐には到達しない。
func TestRemoveWorktreeAtRequiresAPinnedRootDescriptor(t *testing.T) {
	t.Parallel()
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := preparer.RemoveWorktreeAt(context.Background(), repo, root, target, "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unpinned descriptor-bound removal error=%v", err)
	}
}

// testlint:allow-serial -- プロセス全体の環境（PATH と WX_FAULT_*）を変更するため
func TestAddWorktreeWithIdentityRecognizesACleanFailedAdd(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	// gitは予約leafへの書き込みやworktree登録前に失敗するため、addWorktreeWithIdentityは予約namespaceをcleanと判断する。
	// その結果、所有権が不確かなエラーではなく元のGitエラーを返す。
	installGitFault(t, "worktree add --detach", 1)
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil || errors.Is(err, state.ErrOwnership) {
		t.Fatalf("clean failed add error=%v, want a plain Git error", err)
	}
	if _, _, found, err := RegisteredWorktreeLockStatusAt(context.Background(), preparer.Git, string(repo.MainPath), owner, root, filepath.Join(testSlotRelPath, testRepositoryID), "irrelevant"); err != nil || found {
		t.Fatalf("failed add left a Git registration: found=%v err=%v", found, err)
	}
}

// testlint:allow-serial -- プロセス全体の環境（PATH）を変更するため
func TestAddWorktreeWithIdentityQuarantinesAnInterruptedAdd(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	// Gitが予約worktree namespaceへの書き込み後、成功報告前に中断された状態を再現する。
	// 予約leafが空でないため、addWorktreeWithIdentityは単純な再試行可能エラーでなく不確かな結果（ownership quarantine）として扱う。
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\"worktree add --detach\"*)\n" +
		"    : > stray-partial-file\n" +
		"    printf 'interrupted\\n' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + realGit + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("interrupted add error=%v, want an ownership-uncertain error", err)
	}
}

// TestAddWorktreeWithIdentityPropagatesLeafReservationFailureは、addWorktreeWithIdentityを直接呼び出してleaf予約のmkdirat失敗分岐を確認する。
// 同じread-only parentで先に失敗するownership marker作成を迂回し、leaf既存とは別の分岐を対象にする。
func TestAddWorktreeWithIdentityPropagatesLeafReservationFailure(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	if _, err := preparer.addWorktreeWithIdentity(context.Background(), repo, owner, target, relativeTarget, head); err == nil {
		t.Fatal("worktree leaf reservation below a read-only parent succeeded")
	}
}

// TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommandは、runWorktreeAdminOwned固有のcommand前identity証明を直接確認する。
// 上位のPrepare/RemoveWorktreeAt flowでは、意図的に誤ったidentityを与えるこの条件を検査しない。
func TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.runWorktreeAdminOwned(ctx, repo, owner, relative, target, "not-the-real-identity", "unlock"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-command admin identity error=%v", err)
	}
}

// TestVerifyPreparedTargetIdentityDetectsMismatchAndUnavailabilityは、verifyPreparedTargetIdentityを直接呼び、target消失とidentity不一致の両失敗分岐を確認する。
func TestVerifyPreparedTargetIdentityDetectsMismatchAndUnavailability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.verifyPreparedTargetIdentity(owner, relative, "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched identity error=%v", err)
	}
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := preparer.verifyPreparedTargetIdentity(owner, relative, "any-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unavailable identity error=%v", err)
	}
}

// TestAddWorktreeWithIdentityRejectsAReservedLeafThatIsNotADirectoryは、mkdirat予約が既存leaf（os.ErrExist）を許容する分岐を確認する。
// そのleafがaddWorktreeWithIdentityの次のopenに必要な物理directoryでない場合は拒否する。
func TestAddWorktreeWithIdentityRejectsAReservedLeafThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.addWorktreeWithIdentity(context.Background(), repo, owner, target, relativeTarget, head); err == nil {
		t.Fatal("worktree add reserved a regular file as its target leaf")
	}
}
