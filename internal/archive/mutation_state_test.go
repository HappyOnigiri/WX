package archive

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// TestMutationRestoreGitStatePropagatesEachPhaseError は制御ファイルの消去後に
// tree 展開・再収集・再 tree 化の各失敗を成功へ変換しないことを確認する。
func TestMutationRestoreGitStatePropagatesEachPhaseError(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "expand", want: "list git state tree"},
		{name: "collect", want: "MERGE_HEAD is not a regular file"},
		{name: "write", want: "initialize git state index"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, _, _, _ := archiveFixture(t)
			value, baseRun := directGitAccess(t, repository)
			gitDir := gitCommand(t, repository, "rev-parse", "--absolute-git-dir")
			stale := filepath.Join(gitDir, "MERGE_HEAD")
			if err := os.WriteFile(stale, []byte("stale\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			run := func(env []string, input []byte, args ...string) (gitx.Result, error) {
				if len(args) == 0 {
					return gitx.Result{}, errors.New("empty git command")
				}
				switch test.name {
				case "expand":
					if args[0] == "ls-tree" {
						return gitx.Result{}, errors.New("tree listing failed")
					}
				case "collect":
					if args[0] == "ls-tree" {
						if err := os.Mkdir(stale, 0o700); err != nil {
							t.Fatalf("install collect failure: %v", err)
						}
						return gitx.Result{}, nil
					}
				case "write":
					switch args[0] {
					case "ls-tree":
						return gitx.Result{Stdout: "100644 blob " + strings.Repeat("a", 40) + "\tMERGE_HEAD\x00"}, nil
					case "cat-file":
						return gitx.Result{Stdout: "state\n"}, nil
					case "read-tree":
						return gitx.Result{}, errors.New("temporary index unavailable")
					}
				}
				return baseRun(env, input, args...)
			}
			err := restoreGitState(value, run, "saved-tree")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("restoreGitState error=%v want %q", err, test.want)
			}
			if _, statErr := os.Lstat(stale); !os.IsNotExist(statErr) && test.name == "expand" {
				t.Fatalf("stale git state survived failed restore: %v", statErr)
			}
		})
	}
}

// TestMutationRestorePropagatesOperationStateFailure は snapshot の conflict
// state 復元が失敗したとき、後続の FinishRestore で成功へ変換しないことを確認する。
func TestMutationRestorePropagatesOperationStateFailure(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	snapshot, _, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "operation-state", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	// HEAD の通常 tree を conflict tree として参照させると、復元側の
	// parseConflictTreeEntries が stage path の欠落を検出する。
	snapshot.ConflictRef = "refs/wx/recovery/operation-state/repository/conflict"
	snapshot.ConflictOID = snapshot.HeadOID
	gitCommand(t, repository, "update-ref", snapshot.ConflictRef, snapshot.ConflictOID)
	target := filepath.Join(worktreeRoot, "operation-state", "root")
	pointAtSlot(t, manager, worktreeRoot, target)

	err = manager.Restore(context.Background(), repo, target, "operation-state", snapshot, nil)
	if err == nil || !strings.Contains(err.Error(), "restore unmerged index") {
		t.Fatalf("restore operation-state failure was not propagated: %v", err)
	}
}

