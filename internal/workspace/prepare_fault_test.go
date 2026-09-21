package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpointは、prepareLockedの各再検証地点で所有権変更を失敗させる。
// ownership validatorの失敗回数を変え、includes・links・prepare command・tracked-status・READY前後の全7回をfailAt=1..7で網羅する。
func TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpoint(t *testing.T) {
	t.Parallel()
	for failAt := 1; failAt <= 7; failAt++ {
		t.Run(fmt.Sprintf("failAt=%d", failAt), func(t *testing.T) {
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			preparer.Ownership = &edgeCountingOwnershipValidator{failAt: failAt}
			if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
				t.Fatalf("prepare succeeded despite the ownership validator failing at call %d", failAt)
			}
		})
	}
}

// TestCopyIncludesAndCreateLinksPropagateDescriptorFaultsは、pinned modeのcopyIncludes/createLinks共通のroot descriptor欠落を確認する。
// copyIncludesAt/createLinksAtでは検索不能なdestination targetを使い、既存テストのunpinned経路だけでは届かない障害を確認する。
func TestCopyIncludesAndCreateLinksPropagateDescriptorFaults(t *testing.T) {
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

	preparer.RootPath = root
	preparer.OwnedRoot = nil
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("copyIncludes with a missing pinned root descriptor succeeded")
	}
	if err := preparer.createLinks(ctx, repo, target); err == nil {
		t.Fatal("createLinks with a missing pinned root descriptor succeeded")
	}

	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner

	if err := os.Chmod(target, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("pinned copyIncludes opened an unsearchable destination")
	}
	if err := preparer.createLinks(ctx, repo, target); err == nil {
		t.Fatal("pinned createLinks opened an unsearchable destination")
	}
}

// TestPinnedPrepareFailureFullyCleansUpAndRemovesOwnershipMarkerは、pinned modeのprepareLocked遅延cleanupを確認する。
// prepare commandが失敗してもreserved worktreeをunlock・削除し、descriptor-bound ownership markerも削除しなければならない。
func TestPinnedPrepareFailureFullyCleansUpAndRemovesOwnershipMarker(t *testing.T) {
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

	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 1"}, Timeout: &config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err == nil {
		t.Fatal("prepare succeeded despite a failing prepare command")
	}
	if _, _, found, err := RegisteredWorktreeLockStatusAt(ctx, preparer.Git, string(repo.MainPath), owner, root, filepath.Join("slots", "slot", "root"), "irrelevant"); err != nil || found {
		t.Fatalf("pinned cleanup left a Git registration: found=%v err=%v", found, err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("pinned cleanup left the target directory: %v", err)
	}
}

// 既存 worktree の CoW 交換残骸は、通常の再準備として上書きせず所有権不確実として返す。
func TestPrepareRejectsExistingCOWTemporary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, ".wx-cow-leftover"), []byte("interrupted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := preparer.Prepare(ctx, repo, target, head, "slot")
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("existing CoW temporary was accepted: %v", err)
	}
}

// completePrepare は link 配置後にも target identity を再検証し、検証後に置き換えられた directory を READY へ進めない。
func TestCompletePrepareRejectsTargetReplacementBeforeFinalChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatal(err)
	}
	locked := &lockedTarget{root: preparer.OwnedRoot, relative: relative, identity: identity, existing: true, close: func() {}}
	result := &submodulePhaseResult{}
	err = preparer.completePrepare(ctx, repo, target, head, "slot", preparePhaseRestore, locked, cowPlacement{}, result,
		func() error { return nil },
		func() error { return nil },
		func() error { return nil },
		func() error {
			backup := target + ".replaced"
			if err := os.Rename(target, backup); err != nil {
				return err
			}
			return os.Mkdir(target, 0o700)
		})
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement after validation was accepted: %v", err)
	}
}

type replaceTargetDuringOwnershipValidation struct {
	target   string
	replaced bool
}

func (v *replaceTargetDuringOwnershipValidation) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	if !v.replaced {
		backup := v.target + ".old"
		if err := os.Rename(v.target, backup); err != nil {
			return state.WorktreeOwnership{}, err
		}
		if err := os.Mkdir(v.target, 0o000); err != nil {
			return state.WorktreeOwnership{}, err
		}
		v.replaced = true
	}
	return state.WorktreeOwnership{}, nil
}

// prepareLockedTarget は既存 target の identity を取得できない場合に、空の identity のまま書込みへ進まない。
func TestPrepareLockedTargetPropagatesExistingIdentityOpenFailure(t *testing.T) {
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
	preparer.Ownership = &replaceTargetDuringOwnershipValidation{target: target}
	if _, err := preparer.prepareLockedTarget(ctx, repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("existing target identity open failure was ignored")
	}
}

