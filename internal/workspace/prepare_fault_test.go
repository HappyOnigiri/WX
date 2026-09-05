package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpointは、prepareLockedの各再検証地点で所有権変更を失敗させる。
// ownership validatorの失敗回数を変え、includes・links・prepare command・tracked-status・READY前後の全7回をfailAt=1..7で網羅する。
func TestPrepareRejectsOwnershipChangesAtEachRevalidationCheckpoint(t *testing.T) {
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
	if err := preparer.ValidateReady(ctx, repo, target, head); err == nil || err.Error() != "ready-specific state proof rejected" {
		t.Fatalf("ValidateReady accepted despite its own state proof failing: %v", err)
	}
}

// TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaultsは、descriptor処理を完全に省くunpinned/no-identity経路を確認する。
// identityまたはpinned rootが関与した場合だけ発生するdescriptor束縛の障害も確認する。
func TestRunGitInWorktreeUnpinnedFastPathAndDescriptorFaults(t *testing.T) {
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

// TestCopyIncludesAndCreateLinksPropagateDescriptorFaultsは、pinned modeのcopyIncludes/createLinks共通のroot descriptor欠落を確認する。
// copyIncludesAt/createLinksAtでは検索不能なdestination targetを使い、既存テストのunpinned経路だけでは届かない障害を確認する。
func TestCopyIncludesAndCreateLinksPropagateDescriptorFaults(t *testing.T) {
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
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 1"}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
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

// TestMaterializeRootAtRejectsSymlinkAncestorInCopyRuleは、copy ruleの存在検査でErrNotExist以外を扱う分岐を確認する。
// symlink祖先を通るcopy ruleは単なる「欠落」とせず拒否する。
func TestMaterializeRootAtRejectsSymlinkAncestorInCopyRule(t *testing.T) {
	source := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	destinationRoot, err := OpenPhysicalRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	rules := config.Workspace{Copy: []string{filepath.Join("linked", "value")}}
	if err := MaterializeRootAt(source, destinationRoot, rules); err == nil {
		t.Fatal("workspace copy rule through a symlink ancestor was accepted")
	}
}

// TestRemoveWorktreeAtRequiresAPinnedRootDescriptorは、pinned mode外のdescriptor-bound削除を拒否するguardを確認する。
// 他のRemoveWorktreeAtテストは常にpinnedで実行するため、この分岐には到達しない。
func TestRemoveWorktreeAtRequiresAPinnedRootDescriptor(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := preparer.RemoveWorktreeAt(context.Background(), repo, root, target, "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unpinned descriptor-bound removal error=%v", err)
	}
}

// TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergenceは、markerに依存しないworktree自身の検査を確認する。
// 削除済みtarget、欠落・安全でない.git marker、Git pointerとして機能しない.git fileを扱う。
func TestValidateExistingWorktreeOwnedForStatesCoversPhysicalAndGitDivergence(t *testing.T) {
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

// TestPrepareLockedTargetPropagatesRevalidationDescriptorFailureは、common-directory lock取得後に行うprepareLockedTarget固有のdescriptor再openを確認する。
// path置換競合を防ぐ再検査であり、prepareTargetの先行検査前でも検索不能な設定rootは失敗しなければならない。
func TestPrepareLockedTargetPropagatesRevalidationDescriptorFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget opened an unsearchable configured root")
	}
}

// TestPrepareLockedTargetPropagatesParentCreationFailureは、「既存」と異なるMkdirAll失敗分岐を確認する。
// targetの祖父母が書き込みを拒否するため、targetの親作成が恒久的なエラーになる。
func TestPrepareLockedTargetPropagatesParentCreationFailure(t *testing.T) {
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
	if err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root); err == nil {
		t.Fatal("prepareLockedTarget created a worktree parent below a read-only directory")
	}
}

// TestPrepareLockedTargetPropagatesMarkerWriteFailureは、prepareLocked固有のmarkerErr分岐を確認する。
// targetの親は存在してMkdirAllが成功するが書き込みを拒否するため、未作成targetの横へのmarker書き込みが失敗する。
func TestPrepareLockedTargetPropagatesMarkerWriteFailure(t *testing.T) {
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
	err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root)
	if err == nil || !strings.Contains(err.Error(), "marker") {
		t.Fatalf("prepareLockedTarget wrote an ownership marker below a read-only directory: %v", err)
	}
}

// TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpectedは、非空identityがdescriptor-bound command経路を強制する分岐を確認する。
// preparerがunpinnedでも設定rootが未作成ならopenに失敗し、identityを無視する通常のexec.Commandへ黙ってfallbackしてはならない。
func TestRunPrepareWithIdentityForcesDescriptorPathWhenIdentityExpected(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command with a missing configured root succeeded")
	}
}

// TestRunPrepareWithIdentityPropagatesTargetOpenFailureは、descriptor-bound経路でtarget openが失敗する分岐を確認する。
// 設定rootは存在してopenOwnedRootが成功するが、targetは存在しないためディレクトリopenに失敗する。
func TestRunPrepareWithIdentityPropagatesTargetOpenFailure(t *testing.T) {
	_, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "some-identity"); err == nil {
		t.Fatal("descriptor-bound prepare command opened a missing target")
	}
}

// TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommandは、command後のidentity検査を確認する。
// prepare commandが終了前に同じpathのtargetを別directoryへ置換した場合、元のままではなく所有権不確かなidentity変更として検出する。
func TestRunPrepareWithIdentityDetectsTargetReplacementDuringCommand(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	// commandは終了前に自身のworking directoryを同名の新しい空directoryへ置換する。
	// そのためcmd.Run()の返却時にはtargetが別の物理inodeを指す。
	script := "parent=$(dirname \"$PWD\"); name=$(basename \"$PWD\"); cd \"$parent\" && rm -rf \"$name\" && mkdir \"$name\""
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(context.Background(), repo, target, identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the prepare command was not detected: %v", err)
	}
}

// TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResumeは、PrepareResumeWithIdentity先頭のidentity証明を確認する。
// 他のPrepareResumeでは空の（互換目的で常に通る）identityしか検査されない。
func TestPrepareResumeWithIdentityRejectsAMismatchedIdentityBeforeResume(t *testing.T) {
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
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
	preparer.Config = cfg
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", identity); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target replacement during the resume command was not detected: %v", err)
	}
}

// TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlockは、FinishRestoreWithIdentity先頭のidentity証明を確認する。
// 上のresume phaseと同じ条件を扱う。
func TestFinishRestoreWithIdentityRejectsAMismatchedIdentityBeforeUnlock(t *testing.T) {
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

// TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailuresは、existingTargetStateを直接呼び出してfilesystem error分岐を確認する。
// 通常のPrepare flowではprepareLockedTargetが直前に同じLstatを行うため、独立して到達できない。
func TestExistingTargetStatePropagatesLstatAndDirectoryOpenFailures(t *testing.T) {
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

// TestAddWorktreeWithIdentityPropagatesLeafReservationFailureは、addWorktreeWithIdentityを直接呼び出してleaf予約のmkdirat失敗分岐を確認する。
// 同じread-only parentで先に失敗するownership marker作成を迂回し、leaf既存とは別の分岐を対象にする。
func TestAddWorktreeWithIdentityPropagatesLeafReservationFailure(t *testing.T) {
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

// TestPrepareOnANewWorktreePropagatesFinalUnlockAndReadyLockFailuresは、CREATE phaseのPREPARINGからREADYへの終了遷移を確認する。
// 他では開始時のPREPARING lockだけが対象になる。
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

// TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommandは、runWorktreeAdminOwned固有のcommand前identity証明を直接確認する。
// 上位のPrepare/RemoveWorktreeAt flowでは、意図的に誤ったidentityを与えるこの条件を検査しない。
func TestRunWorktreeAdminOwnedRejectsAMismatchedIdentityBeforeTheGitCommand(t *testing.T) {
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

// TestFingerprintRejectsAMissingRepositoryMainPathは、存在しないrepositoryに対するFingerprint先頭のphysical-path検査を確認する。
// 別テストのsymlink祖先や入力不可のケースとは異なる。
func TestFingerprintRejectsAMissingRepositoryMainPath(t *testing.T) {
	missing := domain.CanonicalPath(filepath.Join(t.TempDir(), "missing-repository"))
	if _, err := Fingerprint(1, "oid", discovery.Repository{MainPath: missing}, config.Defaults()); err == nil {
		t.Fatal("fingerprint of a missing repository main path succeeded")
	}
}
