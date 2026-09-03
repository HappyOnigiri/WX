package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// installGitFault makes the occurrence-th invocation of git whose arguments
// contain pattern fail, and lets every other invocation run the real git
// binary. It mirrors the identical helper in internal/archive's test suite so
// specific revalidation/cleanup Git calls can be targeted deterministically.
func installGitFault(t *testing.T, pattern string, occurrence int) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "matches")
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
if case " $* " in *"$WX_FAULT_PATTERN"*) true;; *) false;; esac; then
  count=0
  if [ -f "$WX_FAULT_MARKER" ]; then read -r count < "$WX_FAULT_MARKER"; fi
  count=$((count + 1))
  printf '%s\n' "$count" > "$WX_FAULT_MARKER"
  if [ "$count" -eq "$WX_FAULT_OCCURRENCE" ]; then
    printf 'injected git failure\n' >&2
    exit 2
  fi
fi
exec "$WX_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REAL_GIT", realGit)
	t.Setenv("WX_FAULT_PATTERN", pattern)
	t.Setenv("WX_FAULT_MARKER", marker)
	t.Setenv("WX_FAULT_OCCURRENCE", strconv.Itoa(occurrence))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRestorePreparationResumeAndFinishLifecycle(t *testing.T) {
	repository, repo, preparer, head, target := prepareEdgesFixture(t)
	ctx := context.Background()
	if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
		t.Fatalf("prepare for restore: %v", err)
	}
	if reason, locked, found, err := RegisteredWorktreeLockStatus(ctx, preparer.Git, string(repo.MainPath), target); err != nil || !found || !locked || reason != "wx:slot:RESTORING" {
		t.Fatalf("restore lock reason=%q locked=%v found=%v err=%v", reason, locked, found, err)
	}
	if err := preparer.ValidateRestoringSlotWorktreeOwnership(ctx, repo, target, head, "slot"); err != nil {
		t.Fatalf("restoring replay ownership: %v", err)
	}
	if err := preparer.ValidateSlotWorktreeOwnership(ctx, repo, target, head, ""); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("empty replay slot ID error=%v", err)
	}
	if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", ""); err != nil {
		t.Fatalf("resume prepare: %v", err)
	}
	if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", ""); err != nil {
		t.Fatalf("finish restore: %v", err)
	}
	if err := preparer.ValidateOwnership(ctx, repo, target, head); err != nil {
		t.Fatalf("finished restore ownership: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repository, ".git")); err != nil {
		t.Fatal(err)
	}
}

func TestResumeAndFinishRestoreFailClosedAtBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("resume validation", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		preparer.Ownership = edgeOwnershipValidator{err: errors.New("ownership changed")}
		if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", ""); !errors.Is(err, state.ErrOwnership) {
			t.Fatalf("resume validation error=%v", err)
		}
	})
	t.Run("resume command", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		cfg := preparer.Config
		cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"/bin/false"}, Timeout: config.Duration{Duration: time.Second}}}
		preparer.Config = cfg
		if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", ""); err == nil {
			t.Fatal("failed resume command succeeded")
		}
	})
	t.Run("resume post-validation", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		preparer.Ownership = &edgeCountingOwnershipValidator{failAt: 2}
		if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", ""); !errors.Is(err, state.ErrOwnership) {
			t.Fatalf("resume post-validation error=%v", err)
		}
	})
	t.Run("finish initial proof", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		preparer.Ownership = edgeOwnershipValidator{err: errors.New("ownership changed")}
		if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", ""); !errors.Is(err, state.ErrOwnership) {
			t.Fatalf("finish initial proof error=%v", err)
		}
	})
	t.Run("finish unlock", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		badRepo := repo
		badRepo.MainPath = domain.CanonicalPath(filepath.Join(t.TempDir(), "missing-main"))
		if err := preparer.FinishRestoreWithIdentity(ctx, badRepo, target, head, "slot", ""); err == nil {
			t.Fatal("finish restore with missing main repository succeeded")
		}
	})
	t.Run("finish final validation", func(t *testing.T) {
		_, repo, preparer, head, target := prepareEdgesFixture(t)
		if err := preparer.PrepareForRestore(ctx, repo, target, head, "slot"); err != nil {
			t.Fatal(err)
		}
		preparer.Ownership = &edgeCountingOwnershipValidator{failAt: 2}
		if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", ""); !errors.Is(err, state.ErrOwnership) {
			t.Fatalf("finish final validation error=%v", err)
		}
	})
}