// TestPrepareLockedTargetPropagatesRevalidationDescriptorFailureは、common-directory lock取得後に行うprepareLockedTarget固有のdescriptor再openを確認する。
// path置換競合を防ぐ再検査であり、prepareTargetの先行検査前でも検索不能な設定rootは失敗しなければならない。
func TestPrepareLockedTargetPropagatesRevalidationDescriptorFailure(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := preparer.prepareOwned(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget opened an unsearchable configured root")
	}
}

// TestPrepareLockedTargetPropagatesParentCreationFailureは、「既存」と異なるMkdirAll失敗分岐を確認する。
// targetの祖父母が書き込みを拒否するため、targetの親作成が恒久的なエラーになる。
func TestPrepareLockedTargetPropagatesParentCreationFailure(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	workspaceDirectory := filepath.Join(root, testWorkspaceID)
	if err := os.MkdirAll(workspaceDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(workspaceDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspaceDirectory, 0o700) })
	if err := preparer.prepareOwned(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget created a worktree parent below a read-only directory")
	}
}

// TestPrepareLockedTargetPropagatesMarkerWriteFailureは、prepareLocked固有のmarkerErr分岐を確認する。
// targetの親は存在してMkdirAllが成功するが書き込みを拒否するため、未作成targetの横へのmarker書き込みが失敗する。
func TestPrepareLockedTargetPropagatesMarkerWriteFailure(t *testing.T) {
	t.Parallel()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	slotDirectory := filepath.Dir(target)
	if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(slotDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
	err := preparer.prepareOwned(context.Background(), repo, target, head, "slot", preparePhaseCreate, root)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("prepareLockedTarget wrote an ownership marker below a read-only directory: %v", err)
	}
}

// TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResumeは、PrepareResumeWithIdentity先頭のidentity証明を確認する。
// 他のPrepareResumeでは空の（互換目的で常に通る）identityしか検査されない。
func TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-resume identity error=%v", err)
	}
}

// TestPrepareResumeWithIdentityDetectsTargetReplacementDuringResumeCommandは、resume phaseのcommand後identity再検査を確認する。
// 上のCREATE phaseと同じ置換をPrepareResumeWithIdentity固有の呼び出し順で検証する。
func TestPrepareResumeWithIdentityDetectsTargetReplacementDuringResumeCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	script := "parent=$(dirname \"$PWD\"); name=$(basename \"$PWD\"); cd \"$parent\" && rm -rf \"$name\" && mkdir \"$name\""
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: &config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the resume command was not detected: %v", err)
	}
}

// TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlockは、FinishRestoreWithIdentity先頭のidentity証明を確認する。
// 上のresume phaseと同じ条件を扱う。
func TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", "not-the-real-identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched pre-finish identity error=%v", err)
	}
}

// TestFinishRestoreWithIdentityPropagatesUnlockAndLockFailuresは、「repository欠落」と異なるadmin Git command失敗分岐を確認する。
// 実在repositoryで特定のunlock/lock呼び出しを失敗させる。
// testlint:allow-serial -- プロセス全体の環境（PATH と WX_FAULT_*）を変更するため
func TestFinishRestoreWithIdentityPropagatesUnlockAndLockFailures(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
	}{
		{name: "unlock fails", pattern: "worktree unlock"},
		{name: "lock fails", pattern: "worktree lock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
				t.Fatal(err)
			}
			// installGitFaultはこの時点以降にPATHへ入れたfault wrapper経由の呼び出しだけを捕捉する。
			// 既に完了したPrepareForRestoreのGit呼び出しは数えず、FinishRestoreWithIdentityのunlock/lockが最初になる。
			installGitFault(t, test.pattern, 1)
			if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", ""); err == nil {
				t.Fatalf("finish restore succeeded despite a failing %q", test.pattern)
			}
		})
	}
}

// FinishRestoreWithIdentity が lock 後にも同じ physical directory を要求することを、成功した Git lock 後の path 置換で確認する。
// testlint:allow-serial -- プロセス全体の PATH を変更するため
func TestFinishRestoreWithIdentityRejectsReplacementAfterReadyLock(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
case " $* " in
  *" worktree lock "*)
    "$WX_REPLACE_REAL_GIT" "$@"
    status=$?
    if [ "$status" -eq 0 ]; then
      backup="$WX_REPLACE_TARGET.replaced"
      rm -rf "$backup"
      mv "$WX_REPLACE_TARGET" "$backup"
      cp -Rp "$backup" "$WX_REPLACE_TARGET"
      rm -rf "$backup"
    fi
    exit "$status"
    ;;
