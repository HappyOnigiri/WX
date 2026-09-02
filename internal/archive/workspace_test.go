package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
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

func TestSnapshotWorkspaceAtUsesPinnedRootAcrossReplacement(t *testing.T) {
	parent := t.TempDir()
	ownershipRoot := filepath.Join(parent, "wx")
	outside := filepath.Join(parent, "outside")
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.MkdirAll(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "notes.txt"), "old root\n", 0o600)
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := os.Rename(ownershipRoot, ownershipRoot+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ownershipRoot); err != nil {
		t.Fatal(err)
	}

	_, err = SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, "replaced-root", nil, time.Now().Add(time.Hour))
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("replaced workspace root returned non-ownership error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ownershipRoot+"-old", "recovery")); !os.IsNotExist(err) {
		t.Fatalf("workspace archive was written after root replacement: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "recovery")); !os.IsNotExist(err) {
		t.Fatalf("workspace archive escaped into replacement root: %v", err)
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

func TestWorkspaceSnapshotRejectsUnsafeInputsAndUnsupportedFiles(t *testing.T) {
	ownershipRoot, err := os.MkdirTemp("/tmp", "wx-archive-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(ownershipRoot) })
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "file.txt"), "data", 0o600)

	if _, err := SnapshotWorkspace(context.Background(), t.TempDir(), ownershipRoot, "outside", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot outside ownership root succeeded")
	}
	if _, err := SnapshotWorkspace(context.Background(), filepath.Join(ownershipRoot, "missing"), ownershipRoot, "missing", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot of missing bundle succeeded")
	}
	for _, exclusion := range []string{"", "..", "a/../b", `/absolute`, `back\\slash`} {
		if _, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "unsafe", []string{exclusion}, time.Now().Add(time.Hour)); err == nil {
			t.Fatalf("unsafe exclusion %q accepted", exclusion)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SnapshotWorkspace(canceled, bundleRoot, ownershipRoot, "canceled", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}

	socketPath := filepath.Join(bundleRoot, "unsupported.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if _, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "socket", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot containing a unix socket succeeded")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ownershipRoot, "recovery")); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ownershipRoot, "recovery"), "collision", 0o600)
	if _, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "recovery-collision", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot with a non-directory recovery path succeeded")
	}
}

func TestWorkspaceSnapshotValidationRejectsInvalidMetadataAndArtifacts(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	for _, mutate := range []func(*state.WorkspaceSnapshot){
		func(value *state.WorkspaceSnapshot) { value.Status = "PENDING" },
		func(value *state.WorkspaceSnapshot) { value.ExpiresAt = "not-a-time" },
		func(value *state.WorkspaceSnapshot) { value.ExpiresAt = state.FormatTime(time.Now().Add(-time.Hour)) },
		func(value *state.WorkspaceSnapshot) { value.ArchivePath += ".other" },
	} {
		invalid := snapshot
		mutate(&invalid)
		if err := ValidateWorkspaceSnapshot(ownershipRoot, invalid, time.Now()); err == nil {
			t.Fatalf("invalid snapshot metadata accepted: %+v", invalid)
		}
	}

	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); !os.IsNotExist(err) {
		t.Fatalf("missing artifact error=%v", err)
	}
	if err := os.Mkdir(snapshot.ArchivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); err == nil {
		t.Fatal("directory artifact accepted")
	}
	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bundleRoot, snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); err == nil {
		t.Fatal("symlink artifact accepted")
	}

	missingRoot := filepath.Join(t.TempDir(), "missing-root")
	missingRootSnapshot := snapshot
	missingRootSnapshot.ArchivePath = filepath.Join(missingRoot, filepath.FromSlash(workspaceSnapshotRelativePath(snapshot.SessionID)))
	if err := ValidateWorkspaceSnapshot(missingRoot, missingRootSnapshot, time.Now()); err == nil {
		t.Fatal("snapshot under missing ownership root validated")
	}
	if err := DeleteWorkspaceSnapshot(missingRoot, missingRootSnapshot); err == nil {
		t.Fatal("snapshot under missing ownership root deleted")
	}
}