type edgeOwnershipValidator struct{ err error }

func (v edgeOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	return state.WorktreeOwnership{}, v.err
}

type edgeCountingOwnershipValidator struct{ calls, failAt int }

func (v *edgeCountingOwnershipValidator) ValidateWorktreeOwnership(context.Context, state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	v.calls++
	if v.calls == v.failAt {
		return state.WorktreeOwnership{}, errors.New("ownership changed")
	}
	return state.WorktreeOwnership{}, nil
}

func prepareEdgesFixture(t *testing.T) (string, discovery.Repository, *Preparer, string, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	common := gitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common)}
	worktreeRoot := filepath.Join(root, "worktrees")
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	preparer := &Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, Ownership: allowOwnershipValidator{}, OwnedRoot: owner, RootPath: worktreeRoot}
	target := filepath.Join(worktreeRoot, "slots", "slot", "root")
	return repository, repo, preparer, head, target
}

func TestPinnedPreparerLifecycleAndDescriptorBoundCommands(t *testing.T) {
	ctx := context.Background()
	for _, restore := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "restore"}[restore], func(t *testing.T) {
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

			var prepareErr error
			if restore {
				prepareErr = preparer.PrepareForRestore(ctx, repo, target, head, "slot")
			} else {
				prepareErr = preparer.Prepare(ctx, repo, target, head, "slot")
			}
			if prepareErr != nil {
				t.Fatalf("descriptor-bound preparation: %v", prepareErr)
			}
			identity, err := preparer.WorktreeIdentity(target)
			if err != nil || identity == "" {
				t.Fatalf("worktree identity=%q err=%v", identity, err)
			}
			if err := preparer.VerifyWorktreeIdentity(target, identity); err != nil {
				t.Fatalf("identity verification: %v", err)
			}
			if err := preparer.VerifyWorktreeIdentity(target, "different"); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("identity mismatch error=%v", err)
			}
			if _, err := preparer.RunGitInWorktree(ctx, target, identity, nil, nil, "rev-parse", "HEAD"); err != nil {
				t.Fatalf("descriptor-bound git command: %v", err)
			}
			if err := preparer.ValidateOwnership(ctx, repo, target, head); err != nil {
				t.Fatalf("descriptor-bound ownership validation: %v", err)
			}
			if restore {
				if err := preparer.PrepareResumeWithIdentity(ctx, repo, target, head, "slot", identity); err != nil {
					t.Fatalf("descriptor-bound resume prepare: %v", err)
				}
				if err := preparer.FinishRestoreWithIdentity(ctx, repo, target, head, "slot", identity); err != nil {
					t.Fatalf("descriptor-bound restore finish: %v", err)
				}
				if _, err := preparer.runWorktreeAdmin(ctx, repo, owner, filepath.Join("slots", "slot", "root"), target, "unlock"); err != nil {
					t.Fatalf("unlock restored worktree before removal: %v", err)
				}
			} else {
				if _, err := preparer.runWorktreeAdmin(ctx, repo, owner, filepath.Join("slots", "slot", "root"), target, "unlock"); err != nil {
					t.Fatalf("descriptor-bound admin wrapper: %v", err)
				}
			}
			if err := preparer.RemoveWorktreeAt(ctx, repo, root, target, identity); err != nil {
				t.Fatalf("descriptor-bound worktree removal: %v", err)
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("removed worktree still exists: %v", err)
			}
			if err := preparer.RemoveWorktreeAt(ctx, repo, root, filepath.Join(t.TempDir(), "outside"), identity); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("outside descriptor-bound removal error=%v", err)
			}
		})
	}
}

