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
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	expiry := time.Now().Add(time.Hour)
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "session", []string{"repo", "audit"}, expiry)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err != nil {
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

	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, snapshot, []string{"repo", "audit"}); err != nil {
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
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err == nil {
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

	_, err = SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "replaced-root", nil, time.Now().Add(time.Hour))
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

func TestPinnedWorkspaceSnapshotRestoreValidationAndDeletion(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "notes", "saved.txt"), "pinned state\n", 0o640)

	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, nil, "nil-owner", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("nil descriptor was accepted")
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "pinned", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("pinned snapshot: %v", err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err != nil {
		t.Fatalf("pinned validation: %v", err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired pinned snapshot was accepted")
	}
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, snapshot, nil); err != nil {
		t.Fatalf("pinned restore: %v", err)
	}
	assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "notes", "saved.txt"), "pinned state\n")
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err != nil {
		t.Fatalf("pinned deletion: %v", err)
	}
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err != nil {
		t.Fatalf("idempotent pinned deletion: %v", err)
	}
}

func TestPinnedWorkspaceOperationsRejectClosedAndMismatchedDescriptors(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "boundary", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, nil, ownershipRoot, owner, snapshot, nil); err == nil {
		t.Fatal("restore accepted a nil target descriptor")
	}
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, nil, snapshot, nil); err == nil {
		t.Fatal("restore accepted a nil archive descriptor")
	}
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, nil, snapshot); err == nil {
		t.Fatal("delete accepted a nil descriptor")
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, nil, snapshot, time.Now()); err == nil {
		t.Fatal("validation accepted a nil descriptor")
	}
	if err := verifyPinnedRootPath(filepath.Join(t.TempDir(), "missing"), owner); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("mismatched root error=%v", err)
	}
	if _, _, err := openWorkspaceRestoreRoots(filepath.Join(t.TempDir(), "outside"), ownershipRoot, owner, ownershipRoot, owner, snapshot); err == nil {
		t.Fatal("pinned restore opened a bundle outside the ownership root")
	}
	if _, _, err := openWorkspaceRestoreRoots(bundleRoot, ownershipRoot, nil, filepath.Join(t.TempDir(), "missing"), nil, snapshot); err == nil {
		t.Fatal("restore opened an archive below a missing ownership root")
	}
	closed, err := os.OpenRoot(ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, closed, snapshot, time.Now()); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("closed validation error=%v", err)
	}
	if _, _, err := openWorkspaceRestoreRoots(bundleRoot, ownershipRoot, closed, ownershipRoot, owner, snapshot); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("closed target restore root error=%v", err)
	}
	if err := restoreWorkspaceRegularFile(owner, tar.NewReader(bytes.NewReader(nil)), "bad", "bad", &tar.Header{Size: -1}); err == nil {
		t.Fatal("negative archive file size was accepted")
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
			snapshot := state.WorkspaceSnapshot{SessionID: "session", RootID: testRootID, RelPath: workspaceSnapshotRelPath("session"), ArchivePath: archivePath, SHA256: hex.EncodeToString(digest[:]), Status: "ARCHIVED", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour))}
			owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = owner.Close() }()
			if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, snapshot, test.exclusions); err == nil {
				t.Fatal("unsafe workspace archive restored")
			}
			if _, err := os.Lstat(filepath.Join(ownershipRoot, "evil.txt")); !os.IsNotExist(err) {
				t.Fatalf("archive escaped restore root: %v", err)
			}
		})
	}
}

func TestWorkspaceRestoreAcceptsDirectoryCreatedByChildEntry(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	// A tar writer is allowed to omit an explicit parent directory or emit it
	// after a child. Restore must keep the parent physical and still materialize
	// the regular file without following an attacker-controlled path.
	snapshot := writeWorkspaceArchive(t, ownershipRoot, "directory-order", []tar.Header{
		{Name: "nested/file.txt", Typeflag: tar.TypeReg, Mode: 0o640, Size: 4},
		{Name: "nested", Typeflag: tar.TypeDir, Mode: 0o700},
	})
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, snapshot, nil); err != nil {
		t.Fatalf("restore directory-first archive: %v", err)
	}
	if info, err := os.Stat(filepath.Join(bundleRoot, "nested")); err != nil || !info.IsDir() {
		t.Fatalf("restored parent info=%v err=%v", info, err)
	}
	if info, err := os.Stat(filepath.Join(bundleRoot, "nested", "file.txt")); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o640 {
		t.Fatalf("restored child info=%v err=%v", info, err)
	}
}

func TestDeleteWorkspaceSnapshotRequiresMatchingArtifact(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	tampered := snapshot
	tampered.SHA256 = "00"
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, tampered); err == nil {
		t.Fatal("tampered workspace snapshot was deleted")
	}
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(snapshot.ArchivePath); !os.IsNotExist(err) {
		t.Fatalf("workspace snapshot still exists: %v", err)
	}
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err != nil {
		t.Fatalf("replayed workspace snapshot deletion: %v", err)
	}
}