func TestWorkspaceRestoreRejectsUnsafeTargetsAndArchiveShapes(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	empty := writeWorkspaceArchive(t, ownershipRoot, "empty", nil)
	if err := RestoreWorkspace(context.Background(), t.TempDir(), ownershipRoot, ownershipRoot, empty, nil); err == nil {
		t.Fatal("restore outside ownership root succeeded")
	}
	if err := RestoreWorkspace(context.Background(), bundleRoot, ownershipRoot, ownershipRoot, empty, []string{".."}); err == nil {
		t.Fatal("restore with unsafe exclusion succeeded")
	}
	if err := RestoreWorkspace(context.Background(), filepath.Join(ownershipRoot, "missing-target"), ownershipRoot, ownershipRoot, empty, nil); err == nil {
		t.Fatal("restore into missing target succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestoreWorkspace(canceled, bundleRoot, ownershipRoot, ownershipRoot, empty, nil); err == nil {
		t.Fatal("canceled restore succeeded")
	}

	shapes := []struct {
		name    string
		headers []tar.Header
	}{
		{
			name: "duplicate",
			headers: []tar.Header{
				{Name: "same", Typeflag: tar.TypeReg, Mode: 0o600},
				{Name: "same", Typeflag: tar.TypeReg, Mode: 0o600},
			},
		},
		{
			name: "symlink descendant",
			headers: []tar.Header{
				{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "elsewhere"},
				{Name: "link/child", Typeflag: tar.TypeReg, Mode: 0o600},
			},
		},
		{
			name:    "unsupported type",
			headers: []tar.Header{{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "target"}},
		},
	}
	for _, test := range shapes {
		t.Run(test.name, func(t *testing.T) {
			target := filepath.Join(ownershipRoot, "target-"+test.name)
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			snapshot := writeWorkspaceArchive(t, ownershipRoot, test.name, test.headers)
			if err := RestoreWorkspace(context.Background(), target, ownershipRoot, ownershipRoot, snapshot, nil); err == nil {
				t.Fatal("unsafe archive shape restored")
			}
		})
	}

	ancestorTarget := filepath.Join(ownershipRoot, "ancestor")
	if err := os.Mkdir(ancestorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ancestorTarget, "parent"), "not a directory", 0o600)
	if err := RestoreWorkspace(context.Background(), ancestorTarget, ownershipRoot, ownershipRoot, empty, []string{"parent/child"}); err == nil {
		t.Fatal("non-directory exclusion ancestor accepted")
	}

	childTarget := filepath.Join(ownershipRoot, "child-collision")
	if err := os.Mkdir(childTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	childArchive := writeWorkspaceArchive(t, ownershipRoot, "child-collision", []tar.Header{
		{Name: "parent", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "parent/child", Typeflag: tar.TypeReg, Mode: 0o600},
	})
	if err := RestoreWorkspace(context.Background(), childTarget, ownershipRoot, ownershipRoot, childArchive, nil); err == nil {
		t.Fatal("archive created a child beneath a regular file")
	}

	invalidTarget := filepath.Join(ownershipRoot, "invalid-tar")
	if err := os.Mkdir(invalidTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	invalidPath := filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath("invalid-tar")))
	if err := os.WriteFile(invalidPath, []byte("not a tar archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("not a tar archive"))
	invalid := state.WorkspaceSnapshot{
		SessionID: "invalid-tar", ArchivePath: invalidPath, SHA256: hex.EncodeToString(digest[:]),
		Status: "ARCHIVED", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}
	if err := RestoreWorkspace(context.Background(), invalidTarget, ownershipRoot, ownershipRoot, invalid, nil); err == nil {
		t.Fatal("invalid tar archive restored")
	}
}

func TestDeleteWorkspaceSnapshotRejectsWrongPathAndNonRegularArtifact(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := snapshot
	wrongPath.ArchivePath += ".other"
	if err := DeleteWorkspaceSnapshot(ownershipRoot, wrongPath); err == nil {
		t.Fatal("snapshot with wrong path deleted")
	}
	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshot.ArchivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteWorkspaceSnapshot(ownershipRoot, snapshot); err == nil {
		t.Fatal("directory snapshot artifact deleted")
	}
}

func TestWorkspaceSnapshotSurfacesFilesystemPermissionFailures(t *testing.T) {
	t.Run("unreadable file during snapshot", func(t *testing.T) {
		ownershipRoot := t.TempDir()
		bundleRoot := filepath.Join(ownershipRoot, "bundle")
		if err := os.Mkdir(bundleRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(bundleRoot, "private")
		if err := os.WriteFile(file, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(file, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(file, 0o600) })
		if _, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "unreadable-file", nil, time.Now().Add(time.Hour)); err == nil {
			t.Fatal("snapshot read an unreadable file")
		}
	})

	t.Run("unreadable directory during snapshot", func(t *testing.T) {
		ownershipRoot := t.TempDir()
		bundleRoot := filepath.Join(ownershipRoot, "bundle")
		directory := filepath.Join(bundleRoot, "private")
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "file"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(directory, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
		if _, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "unreadable-directory", nil, time.Now().Add(time.Hour)); err == nil {
			t.Fatal("snapshot traversed an unreadable directory")
		}
	})

	t.Run("unreadable archive", func(t *testing.T) {
		ownershipRoot := t.TempDir()
		bundleRoot := filepath.Join(ownershipRoot, "bundle")
		if err := os.Mkdir(bundleRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot, err := SnapshotWorkspace(context.Background(), bundleRoot, ownershipRoot, "unreadable-archive", nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(snapshot.ArchivePath, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(snapshot.ArchivePath, 0o600) })
		if err := ValidateWorkspaceSnapshot(ownershipRoot, snapshot, time.Now()); err == nil {
			t.Fatal("unreadable archive validated")
		}
		if err := RestoreWorkspace(context.Background(), bundleRoot, ownershipRoot, ownershipRoot, snapshot, nil); err == nil {
			t.Fatal("unreadable archive restored")
		}
		if err := DeleteWorkspaceSnapshot(ownershipRoot, snapshot); err == nil {
			t.Fatal("unreadable archive deleted")
		}
	})

	t.Run("unreadable restore target", func(t *testing.T) {
		ownershipRoot := t.TempDir()
		source := filepath.Join(ownershipRoot, "source")
		target := filepath.Join(ownershipRoot, "target")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot, err := SnapshotWorkspace(context.Background(), source, ownershipRoot, "unreadable-target", nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
		if err := RestoreWorkspace(context.Background(), target, ownershipRoot, ownershipRoot, snapshot, nil); err == nil {
			t.Fatal("archive restored into unreadable target")
		}
	})
}

func writeWorkspaceArchive(t *testing.T, ownershipRoot, sessionID string, headers []tar.Header) state.WorkspaceSnapshot {
	t.Helper()
	var content bytes.Buffer
	writer := tar.NewWriter(&content)
	for i := range headers {
		if err := writer.WriteHeader(&headers[i]); err != nil {
			t.Fatal(err)
		}
		if headers[i].Size > 0 {
			if _, err := writer.Write(make([]byte, headers[i].Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(ownershipRoot, filepath.FromSlash(workspaceSnapshotRelativePath(sessionID)))
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archivePath, content.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content.Bytes())
	return state.WorkspaceSnapshot{
		SessionID: sessionID, ArchivePath: archivePath, SHA256: hex.EncodeToString(digest[:]),
		Status: "ARCHIVED", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}
}

func TestPruneWorkspaceRootPropagatesClosedRootFailure(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pruneWorkspaceRoot(root, ".", nil); err == nil {
		t.Fatal("pruning through a closed root succeeded")
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
