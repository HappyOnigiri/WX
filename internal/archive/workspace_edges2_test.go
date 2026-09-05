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

// TestNormalizeWorkspaceExclusionsDeduplicatesAndSorts は normalizeWorkspaceExclusions の重複除外を検証する。
// 重複値を一つにまとめ、入力順にかかわらず結果を sort する。
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

// TestSnapshotWorkspacePropagatesRecoveryDirectoryCreationFailure は pin 済み snapshot path の MkdirAll 失敗を検証する。
// 決定的な `_recovery` directory 名を通常 file で占有し、recovery snapshots directory を作れなくする。
func TestSnapshotWorkspacePropagatesRecoveryDirectoryCreationFailure(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ownershipRoot, recoveryNamespace), "blocks directory creation", 0o600)
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "recovery-blocked", nil, time.Now().Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "create workspace recovery directory") {
		t.Fatalf("snapshot succeeded despite a blocked recovery directory: %v", err)
	}
}

// TestPruneWorkspaceRootDescendsThroughNonExcludedAncestor は、除外されない ancestor に除外対象の子孫がある場合を検証する。
// prune はその ancestor を再帰的に降り、除外対象の子を残しつつ除外されない sibling を消す。
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