func TestWorkspaceSnapshotRejectsUnsafeInputsAndUnsupportedFiles(t *testing.T) {
	// /tmp (rather than t.TempDir()) keeps the unix socket path below the
	// platform's socket path length limit later in this test. It is resolved
	// to its physical form immediately because /tmp itself is a symlink on
	// macOS (-> /private/tmp) and the pinned root descriptor this test opens
	// below rejects a symlink ancestor.
	rawOwnershipRoot, err := os.MkdirTemp("/tmp", "wx-archive-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(rawOwnershipRoot) })
	ownershipRoot, err := filepath.EvalSymlinks(rawOwnershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "file.txt"), "data", 0o600)
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()

	if _, err := SnapshotWorkspaceAt(context.Background(), t.TempDir(), ownershipRoot, testRootID, owner, "outside", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot outside ownership root succeeded")
	}
	if _, err := SnapshotWorkspaceAt(context.Background(), filepath.Join(ownershipRoot, "missing"), ownershipRoot, testRootID, owner, "missing", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot of missing bundle succeeded")
	}
	for _, exclusion := range []string{"", "..", "a/../b", `/absolute`, `back\\slash`} {
		if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "unsafe", []string{exclusion}, time.Now().Add(time.Hour)); err == nil {
			t.Fatalf("unsafe exclusion %q accepted", exclusion)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SnapshotWorkspaceAt(canceled, bundleRoot, ownershipRoot, testRootID, owner, "canceled", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("canceled snapshot succeeded")
	}

	socketPath := filepath.Join(bundleRoot, "unsupported.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "socket", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot containing a unix socket succeeded")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(ownershipRoot, recoveryNamespace)); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ownershipRoot, recoveryNamespace), "collision", 0o600)
	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "recovery-collision", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot with a non-directory recovery path succeeded")
	}
}

func TestWorkspaceSnapshotValidationRejectsInvalidMetadataAndArtifacts(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	for _, mutate := range []func(*state.WorkspaceSnapshot){
		func(value *state.WorkspaceSnapshot) { value.Status = "PENDING" },
		func(value *state.WorkspaceSnapshot) { value.ExpiresAt = "not-a-time" },
		func(value *state.WorkspaceSnapshot) { value.ExpiresAt = state.FormatTime(time.Now().Add(-time.Hour)) },
		func(value *state.WorkspaceSnapshot) { value.RelPath += ".other" },
	} {
		invalid := snapshot
		mutate(&invalid)
		if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, invalid, time.Now()); err == nil {
			t.Fatalf("invalid snapshot metadata accepted: %+v", invalid)
		}
	}

	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); !os.IsNotExist(err) {
		t.Fatalf("missing artifact error=%v", err)
	}
	if err := os.Mkdir(snapshot.ArchivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err == nil {
		t.Fatal("directory artifact accepted")
	}
	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bundleRoot, snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err == nil {
		t.Fatal("symlink artifact accepted")
	}

	// A missing ownership root cannot even be opened as a pinned descriptor,
	// so the *At entry points cannot be reached with one; the fail-closed
	// behavior for a missing root is the descriptor open itself failing.
	missingRoot := filepath.Join(t.TempDir(), "missing-root")
	if _, _, err := domain.OpenOwnedRoot(missingRoot, missingRoot); err == nil {
		t.Fatal("missing ownership root was opened as a pinned descriptor")
	}
}