func TestPreparerDescriptorOperationsRejectInvalidRootsAndIdentities(t *testing.T) {
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

	if _, _, err := preparer.prepareTarget(filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("prepare target outside root was accepted")
	}
	preparer.OwnedRoot = nil
	if _, _, err := preparer.prepareTarget(target); err == nil {
		t.Fatal("missing required root descriptor was accepted")
	}
	preparer.OwnedRoot = owner
	if _, _, _, err := preparer.openOwnedRoot(root, target); err != nil {
		t.Fatalf("pinned root open: %v", err)
	}

	if _, err := preparer.WorktreeIdentity(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing worktree identity succeeded")
	}
	if err := preparer.VerifyWorktreeIdentity(target, ""); err != nil {
		t.Fatalf("empty identity compatibility path: %v", err)
	}
	if _, err := preparer.RunGitInWorktree(ctx, filepath.Join(t.TempDir(), "missing"), "identity", nil, nil, "status"); err == nil {
		t.Fatalf("missing descriptor-bound command error=%v", err)
	}
	if err := preparer.RemoveWorktreeAt(ctx, repo, root, filepath.Join(t.TempDir(), "outside"), "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside removal error=%v", err)
	}
	if err := preparer.RemoveWorktreeAt(ctx, repo, root, target, "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("unavailable removal target error=%v", err)
	}

	closed, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.addWorktreeWithIdentity(ctx, repo, closed, target, filepath.Join("slot", "root"), head); err == nil {
		t.Fatal("closed descriptor worktree add succeeded")
	}
	if _, err := preparer.runWorktreeAdminOwned(ctx, repo, closed, filepath.Join("slot", "root"), target, "", "unlock"); err == nil {
		t.Fatal("closed descriptor worktree admin succeeded")
	}

	badConfig := preparer.Config
	badConfig.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/true"}, Timeout: config.Duration{Duration: time.Second}}}}
	preparer.Config = badConfig
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.runPrepareWithIdentity(ctx, repo, target, "mismatched"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched prepare identity error=%v", err)
	}
}

func TestPreparationHelpersRejectInvalidPhaseAndOwnershipInputs(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	if slots, repos := preparationOwnershipStates(preparePhaseRestore); strings.Join(slots, ",") != "RESTORING" || strings.Join(repos, ",") != "RESTORING,RESTORE_RUNNING" {
		t.Fatalf("restore states=%v/%v", slots, repos)
	}
	if slots, repos := preparationOwnershipStates(preparePhase("unknown")); strings.Join(slots, ",") != "PREPARING" || strings.Join(repos, ",") != "PREPARING,PREPARE_RUNNING" {
		t.Fatalf("create states=%v/%v", slots, repos)
	}
	if err := preparer.validateStateOwnership(context.Background(), repo, target, "", nil, nil); err != nil {
		t.Fatalf("empty state ownership should be skipped: %v", err)
	}
	preparer.Ownership = nil
	if err := preparer.validateStateOwnership(context.Background(), repo, target, "slot", nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing state validator error=%v", err)
	}
	preparer.Ownership = edgeOwnershipValidator{err: errors.New("validator fault")}
	if err := preparer.validateStateOwnership(context.Background(), repo, target, "slot", nil, nil); !strings.Contains(err.Error(), "validator fault") {
		t.Fatalf("validator error=%v", err)
	}
	if err := preparer.ValidateRestoringOwnership(context.Background(), repo, target, head, "slot"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("restoring ownership without worktree error=%v", err)
	}
}

func TestPrepareReusesAnEmptyAllocationShellAndValidatesReadyState(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	root, normalizedTarget, err := preparer.prepareTarget(target)
	if err != nil || normalizedTarget != filepath.Clean(target) {
		t.Fatalf("empty allocation target=%q root=%q err=%v", normalizedTarget, root, err)
	}
	ownedRoot, relative, err := domain.OpenOwnedRoot(root, normalizedTarget)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := preparer.existingTargetState(context.Background(), repo, normalizedTarget, head, "slot", preparePhaseCreate, root, ownedRoot, relative)
	if err := ownedRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil || empty {
		t.Fatalf("empty allocation state=%v err=%v", empty, err)
	}
	if err := preparer.Prepare(context.Background(), repo, normalizedTarget, head, "slot"); err != nil {
		t.Fatalf("prepare empty allocation: %v", err)
	}
	if err := preparer.ValidateReady(context.Background(), repo, normalizedTarget, head); err != nil {
		t.Fatalf("validate prepared READY allocation: %v", err)
	}
}

func TestPrepareRejectsUnsafeConfiguredWorktreeRoot(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	rootFile := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(rootFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := preparer.Config
	config.Storage.WorktreeRoot = rootFile
	preparer.Config = config
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
		t.Fatal("prepare beneath a regular configured root succeeded")
	}
}

