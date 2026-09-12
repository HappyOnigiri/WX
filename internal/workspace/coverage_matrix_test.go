package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestPreparationHelpersRejectMissingDescriptorsAndUnsupportedTargets(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

	target := filepath.Join(root, testSlotRelPath, testRepositoryID)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarkerAt(owner, root, target, markerFor("s"), root); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarkerAt(owner, root, target, testRepositoryID); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarkerAt(owner, root, target, testRepositoryID); err != nil {
		t.Fatalf("idempotent marker removal: %v", err)
	}
	if err := removeOwnershipMarkerAt(nil, root, target, testRepositoryID); err == nil {
		t.Fatal("nil marker removal root was accepted")
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
}