esac
exec "$WX_REPLACE_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REPLACE_REAL_GIT", realGit)
	t.Setenv("WX_REPLACE_TARGET", target)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	err = preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", "")
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replacement after READY lock was accepted: %v", err)
	}
}

// TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailuresは、existingTargetStateを直接呼び出してfilesystem error分岐を確認する。
// 通常のPrepare flowではprepareLockedTargetが直前に同じLstatを行うため、独立して到達できない。
func TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, _ := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot

	t.Run("target lookup blocked by an unsearchable parent", func(t *testing.T) {
		slotDirectory := filepath.Join(root, testWorkspaceID, "blockd")
		target := filepath.Join(slotDirectory, testRepositoryID)
		if err := os.MkdirAll(slotDirectory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(slotDirectory, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(slotDirectory, 0o700) })
		owner, _, err := domain.OpenOwnedRoot(root, root)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		relative, err := filepath.Rel(root, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := preparer.existingTargetState(ctx, repo, target, head, "slot", preparePhaseCreate, root, owner, relative); err == nil {
			t.Fatal("existing target state lookup below an unsearchable directory succeeded")
		}
	})

	t.Run("target directory itself is unsearchable", func(t *testing.T) {
		slotDirectory := filepath.Join(root, testWorkspaceID, "blocko")
		target := filepath.Join(slotDirectory, testRepositoryID)
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
		relative, err := filepath.Rel(root, target)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := preparer.existingTargetState(ctx, repo, target, head, "slot", preparePhaseCreate, root, owner, relative); err == nil {
			t.Fatal("existing target state opened an unsearchable directory")
		}
	})
}

// 空の allocation shell でも marker path を組めない repository ID は成功扱いにしない。
func TestExistingTargetStatePropagatesInvalidMarkerPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, relative, err := domain.OpenOwnedRoot(root, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	badRepo := repo
	badRepo.ID = domain.RepositoryID("../invalid")
	if _, err := preparer.existingTargetState(ctx, badRepo, target, head, "slot", preparePhaseCreate, root, owner, relative); err == nil {
		t.Fatal("invalid ownership marker path was accepted for an empty allocation")
	}
}

// TestPrepareOnANewWorktreePropagatesFinalUnlockAndReadyLockFailuresは、CREATE phaseのPREPARINGからREADYへの終了遷移を確認する。
// 他では開始時のPREPARING lockだけが対象になる。
// testlint:allow-serial -- プロセス全体の環境（PATH と WX_FAULT_*）を変更するため
func TestPrepareOnANewWorktreePropagatesFinalUnlockAndReadyLockFailures(t *testing.T) {
	t.Run("final unlock fails", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// 新規worktreeは既存worktreeをunlockしないため、最初の「worktree unlock」はPREPARINGからREADYへの終了遷移である。
		installGitFault(t, "worktree unlock", 1)
		if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
			t.Fatal("prepare succeeded despite a failing final unlock")
		}
	})
	t.Run("ready lock fails", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		root := preparer.Config.Storage.WorktreeRoot
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		// 最初の「worktree lock」は開始時のPREPARING lock、2回目は終了時のREADY lockである。
		installGitFault(t, "worktree lock", 2)
		if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
			t.Fatal("prepare succeeded despite a failing READY lock")
		}
	})
}

// finishPrepare は READY lock 成功後にも同じ physical directory を要求し、検証を飛ばして成功扱いにしない。
// testlint:allow-serial -- プロセス全体の PATH と置換用 counter を変更するため
func TestPrepareRejectsReplacementAfterReadyLock(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	counter := filepath.Join(bin, "lock-count")
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
case " $* " in
  *" worktree lock "*)
    "$WX_REPLACE_REAL_GIT" "$@"
    status=$?
    if [ "$status" -eq 0 ]; then
      count=0
      if [ -f "$WX_REPLACE_COUNTER" ]; then
        count=$(cat "$WX_REPLACE_COUNTER")
      fi
      count=$((count + 1))
      printf '%s\n' "$count" > "$WX_REPLACE_COUNTER"
      if [ "$count" -eq 2 ]; then
        backup="$WX_REPLACE_TARGET.replaced"
        rm -rf "$backup"
        mv "$WX_REPLACE_TARGET" "$backup"
        cp -Rp "$backup" "$WX_REPLACE_TARGET"
        rm -rf "$backup"
      fi
    fi
    exit "$status"
    ;;
esac
exec "$WX_REPLACE_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REPLACE_REAL_GIT", realGit)
	t.Setenv("WX_REPLACE_COUNTER", counter)
	t.Setenv("WX_REPLACE_TARGET", target)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replacement after READY lock was accepted: %v", err)
	}
}