func TestWorkspaceRestoreRejectsUnsafeTargetsAndArchiveShapes(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	empty := writeWorkspaceArchive(t, ownershipRoot, "empty", nil)
	if err := RestoreWorkspaceAt(context.Background(), t.TempDir(), ownershipRoot, owner, ownershipRoot, owner, empty, nil); err == nil {
		t.Fatal("restore outside ownership root succeeded")
	}
	if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, empty, []string{".."}); err == nil {
		t.Fatal("restore with unsafe exclusion succeeded")
	}
	if err := RestoreWorkspaceAt(context.Background(), filepath.Join(ownershipRoot, "missing-target"), ownershipRoot, owner, ownershipRoot, owner, empty, nil); err == nil {
		t.Fatal("restore into missing target succeeded")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RestoreWorkspaceAt(canceled, bundleRoot, ownershipRoot, owner, ownershipRoot, owner, empty, nil); err == nil {
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
			if err := RestoreWorkspaceAt(context.Background(), target, ownershipRoot, owner, ownershipRoot, owner, snapshot, nil); err == nil {
				t.Fatal("unsafe archive shape restored")
			}
		})
	}

	ancestorTarget := filepath.Join(ownershipRoot, "ancestor")
	if err := os.Mkdir(ancestorTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(ancestorTarget, "parent"), "not a directory", 0o600)
	if err := RestoreWorkspaceAt(context.Background(), ancestorTarget, ownershipRoot, owner, ownershipRoot, owner, empty, []string{"parent/child"}); err == nil {
		t.Fatal("non-directory exclusion ancestor accepted")
	}

	directoryTarget := filepath.Join(ownershipRoot, "directory-entry")
	if err := os.Mkdir(directoryTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	directoryArchive := writeWorkspaceArchive(t, ownershipRoot, "directory-entry", []tar.Header{{Name: "created", Typeflag: tar.TypeDir, Mode: 0o700}})
	if err := RestoreWorkspaceAt(context.Background(), directoryTarget, ownershipRoot, owner, ownershipRoot, owner, directoryArchive, nil); err != nil {
		t.Fatalf("restore explicit directory entry: %v", err)
	}
	if info, err := os.Stat(filepath.Join(directoryTarget, "created")); err != nil || !info.IsDir() {
		t.Fatalf("restored directory info=%v err=%v", info, err)
	}

	childTarget := filepath.Join(ownershipRoot, "child-collision")
	if err := os.Mkdir(childTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	childArchive := writeWorkspaceArchive(t, ownershipRoot, "child-collision", []tar.Header{
		{Name: "parent", Typeflag: tar.TypeReg, Mode: 0o600},
		{Name: "parent/child", Typeflag: tar.TypeReg, Mode: 0o600},
	})
	if err := RestoreWorkspaceAt(context.Background(), childTarget, ownershipRoot, owner, ownershipRoot, owner, childArchive, nil); err == nil {
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
		SessionID: "invalid-tar", RootID: testRootID, RelPath: workspaceSnapshotRelPath("invalid-tar"),
		ArchivePath: invalidPath, SHA256: hex.EncodeToString(digest[:]),
		Status: "ARCHIVED", ExpiresAt: state.FormatTime(time.Now().Add(time.Hour)),
	}
	if err := RestoreWorkspaceAt(context.Background(), invalidTarget, ownershipRoot, owner, ownershipRoot, owner, invalid, nil); err == nil {
		t.Fatal("invalid tar archive restored")
	}
}

func TestDeleteWorkspaceSnapshotRejectsWrongPathAndNonRegularArtifact(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.Mkdir(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "session", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wrongPath := snapshot
	wrongPath.RelPath += ".other"
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, wrongPath); err == nil {
		t.Fatal("snapshot with wrong path deleted")
	}
	if err := os.Remove(snapshot.ArchivePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshot.ArchivePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err == nil {
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
		owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "unreadable-file", nil, time.Now().Add(time.Hour)); err == nil {
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
		owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "unreadable-directory", nil, time.Now().Add(time.Hour)); err == nil {
			t.Fatal("snapshot traversed an unreadable directory")
		}
	})

	t.Run("unreadable archive", func(t *testing.T) {
		ownershipRoot := t.TempDir()
		bundleRoot := filepath.Join(ownershipRoot, "bundle")
		if err := os.Mkdir(bundleRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "unreadable-archive", nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(snapshot.ArchivePath, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(snapshot.ArchivePath, 0o600) })
		if err := ValidateWorkspaceSnapshotAt(ownershipRoot, owner, snapshot, time.Now()); err == nil {
			t.Fatal("unreadable archive validated")
		}
		if err := RestoreWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, owner, ownershipRoot, owner, snapshot, nil); err == nil {
			t.Fatal("unreadable archive restored")
		}
		if err := DeleteWorkspaceSnapshotAt(ownershipRoot, owner, snapshot); err == nil {
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
		owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = owner.Close() }()
		snapshot, err := SnapshotWorkspaceAt(context.Background(), source, ownershipRoot, testRootID, owner, "unreadable-target", nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
		if err := RestoreWorkspaceAt(context.Background(), target, ownershipRoot, owner, ownershipRoot, owner, snapshot, nil); err == nil {
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
		SessionID: sessionID, RootID: testRootID, RelPath: workspaceSnapshotRelPath(sessionID),
		ArchivePath: archivePath, SHA256: hex.EncodeToString(digest[:]),
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

func TestSnapshotWorkspacePropagatesPublishRenameFailure(t *testing.T) {
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "file.txt"), "data", 0o600)
	// Pre-occupy the deterministic archive destination with a non-empty
	// directory so the final rename from the temporary file cannot succeed,
	// exercising the publish-time failure path without touching production
	// behavior.
	collision := filepath.Join(ownershipRoot, workspaceSnapshotRelativePath("collision"))
	if err := os.MkdirAll(collision, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(collision, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if _, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, "collision", nil, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("snapshot publish succeeded despite a colliding archive destination")
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
