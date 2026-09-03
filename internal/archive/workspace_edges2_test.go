package archive

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// TestNormalizeWorkspaceExclusionsDeduplicatesAndSorts exercises the
// duplicate-skip branch in normalizeWorkspaceExclusions directly: repeated
// exclusion values must collapse to one entry, and the result must come back
// sorted regardless of input order.
func TestNormalizeWorkspaceExclusionsDeduplicatesAndSorts(t *testing.T) {
	out, err := normalizeWorkspaceExclusions([]string{"b", "a", "b", "a/c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "a/c", "b"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("normalizeWorkspaceExclusions=%v, want %v", out, want)
	}
}

// TestSnapshotWorkspacePropagatesRecoveryDirectoryCreationFailure exercises
// the MkdirAll failure branch in the pinned snapshot path: the deterministic
// "recovery" directory name is pre-occupied by a regular file, so the
// recovery-snapshots directory can never be created.
func TestSnapshotWorkspacePropagatesRecoveryDirectoryCreationFailure(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ownershipRoot, "recovery"), "blocks directory creation", 0o600)
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, "recovery-blocked", nil, time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "create workspace recovery directory") {
		t.Fatalf("snapshot succeeded despite a blocked recovery directory: %v", err)
	}
}

// TestPruneWorkspaceRootDescendsThroughNonExcludedAncestor exercises the
// recursive branch of pruneWorkspaceRoot where an ancestor directory itself
// is not excluded but contains an excluded descendant: pruning must descend
// into it, preserving the excluded child while still removing its
// non-excluded sibling.
func TestPruneWorkspaceRootDescendsThroughNonExcludedAncestor(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.MkdirAll(filepath.Join(bundleRoot, "outer", "kept"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "outer", "kept", "file.txt"), "keep\n", 0o600)
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "outer", "extra.txt"), "remove me\n", 0o600)
	empty := writeWorkspaceArchive(t, ownershipRoot, "descend", nil)
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, empty, []string{"outer/kept"}); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bundleRoot, "outer", "kept", "file.txt")); err != nil {
		t.Fatalf("excluded nested path was removed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(bundleRoot, "outer", "extra.txt")); !os.IsNotExist(err) {
		t.Fatalf("non-excluded sibling under a kept ancestor survived pruning: %v", err)
	}
}
