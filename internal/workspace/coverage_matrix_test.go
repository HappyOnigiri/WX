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
	preparer.RootPath = root
	preparer.OwnedRoot = nil
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
	if _, _, _, err := plainPreparer.openOwnedRoot(plainRoot, filepath.Join(plainRoot, "target")); err == nil {
		t.Fatal("openOwnedRoot accepted a preparer without a pinned root descriptor")
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
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, "s", root); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarkerAt(owner, root, target); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarkerAt(owner, root, target); err != nil {
		t.Fatalf("idempotent marker removal: %v", err)
	}
	if err := removeOwnershipMarkerAt(nil, root, target); err == nil {
		t.Fatal("nil marker removal root was accepted")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedPrepareCommandRunsInsideValidatedWorktree(t *testing.T) {
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
	plain := &Preparer{Git: preparer.Git, Config: cfg, OwnedRoot: owner, RootPath: root}
	if err := plain.runPrepareWithIdentity(ctx, repo, target, ""); err != nil {
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