func TestPrepareRejectsWorktreeAlteredByThePrepareCommand(t *testing.T) {
	for _, test := range []struct {
		name    string
		script  string
		wantErr string
	}{
		{name: "checks out a branch", script: "git checkout -b stray", wantErr: "not detached"},
		{name: "moves HEAD", script: "git commit --allow-empty -m stray", wantErr: "differs from requested OID"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := preparer.Config
			cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", test.script}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
			preparer.Config = cfg
			err := preparer.Prepare(context.Background(), repo, target, head, "slot")
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("prepare command tampering error=%v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestPrepareLeavesWorktreeForQuarantineWhenCleanupGitFails(t *testing.T) {
	for _, test := range []struct {
		name       string
		pattern    string
		wantLocked bool
	}{
		{name: "unlock fails", pattern: "worktree unlock", wantLocked: true},
		{name: "remove fails", pattern: "worktree remove --force", wantLocked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, repo, preparer, head, target := prepareEdgesFixture(t)
			root := preparer.Config.Storage.WorktreeRoot
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := preparer.Config
			cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "exit 1"}, Timeout: config.Duration{Duration: 5 * time.Second}}}}
			preparer.Config = cfg
			// The prepare command is the only failure injected by the test
			// scenario; "worktree lock" must still succeed so cleanup is
			// armed (ownedAfterLock=true). The cleanup path's own Git call
			// is then made to fail so the deferred cleanup must leave the
			// worktree registered for later quarantine/reconcile instead of
			// silently discarding it.
			installGitFault(t, test.pattern, 1)
			if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
				t.Fatal("prepare succeeded despite a failing prepare command")
			}
			_, locked, found, err := RegisteredWorktreeLockStatus(context.Background(), preparer.Git, string(repo.MainPath), target)
			if err != nil || !found {
				t.Fatalf("worktree registration was removed despite a cleanup Git failure: found=%v err=%v", found, err)
			}
			if locked != test.wantLocked {
				t.Fatalf("worktree locked=%v, want %v", locked, test.wantLocked)
			}
		})
	}
}

func TestPrepareOnAnExistingWorktreePropagatesInitialUnlockFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err != nil {
		t.Fatalf("initial prepare: %v", err)
	}
	// The first Prepare call only ever locks a newly created worktree; the
	// re-prepare below is the first call in this test to unlock an existing
	// one, so occurrence 1 targets exactly that call.
	installGitFault(t, "worktree unlock", 1)
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil || !strings.Contains(err.Error(), "unlock existing wx worktree") {
		t.Fatalf("re-prepare error=%v", err)
	}
}

func TestPrepareOnANewWorktreePropagatesLockFailure(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	installGitFault(t, "worktree lock", 1)
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil {
		t.Fatal("prepare succeeded despite a failing initial lock")
	}
}

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
	// git fails before writing anything into the reserved leaf or registering
	// the worktree, so addWorktreeWithIdentity must recognize the reserved
	// namespace as clean and return the original Git error rather than an
	// uncertain-outcome ownership error.
	installGitFault(t, "worktree add --detach", 1)
	if err := preparer.Prepare(context.Background(), repo, target, head, "slot"); err == nil || errors.Is(err, state.ErrOwnership) {
		t.Fatalf("clean failed add error=%v, want a plain Git error", err)
	}
	if _, _, found, err := RegisteredWorktreeLockStatusAt(context.Background(), preparer.Git, string(repo.MainPath), owner, root, "slots/slot/root", "irrelevant"); err != nil || found {
		t.Fatalf("failed add left a Git registration: found=%v err=%v", found, err)
	}
}

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
	// Simulate Git being interrupted after it started writing into the
	// reserved worktree namespace but before it reported success: the
	// reserved leaf is left non-empty, which addWorktreeWithIdentity must
	// treat as an uncertain outcome (ownership quarantine) rather than a
	// plain, retryable Git failure.
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

func TestPrepareRejectsStateOwnershipBeforeWritingMarker(t *testing.T) {
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	preparer.Ownership = edgeOwnershipValidator{err: errors.New("ownership changed")}
	err := preparer.prepareLocked(context.Background(), repo, target, head, "slot", preparePhaseCreate, root)
	if err == nil || !strings.Contains(err.Error(), "before marker") || !strings.Contains(err.Error(), "ownership changed") {
		t.Fatalf("state ownership failure=%v", err)
	}
}