// TestMutationVerifyRestoredSubmoduleFailures は子 repository の HEAD、index、
// 一時 index、worktree tree の各照合を失敗時に通過させないことを確認する。
func TestMutationVerifyRestoredSubmoduleFailures(t *testing.T) {
	snapshot := state.SubmoduleSnapshot{HeadOID: "head", IndexTreeOID: "index", WorktreeTreeOID: "worktree"}
	for _, test := range []struct {
		name       string
		failRun    string
		failFinal  bool
		setTempDir bool
	}{
		{name: "temporary index", setTempDir: true},
		{name: "read tree", failRun: "read-tree"},
		{name: "add worktree", failRun: "add"},
		{name: "worktree mismatch", failFinal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.setTempDir {
				t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
			}
			value := func(env []string, args ...string) (string, error) {
				if len(args) == 0 {
					return "", errors.New("empty git command")
				}
				switch args[0] {
				case "rev-parse":
					return snapshot.HeadOID, nil
				case "diff-index":
					return "", nil
				case "write-tree":
					if len(env) > 0 {
						if test.failFinal {
							return "", errors.New("verification write-tree failed")
						}
						return snapshot.WorktreeTreeOID, nil
					}
					return snapshot.IndexTreeOID, nil
				default:
					return "", errors.New("unexpected git value command")
				}
			}
			run := func(_ []string, _ []byte, args ...string) (gitx.Result, error) {
				if len(args) > 0 && args[0] == test.failRun {
					return gitx.Result{}, errors.New(test.name + " failed")
				}
				return gitx.Result{}, nil
			}
			if err := verifyRestoredSubmodule(value, run, snapshot); err == nil {
				t.Fatal("verifyRestoredSubmodule accepted a failed verification phase")
			}
		})
	}

	t.Run("force-added collection", func(t *testing.T) {
		value := func(env []string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "diff-index" {
				return "", errors.New("diff-index failed")
			}
			switch args[0] {
			case "rev-parse":
				return snapshot.HeadOID, nil
			case "write-tree":
				if len(env) == 0 {
					return snapshot.IndexTreeOID, nil
				}
				return snapshot.WorktreeTreeOID, nil
			default:
				return "", errors.New("unexpected git value command")
			}
		}
		run := func(_ []string, _ []byte, _ ...string) (gitx.Result, error) {
			return gitx.Result{}, nil
		}
		if err := verifyRestoredSubmodule(value, run, snapshot); err == nil {
			t.Fatal("verifyRestoredSubmodule accepted a force-added collection failure")
		}
	})
}

// TestMutationDeleteSubmoduleCapsuleRefsChecksExpectedOID は capsule ref の欠落を
// 中断済み削除として許容しつつ、別 OID と update-ref の失敗は隠さないことを確認する。
func TestMutationDeleteSubmoduleCapsuleRefsChecksExpectedOID(t *testing.T) {
	_, repo, manager, _ := archiveFixture(t)
	moduleDir := filepath.Join(string(repo.CommonDir), "modules", "child")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, moduleDir, "init", "--bare")
	ref := "refs/wx/recovery/session/repository/submodule/child"
	objectPath := filepath.Join(t.TempDir(), "capsule-object")
	if err := os.WriteFile(objectPath, []byte("capsule"), 0o600); err != nil {
		t.Fatal(err)
	}
	oid := gitCommand(t, moduleDir, "--git-dir=.", "hash-object", "-w", objectPath)
	gitCommand(t, moduleDir, "update-ref", ref, oid)
	snapshot := state.SubmoduleSnapshot{Name: "child", CapsuleRef: ref, CapsuleOID: oid}
	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{snapshot}); err != nil {
		t.Fatalf("matching capsule ref was not deleted: %v", err)
	}
	if err := gitCommandExpectFailure(moduleDir, "show-ref", "--verify", "--hash", ref); err == nil {
		t.Fatal("capsule ref survived deletion")
	}
	// 既に消えた ref は GC/再実行との競合として成功扱いにする。
	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{snapshot}); err != nil {
		t.Fatalf("missing capsule ref was not idempotent: %v", err)
	}
	installGitFaultWithExitCode(t, " show-ref --verify --hash "+ref+" ", 1, 1)
	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{snapshot}); err != nil {
		t.Fatalf("exit code 1 for a missing capsule ref was not idempotent: %v", err)
	}

	gitCommand(t, moduleDir, "update-ref", ref, oid)
	wrong := snapshot
	wrong.CapsuleOID = strings.Repeat("b", 40)
	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{wrong}); err == nil || !strings.Contains(err.Error(), "unexpected OID") {
		t.Fatalf("unexpected capsule OID error=%v", err)
	}
	installGitFault(t, " update-ref -d ", 1)
	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{snapshot}); err == nil {
		t.Fatal("update-ref failure was swallowed")
	}
}

func gitCommandExpectFailure(dir string, args ...string) error {
	command := exec.Command("git", args...)
	command.Dir = dir
	return command.Run()
}
