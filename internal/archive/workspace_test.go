package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestWorkspaceSnapshotRestorePreservesOnlyOwnedRootState(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "workspaces", "workspace", "slots", "session", "root")
	if err := os.MkdirAll(filepath.Join(bundleRoot, "repo"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "AGENTS.md"), "archived rules\n", 0o600)
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "notes", "todo.txt"), "historical\n", 0o640)
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "repo", "tracked.txt"), "repository state\n", 0o600)
	shared := filepath.Join(ownershipRoot, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(bundleRoot, "audit")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("notes/todo.txt", filepath.Join(bundleRoot, "shortcut")); err != nil {
		t.Fatal(err)
	}

	expiry := time.Now().Add(time.Hour)
	snapshot, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "session", []string{"repo", "audit"}, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}

	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "AGENTS.md"), "current source rules\n", 0o600)
	if err := os.RemoveAll(filepath.Join(bundleRoot, "notes")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(bundleRoot, "shortcut")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "current-only.txt"), "remove me\n", 0o600)
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "repo", "tracked.txt"), "restored separately\n", 0o600)

	if err := RestoreWorkspace(context.Background(), bundleRoot, ownershipRoot, ownershipRoot, snapshot, []string{"repo", "audit"}); err != nil {
		t.Fatal(err)
	}
	assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "AGENTS.md"), "archived rules\n")
	assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "notes", "todo.txt"), "historical\n")
	assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "repo", "tracked.txt"), "restored separately\n")
	if _, err := os.Lstat(filepath.Join(bundleRoot, "audit")); err != nil {
		t.Fatalf("shared link was removed: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(bundleRoot, "shortcut")); err != nil || target != "notes/todo.txt" {
		t.Fatalf("restored symlink target=%q err=%v", target, err)
	}
	if _, err := os.Lstat(filepath.Join(bundleRoot, "current-only.txt")); !os.IsNotExist(err) {
		t.Fatalf("materialized-only root file survived restore: %v", err)
	}
	if info, err := os.Stat(filepath.Join(bundleRoot, "notes", "todo.txt")); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored mode=%v err=%v", info, err)
	}

	archiveData, err := os.ReadFile(snapshot.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	archiveData[len(archiveData)/2] ^= 0xff
	if err := os.WriteFile(snapshot.ArchivePath, archiveData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); err == nil {
		t.Fatal("tampered workspace archive passed checksum validation")
	}
}

func TestWorkspaceRestoreRejectsArchiveOverlapAndTraversal(t *testing.T) {
	for _, test := range []struct {
		name       string
		entry      string
		exclusions []string
	}{
		{name: "repository overlap", entry: "repo/evil.txt", exclusions: []string{"repo"}},
		{name: "parent traversal", entry: "../evil.txt"},
		{name: "unclean path", entry: "a/../evil.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ownershipRoot := t.TempDir()
			bundleRoot := filepath.Join(ownershipRoot, "bundle")
			if err := os.MkdirAll(filepath.Join(bundleRoot, "repo"), 0o700); err != nil {
				t.Fatal(err)
			}
			var content bytes.Buffer
			writer := tar.NewWriter(&content)
			if err := writer.WriteHeader(&tar.Header{Name: test.entry, Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath("session")))
			if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archivePath, content.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(content.Bytes())
			snapshot := state.WorkspaceSnapshot{SessionID: "session", ArchivePath: archivePath, SHA256: hex.EncodeToString(digest[:]), Status: "ARCHIVED", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}
			if err := RestoreWorkspace(context.Background(), bundleRoot, ownershipRoot, ownershipRoot, snapshot, test.exclusions); err == nil {
				t.Fatal("unsafe workspace archive restored")
			}
			if _, err := os.Lstat(filepath.Join(ownershipRoot, "evil.txt")); !os.IsNotExist(err) {
				t.Fatalf("archive escaped restore root: %v", err)
			}
		})
	}
}

func TestDeleteWorkspaceSnapshotRequiresMatchingArtifact(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tampered := snapshot
	tampered.SHA256 = "00"
	if err := DeleteWorkspaceSnapshot(ownershipRoot, tampered); err == nil {
		t.Fatal("tampered workspace snapshot was deleted")
	}
	if err := DeleteWorkspaceSnapshot(ownershipRoot, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(snapshot.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("workspace snapshot still exists: %v", err)
	}
	if err := DeleteWorkspaceSnapshot(ownershipRoot, snapshot); err != nil {
		t.Fatalf("replayed workspace snapshot deletion: %v", err)
	}
}

func writeWorkspaceTestFile(t *testing.T, name, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func assertWorkspaceTestFile(t *testing.T, name, expected string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != expected {
		t.Fatalf("%s=%q err=%v", name, data, err)
	}
}
