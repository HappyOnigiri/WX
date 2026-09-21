package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// readyStateRejectingOwnershipValidatorはREADY状態だけを求める証明以外を許可する。
// ValidateOwnershipの広いライフサイクル証明と、ValidateReady固有の厳密なREADY専用証明を区別できる。
type readyStateRejectingOwnershipValidator struct{}

func (readyStateRejectingOwnershipValidator) ValidateWorktreeOwnership(_ context.Context, req state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	if len(req.AllowedSlotStates) == 1 && req.AllowedSlotStates[0] == "READY" {
		return state.WorktreeOwnership{}, errors.New("ready-specific state proof rejected")
	}
	return state.WorktreeOwnership{}, nil
}

// TestValidateReadyEnforcesItsOwnReadyStateProofは、ValidateReadyが独自の狭い状態所有権検査を行うことを確認する。
// 先に実行されたValidateOwnershipの広い証明だけに依存しない。
func TestValidateReadyEnforcesItsOwnReadyStateProof(t *testing.T) {
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
	preparer.Ownership = readyStateRejectingOwnershipValidator{}
	err := preparer.ValidateReady(ctx, repo, target, head)
	if !errors.Is(err, state.ErrOwnership) || !strings.Contains(err.Error(), "ready-specific state proof rejected") {
		t.Fatalf("ValidateReady accepted despite its own state proof failing: %v", err)
	}
}

// TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergenceは、markerに依存しないworktree自身の検査を確認する。
// 削除済みtarget、欠落・安全でない.git marker、Git pointerとして機能しない.git fileを扱う。
func TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("target removed after marker survives", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil {
			t.Fatal("ownership validation for a removed physical target succeeded")
		}
	})

	t.Run("git marker replaced by a symlink", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		gitFile := filepath.Join(target, ".git")
		if err := os.Remove(gitFile); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), gitFile); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || !strings.Contains(err.Error(), "missing or unsafe .git marker") {
			t.Fatalf("symlinked .git marker error=%v", err)
		}
	})

	t.Run("git marker present but unusable", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		gitFile := filepath.Join(target, ".git")
		if err := os.WriteFile(gitFile, []byte("gitdir: /nonexistent/common/dir\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || strings.Contains(err.Error(), "missing or unsafe .git marker") {
			t.Fatalf("unusable .git marker error=%v, want a Git command failure instead", err)
		}
	})

	t.Run("target becomes unsearchable after the marker validates", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil {
			t.Fatal("ownership validation opened an unsearchable target")
		}
	})

	t.Run("git common directory diverges from the recorded repository", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		// worktreeの.git fileを無関係な別repositoryへ向け、Git commandは動くがslot記録と異なるcommon directoryを返す状態にする。
		otherRepository := t.TempDir()
		gitCommand(t, otherRepository, "init", "-b", "main")
		gitFile := filepath.Join(target, ".git")
		if err := os.WriteFile(gitFile, []byte("gitdir: "+filepath.Join(otherRepository, ".git")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil || !strings.Contains(err.Error(), "common Git directory does not match") {
			t.Fatalf("diverged common directory error=%v", err)
		}
	})
}

func TestWorktreeOwnershipValidationCoversPhysicalAndGitBoundaries(t *testing.T) {
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
		t.Fatalf("prepare descriptor-bound worktree: %v", err)
	}
	if err := preparer.ValidateSlotWorktreeOwnership(ctx, repo, target, head, "slot"); err != nil {
		t.Fatalf("valid replay ownership: %v", err)
	}
	if err := preparer.ValidateRestoringSlotWorktreeOwnership(ctx, repo, target, head, "slot"); err != nil {
		t.Fatalf("restoring replay ownership: %v", err)
	}
	if err := preparer.ValidateReady(ctx, repo, target, head); err != nil {
		t.Fatalf("valid ready ownership: %v", err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.VerifyWorktreeIdentity(target, ""); err != nil {
		t.Fatalf("empty identity compatibility: %v", err)
	}
	if err := preparer.VerifyWorktreeIdentity(target, identity); err != nil {
		t.Fatalf("matching identity: %v", err)
	}
	if err := preparer.VerifyWorktreeIdentity(target, "not-the-target"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched identity error=%v", err)
	}

	if err := preparer.ValidateSlotWorktreeOwnership(ctx, repo, target, "wrong-head", "slot"); err == nil {
		t.Fatal("wrong detached HEAD accepted")
	}
	badCommon := repo
	badCommon.CommonDir = domain.CanonicalPath(t.TempDir())
	if err := preparer.ValidateOwnership(ctx, badCommon, target, head); err == nil {
		t.Fatal("foreign Git common directory accepted")
	}

	marker := filepath.Join(filepath.Dir(target), ownershipMarkerPrefix+string(repo.ID))
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := preparer.ValidateOwnership(ctx, repo, target, head); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing marker error=%v", err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, preparer.markerIdentity(repo, "slot"), string(repo.CommonDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(target, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside-git"), filepath.Join(target, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := preparer.ValidateOwnership(ctx, repo, target, head); err == nil {
		t.Fatal("symlink .git marker accepted")
	}
}

// TestValidateOwnershipReportsCancellationInsteadOfOwnershipFailureは、中断がstate.ErrOwnershipへ畳まれないことを確認する。
// HEADの検査はGitの失敗内容を見ずに結論だけを返すため、ここで中断を握り潰すとcancelしただけのslotがWORKTREE_OWNERSHIP_UNCERTAINで隔離される。
func TestValidateOwnershipReportsCancellationInsteadOfOwnershipFailure(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reached := false
	preparer.Git.SetBeforeRunAtHook(func(args []string) {
		if reached || strings.Join(args, " ") != "rev-parse HEAD" {
			return
		}
		reached = true
		cancel()
	})
	err := preparer.ValidateOwnership(ctx, repo, target, head)
	if !reached {
		t.Fatal("HEAD verification barrier was not reached")
	}
	if !errors.Is(err, context.Canceled) || errors.Is(err, state.ErrOwnership) {
		t.Fatalf("canceled ownership validation returned %v, want a cancellation", err)
	}
}

// 親の Git 設定が submodule の変更を隠しても、prepare command が汚した子は READY へ通さない。
// `submodule.<name>.ignore=all` は通常の利用者設定なので、tracked-clean 検査を親 status の既定へ委ねられない。
func TestValidateTrackedCleanRejectsDirtySubmoduleHiddenByIgnoreConfig(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	gitCommand(t, f.repository, "config", "submodule."+submoduleName+".ignore", "all")
	f.preparer.Config.Storage.CopyMode = config.CopyModeCopy
	f.preparer.Config.Repositories = map[string]config.Repository{
		string(f.repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf prepared > " + submodulePath + "/kid.txt"}}},
	}
	err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot")
	if !errors.Is(err, ErrTrackedChanges) {
		t.Fatalf("prepare error=%v, want %v", err, ErrTrackedChanges)
	}
}

// 子の untracked file は親の `--untracked-files=no` と同じく準備を止めない。
// prepare command が submodule 配下へ生成物を置く構成を tracked 変更と同じに扱わないためである。
func TestValidateTrackedCleanAllowsUntrackedSubmoduleContent(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	f.preparer.Config.Storage.CopyMode = config.CopyModeCopy
	f.preparer.Config.Repositories = map[string]config.Repository{
		string(f.repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf generated > " + submodulePath + "/generated.txt"}}},
	}
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatalf("prepare with untracked submodule content: %v", err)
	}
}
