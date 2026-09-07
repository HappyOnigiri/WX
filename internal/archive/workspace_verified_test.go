package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// largeWorkspaceFixture は hash の読み取り単位を複数回またぐ archive を作る。
// 読み取り量が archive の大きさに比例することを、軽量な metadata 検査と対比して確かめるために使う。
func largeWorkspaceFixture(t *testing.T, sessionID string) (string, *os.Root, string, state.WorkspaceSnapshot) {
	t.Helper()
	ownershipRoot := t.TempDir()
	bundleRoot := filepath.Join(ownershipRoot, "bundle")
	if err := os.MkdirAll(bundleRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("workspace payload\n"), 3*workspaceSnapshotHashChunk/18)
	if err := os.WriteFile(filepath.Join(bundleRoot, "large.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(ownershipRoot, ownershipRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	snapshot, err := SnapshotWorkspaceAt(context.Background(), bundleRoot, ownershipRoot, testRootID, owner, sessionID, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(snapshot.ArchivePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() <= workspaceSnapshotHashChunk {
		t.Fatalf("fixture archive size=%d must exceed the hash chunk %d", info.Size(), workspaceSnapshotHashChunk)
	}
	return ownershipRoot, owner, bundleRoot, snapshot
}

// overwriteArchiveInPlace は inode と大きさを保ったまま archive の内容だけを書き換える。
func overwriteArchiveInPlace(t *testing.T, path string) {
	t.Helper()
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(bytes.Repeat([]byte{0xff}, 512), before.Size()/2); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("in-place overwrite changed size %d -> %d", before.Size(), after.Size())
	}
}

func TestWorkspaceSnapshotMetadataCheckDoesNotReadArchiveBody(t *testing.T) {
	ownershipRoot, owner, _, snapshot := largeWorkspaceFixture(t, "metadata-only")
	overwriteArchiveInPlace(t, snapshot.ArchivePath)
	if err := ValidateWorkspaceSnapshotMetadataAt(ownershipRoot, owner, snapshot, time.Now()); err != nil {
		t.Fatalf("metadata check read the archive body: %v", err)
	}
	err := ValidateWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, time.Now())
	if !errors.Is(err, ErrWorkspaceSnapshotIntegrity) {
		t.Fatalf("full validation of a corrupted archive error=%v", err)
	}
}

func TestWorkspaceSnapshotMetadataCheckSeparatesExpiryFromPathFailures(t *testing.T) {
	ownershipRoot, owner, _, snapshot := largeWorkspaceFixture(t, "metadata-expiry")
	if err := ValidateWorkspaceSnapshotMetadataAt(ownershipRoot, owner, snapshot, time.Now()); err != nil {
		t.Fatal(err)
	}
	expired := snapshot
	expired.ExpiresAt = state.FormatTime(time.Now().Add(-time.Hour))
	if err := ValidateWorkspaceSnapshotMetadataAt(ownershipRoot, owner, expired, time.Now()); !errors.Is(err, ErrWorkspaceSnapshotExpired) {
		t.Fatalf("expired snapshot error=%v", err)
	}
	moved := snapshot
	moved.RelPath = filepath.Join("_recovery", "workspace-snapshots", "other.tar")
	if err := ValidateWorkspaceSnapshotMetadataAt(ownershipRoot, owner, moved, time.Now()); err == nil || errors.Is(err, ErrWorkspaceSnapshotExpired) {
		t.Fatalf("path mismatch must not be reported as expiry: %v", err)
	}
}

func TestVerifiedWorkspaceSnapshotHashHonorsCancellation(t *testing.T) {
	ownershipRoot, owner, _, snapshot := largeWorkspaceFixture(t, "hash-cancel")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	verified, err := OpenVerifiedWorkspaceSnapshotAt(canceled, ownershipRoot, owner, snapshot, time.Now())
	if verified != nil {
		_ = verified.Close()
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled hash error=%v", err)
	}
	if errors.Is(err, ErrWorkspaceSnapshotIntegrity) {
		t.Fatal("cancellation was reported as an integrity failure")
	}
}

func TestVerifiedWorkspaceSnapshotRejectsInPlaceChangeBeforeRestore(t *testing.T) {
	ownershipRoot, owner, bundleRoot, snapshot := largeWorkspaceFixture(t, "in-place-change")
	verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = verified.Close() }()
	writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "current-only.txt"), "keep me until the archive is trusted\n", 0o600)
	overwriteArchiveInPlace(t, snapshot.ArchivePath)
	err = RestoreVerifiedWorkspace(context.Background(), verified, bundleRoot, ownershipRoot, owner, nil)
	if !errors.Is(err, ErrWorkspaceSnapshotIntegrity) {
		t.Fatalf("restore after an in-place archive change error=%v", err)
	}
	assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "current-only.txt"), "keep me until the archive is trusted\n")
}

// TestVerifiedWorkspaceSnapshotRejectsPathReplacementAfterVerification は、検証済み descriptor が指す実体の
// link 数が変わる rename・symlink 置換も変更として扱い、復元を始めないことを確かめる。
// 貸出中 snapshot は RESTORE 予約が GC から守るため、この検出が正常系を妨げることはない。
func TestVerifiedWorkspaceSnapshotRejectsPathReplacementAfterVerification(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(t *testing.T, ownershipRoot, archivePath string)
	}{
		{name: "rename", replace: func(t *testing.T, ownershipRoot, archivePath string) {
			t.Helper()
			if err := os.Rename(archivePath, filepath.Join(filepath.Dir(archivePath), "moved.tar")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", replace: func(t *testing.T, ownershipRoot, archivePath string) {
			t.Helper()
			decoy := filepath.Join(ownershipRoot, "decoy.tar")
			writeWorkspaceTestFile(t, decoy, "decoy archive\n", 0o600)
			if err := os.Remove(archivePath); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(decoy, archivePath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ownershipRoot, owner, bundleRoot, snapshot := largeWorkspaceFixture(t, "path-replacement-"+test.name)
			verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = verified.Close() }()
			writeWorkspaceTestFile(t, filepath.Join(bundleRoot, "current-only.txt"), "kept\n", 0o600)
			test.replace(t, ownershipRoot, snapshot.ArchivePath)
			err = RestoreVerifiedWorkspace(context.Background(), verified, bundleRoot, ownershipRoot, owner, nil)
			if !errors.Is(err, ErrWorkspaceSnapshotIntegrity) {
				t.Fatalf("restore after archive path replacement error=%v", err)
			}
			assertWorkspaceTestFile(t, filepath.Join(bundleRoot, "current-only.txt"), "kept\n")
		})
	}
}

func TestVerifiedWorkspaceSnapshotCloseReleasesDescriptor(t *testing.T) {
	ownershipRoot, owner, bundleRoot, snapshot := largeWorkspaceFixture(t, "close-release")
	verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), ownershipRoot, owner, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := verified.Close(); err != nil {
		t.Fatal(err)
	}
	if err := verified.Close(); err != nil {
		t.Fatalf("replayed close: %v", err)
	}
	if err := verified.verifyUnchanged(); err == nil {
		t.Fatal("a closed handle reported an unchanged archive")
	}
	if err := RestoreVerifiedWorkspace(context.Background(), verified, bundleRoot, ownershipRoot, owner, nil); err == nil {
		t.Fatal("restore accepted a closed handle")
	}
	if err := RestoreVerifiedWorkspace(context.Background(), nil, bundleRoot, ownershipRoot, owner, nil); err == nil {
		t.Fatal("restore accepted a missing handle")
	}
}
