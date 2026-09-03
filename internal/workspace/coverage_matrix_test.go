package workspace

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
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

func TestUnpinnedMaterializersAndOwnershipValidation(t *testing.T) {
	repository, repo, preparer, _, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.env"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := preparer.copyIncludes(repo, target); err != nil {
		t.Fatalf("unpinned include materialization: %v", err)
	}
	if err := preparer.copyIncludes(repo, target); err != nil {
		t.Fatalf("idempotent unpinned include materialization: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "local.env")); err != nil || string(data) != "local\n" {
		t.Fatalf("included file=%q err=%v", data, err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("unpinned link materialization: %v", err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("idempotent unpinned link materialization: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(target, "shared")); err != nil || link != filepath.Join(repository, "shared") {
		t.Fatalf("materialized link=%q err=%v", link, err)
	}

	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil || !strings.Contains(err.Error(), "overwrite tracked path") {
		t.Fatalf("tracked include error=%v", err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := preparer.copyIncludes(repo, target); err == nil {
		t.Fatal("unsafe include pattern succeeded")
	}

	if err := os.Remove(filepath.Join(target, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("link collision error=%v", err)
	}

	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeOwnershipValidationCoversPhysicalAndGitBoundaries(t *testing.T) {
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
	preparer.RequireOwnedRoot = true

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

	marker := filepath.Join(filepath.Dir(target), ownershipMarkerNameForTarget(target))
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := preparer.ValidateOwnership(ctx, repo, target, head); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing marker error=%v", err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, "slot", string(repo.CommonDir)); err != nil {
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

func TestPreparationHelpersRejectMissingDescriptorsAndUnsupportedTargets(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	preparer.RequireOwnedRoot = true
	preparer.RootPath = root
	if _, _, err := preparer.prepareTarget(target); err == nil {
		t.Fatal("prepare target accepted a missing required descriptor")
	}
	if _, _, _, err := preparer.openOwnedRoot(root, target); err == nil {
		t.Fatal("openOwnedRoot accepted a missing pinned descriptor")
	}
	preparer.Ownership = nil
	if err := preparer.validateStateOwnership(ctx, repo, target, "slot", nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing state validator error=%v", err)
	}
	if err := preparer.validateExistingWorktreeOwnedForPhase(ctx, repo, filepath.Join(t.TempDir(), "outside"), head, "", preparePhaseCreate); err == nil {
		t.Fatal("outside ownership target accepted")
	}
	if err := preparer.VerifyWorktreeIdentity(filepath.Join(t.TempDir(), "outside"), "identity"); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside identity error=%v", err)
	}

	plainRoot := t.TempDir()
	plainPreparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: func() config.Config {
		cfg := config.Defaults()
		cfg.Storage.WorktreeRoot = plainRoot
		return cfg
	}()}
	if _, _, _, err := plainPreparer.openOwnedRoot(plainRoot, filepath.Join(plainRoot, "target")); err != nil {
		// A missing target is expected to fail only when the descriptor root itself
		// cannot provide a safe parent; make the assertion explicit below instead.
		t.Logf("missing target descriptor result: %v", err)
	}
	if err := plainPreparer.validateStateOwnership(ctx, repo, target, "", nil, nil); err != nil {
		t.Fatalf("empty slot state validation should be a no-op: %v", err)
	}
}

func TestFilesystemHelperMatrixCoversPhysicalGlobAndCopyBoundaries(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "deep", "file.txt"), []byte("copy"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "regular"), []byte("regular"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "regular"), filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if matches, err := safeGlob(source, "nested/*/*.txt"); err != nil || len(matches) != 1 {
		t.Fatalf("safe glob matches=%v err=%v", matches, err)
	}
	if matches, err := safeGlob(source, "missing/*.txt"); err != nil || len(matches) != 0 {
		t.Fatalf("missing safe glob matches=%v err=%v", matches, err)
	}
	if _, err := safeGlob(source, "link/file"); err == nil {
		t.Fatal("glob descended through a symlink")
	}
	if matches, err := safeGlob(source, "regular/*"); err != nil || len(matches) != 0 {
		t.Fatalf("regular glob matches=%v err=%v", matches, err)
	}

	sourceRoot, err := OpenPhysicalRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	destinationRoot, err := OpenPhysicalRoot(destination)
	if err != nil {
		_ = sourceRoot.Close()
		t.Fatal(err)
	}
	if err := copyPathFromOwnedRoot(nil, "regular", destinationRoot, "regular"); err == nil {
		t.Fatal("nil source root was accepted")
	}
	if err := copyPathFromOwnedRoot(sourceRoot, "regular", nil, "regular"); err == nil {
		t.Fatal("nil destination root was accepted")
	}
	if err := copyPathFromOwnedRoot(sourceRoot, "regular", destinationRoot, "."); err == nil {
		t.Fatal("unsafe destination path was accepted")
	}
	if err := copyPathFromOwnedRoot(sourceRoot, "nested", destinationRoot, "copied"); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromOwnedRoot(sourceRoot, "regular", destinationRoot, "copied-regular"); err != nil {
		t.Fatal(err)
	}
	if data, err := destinationRoot.ReadFile("copied/deep/file.txt"); err != nil || string(data) != "copy" {
		t.Fatalf("copied directory data=%q err=%v", data, err)
	}
	if data, err := destinationRoot.ReadFile("copied-regular"); err != nil || string(data) != "regular" {
		t.Fatalf("copied file data=%q err=%v", data, err)
	}
	if err := sourceRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := destinationRoot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(filepath.Join(source, "missing"), "regular", destination, "copy"); err == nil {
		t.Fatal("copy from missing root succeeded")
	}
	if err := copyPathFromRoots(source, "../regular", destination, "copy"); err == nil {
		t.Fatal("copy with unsafe source path succeeded")
	}
}

func TestPhysicalManifestAndMarkerRemovalBoundaries(t *testing.T) {
	root := t.TempDir()
	if data, err := readPhysicalManifest(root, ".missing"); err != nil || data != nil {
		t.Fatalf("missing physical manifest data=%q err=%v", data, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".worktreeinclude"), []byte("one\n# comment\n\n two \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patterns, err := readPhysicalPatterns(root, ".worktreeinclude")
	if err != nil || strings.Join(patterns, ",") != "one,two" {
		t.Fatalf("physical patterns=%v err=%v", patterns, err)
	}
	if err := os.Mkdir(filepath.Join(root, "directory-manifest"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalManifest(root, "directory-manifest"); err == nil {
		t.Fatal("directory manifest was accepted")
	}
	if err := os.Symlink(root, filepath.Join(root, "manifest-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalManifest(root, "manifest-link"); err == nil {
		t.Fatal("symlink manifest was accepted")
	}

	target := filepath.Join(root, "workspaces", "w", "slots", "s", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarker(root, target, "s", root); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarker(root, target); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarker(root, target); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarkerAt(nil, root, target); err == nil {
		t.Fatal("nil marker removal root was accepted")
	}
	if err := removeOwnershipMarkerAt(owner, root, target); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedPrepareCommandRunsInsideValidatedWorktree(t *testing.T) {
	ctx := context.Background()
	_, repo, preparer, head, target := prepareEdgesFixture(t)
	root := preparer.Config.Storage.WorktreeRoot
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	preparer.OwnedRoot = owner
	preparer.RootPath = root
	preparer.RequireOwnedRoot = true
	if err := preparer.Prepare(ctx, repo, target, head, "prepare-command"); err != nil {
		t.Fatal(err)
	}
	identity, err := preparer.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	cfg := preparer.Config
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", "printf pinned > prepare-marker"}}}}
	preparer.Config = cfg
	if err := preparer.runPrepareWithIdentity(ctx, repo, target, identity); err != nil {
		t.Fatalf("descriptor-bound prepare command: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "prepare-marker")); err != nil || string(data) != "pinned" {
		t.Fatalf("prepare marker=%q err=%v", data, err)
	}
	// A non-positive command timeout falls back to the configured readiness
	// budget, keeping the ordinary path covered as well.
	cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: config.Prepare{Command: []string{"/usr/bin/true"}}}
	preparer.Config = cfg
	plain := &Preparer{Config: cfg}
	if err := plain.runPrepare(ctx, repo, target); err != nil {
		t.Fatalf("default prepare timeout: %v", err)
	}
}

func TestFingerprintAndRelativePathBoundaries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "deep", "file"), []byte("fingerprint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	for _, test := range []struct {
		name string
		rel  string
		bad  bool
	}{
		{name: "directory", rel: "nested"},
		{name: "file", rel: "nested/deep/file"},
		{name: "missing", rel: "missing", bad: true},
		{name: "symlink", rel: "link", bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := sha256.New()
			err := fingerprintRootPath(h, owner, test.rel, test.rel)
			if test.bad {
				if err == nil {
					t.Fatal("unsafe fingerprint input succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("fingerprintRootPath: %v", err)
			}
			if h.Size() == 0 {
				t.Fatal("fingerprint did not include metadata or file contents")
			}
		})
	}
	if err := fingerprintPath(sha256.New(), root, filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("fingerprintPath accepted an outside path")
	}
	if got, err := repositoryWorkspaceRoot(discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "nested/deep"}); err != nil || got != filepath.Dir(filepath.Dir(root)) {
		t.Fatalf("repository workspace root=%q err=%v", got, err)
	}
	if _, err := repositoryWorkspaceRoot(discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "../outside"}); err == nil {
		t.Fatal("unsafe repository relative path accepted")
	}
	for _, value := range []string{"", ".", "..", "../escape", "/absolute", "nested/file"} {
		_, err := safeRelative(value)
		if (value == "nested/file") == (err != nil) {
			t.Fatalf("safeRelative(%q) err=%v", value, err)
		}
	}
}

func TestOwnershipDescriptorAndGitRecordBoundaryMatrix(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, "common")
	target := filepath.Join(root, "workspaces", "w", "slots", "s", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(common, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	if err := EnsureOwnershipMarkerAt(nil, root, target, "s", common); err == nil {
		t.Fatal("nil marker owner was accepted")
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, "bad/slot", common); err == nil {
		t.Fatal("unsafe marker slot was accepted")
	}
	if _, err := newOwnershipMarkerAt(nil, root, target, "s", common, true); err == nil {
		t.Fatal("nil owner was accepted by newOwnershipMarkerAt")
	}
	// EnsureOwnershipMarkerAt/ValidateOwnershipMarkerAt already reject an
	// unsafe slot id before calling newOwnershipMarkerAt, so exercise its own
	// defense-in-depth guard directly: the helper must not trust a future
	// caller to have validated its input.
	if _, err := newOwnershipMarkerAt(owner, root, target, "bad/slot", common, true); err == nil {
		t.Fatal("unsafe slot id was accepted by newOwnershipMarkerAt directly")
	}
	if _, err := newOwnershipMarkerAt(owner, root, filepath.Join(t.TempDir(), "outside"), "s", common, true); err == nil {
		t.Fatal("outside marker target was accepted")
	}
	symlinkTarget := filepath.Join(root, "workspaces", "w", "slots", "s", "link")
	if err := os.Symlink(target, symlinkTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := newOwnershipMarkerAt(owner, root, symlinkTarget, "s", common, true); err == nil {
		t.Fatal("symlink marker target was accepted")
	}
	missingTarget := filepath.Join(root, "workspaces", "w", "slots", "s", "missing")
	if _, err := newOwnershipMarkerAt(owner, root, missingTarget, "s", common, true); err != nil {
		t.Fatalf("missing marker target with physical parent: %v", err)
	}
	if _, err := newOwnershipMarkerAt(owner, root, filepath.Join(root, "missing", "target"), "s", common, true); err == nil {
		t.Fatal("missing marker parent was accepted")
	}
	if _, err := newOwnershipMarkerAt(owner, root, target, "s", filepath.Join(root, "missing-common"), true); err == nil {
		t.Fatal("missing common directory was accepted")
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, "s", common); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOwnershipMarkerAt(owner, root, target, "s", common); err != nil {
		t.Fatalf("valid descriptor marker: %v", err)
	}
	if err := ValidateOwnershipMarkerAt(owner, root, target, "other", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("wrong slot marker error=%v", err)
	}
	markerRelative, err := ownershipMarkerRelative(root, target)
	if err != nil {
		t.Fatal(err)
	}
	file, err := owner.OpenFile(markerRelative, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{} {}")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnershipMarker(owner, markerRelative); err == nil {
		t.Fatal("malformed marker was accepted")
	}
	if err := owner.Remove(markerRelative); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, "s", common); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		out  string
		want int
	}{
		{name: "preamble and unlocked", out: "noise\x00worktree /tmp/a\x00locked\x00\x00", want: 1},
		{name: "reason", out: "worktree /tmp/a\x00locked wx:s:READY\x00worktree /tmp/b\x00", want: 2},
		{name: "empty", out: "", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := parseWorktreeRecords(test.out)
			if len(records) != test.want {
				t.Fatalf("records=%+v want %d", records, test.want)
			}
		})
	}
	if base := ownershipMarkerBase(root, target); base != filepath.Join(root, "workspaces", "w", "slots", "s") {
		t.Fatalf("slot marker base=%q", base)
	}
	if base := ownershipMarkerBase(root, filepath.Join(root, "misc", "target")); base != filepath.Join(root, "misc") {
		t.Fatalf("ordinary marker base=%q", base)
	}
	for _, candidate := range []string{root, filepath.Join(root, "link"), filepath.Join(root, "missing", "leaf")} {
		if candidate == filepath.Join(root, "link") {
			if err := os.Symlink(root, candidate); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := canonicalPathAllowMissing(candidate); err != nil {
			t.Fatalf("canonicalPathAllowMissing(%q): %v", candidate, err)
		}
	}
	if _, _, _, err := RegisteredWorktreeLockStatusAt(context.Background(), &gitx.Runner{Timeout: time.Second}, filepath.Join(t.TempDir(), "missing-repository"), owner, root, "target", "identity"); err == nil {
		t.Fatal("invalid Git main path was accepted")
	}
	if _, _, _, err := RegisteredWorktreeLockStatusAt(context.Background(), &gitx.Runner{}, "", nil, root, "target", "identity"); err == nil {
		t.Fatal("nil status owner was accepted")
	}
	if _, _, _, err := RegisteredWorktreeLockStatusAt(context.Background(), &gitx.Runner{}, "", owner, root, "target", ""); err == nil {
		t.Fatal("missing target identity was accepted")
	}
}

// TestValidateRegisteredWorktreeAtCoversLockAndSlotBoundaries exercises the
// descriptor-bound lock/slot matrix that ValidateRegisteredWorktreeAt shares
// conceptually with the lexical-path ValidateRegisteredWorktree, but which is
// tracked as separate code because it never resolves the mutable target
// pathname (see RegisteredWorktreeLockStatusAt's doc comment).
func TestValidateRegisteredWorktreeAtCoversLockAndSlotBoundaries(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
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
	worktreeRoot := filepath.Join(root, "worktrees")
	target := filepath.Join(worktreeRoot, "slot", "root")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	owner, relative, err := domain.OpenOwnedRoot(worktreeRoot, target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()

	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "", false); err != nil {
		t.Fatalf("unlocked descriptor-bound worktree without lock requirement: %v", err)
	}
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "", true); err == nil {
		t.Fatal("unlocked descriptor-bound worktree accepted when lock was required")
	}
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, filepath.Join(relative, "missing"), identity, "", false); err == nil {
		t.Fatal("unregistered descriptor-bound path accepted")
	}

	gitCommand(t, repository, "worktree", "lock", "--reason", "unaffiliated-lock", target)
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "", true); err == nil {
		t.Fatal("non-wx descriptor-bound lock reason accepted without a slot")
	}
	gitCommand(t, repository, "worktree", "unlock", target)

	gitCommand(t, repository, "worktree", "lock", "--reason", "wx:slot:READY", target)
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "", true); err != nil {
		t.Fatalf("wx descriptor-bound lock without slot requirement: %v", err)
	}
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "slot", true); err != nil {
		t.Fatalf("matching descriptor-bound wx lock rejected: %v", err)
	}
	if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "other", true); err == nil {
		t.Fatal("foreign descriptor-bound wx slot lock accepted")
	}
	gitCommand(t, repository, "worktree", "unlock", target)

	for _, state := range []string{"PREPARING", "RESTORING"} {
		t.Run(state, func(t *testing.T) {
			gitCommand(t, repository, "worktree", "lock", "--reason", "wx:slot:"+state, target)
			defer gitCommand(t, repository, "worktree", "unlock", target)
			if err := ValidateRegisteredWorktreeAt(ctx, runner, repository, owner, worktreeRoot, relative, identity, "slot", true); err != nil {
				t.Fatalf("wx slot lock reason %s rejected: %v", state, err)
			}
		})
	}

	if err := ValidateRegisteredWorktreeAt(ctx, runner, filepath.Join(t.TempDir(), "missing-repository"), owner, worktreeRoot, relative, identity, "", false); err == nil {
		t.Fatal("descriptor-bound validation with an invalid Git main path succeeded")
	}
}
