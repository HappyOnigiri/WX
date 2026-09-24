package archive

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
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

// TestMutationRestoreVerifiedWorkspaceRechecksArchiveAfterRestore は展開中に
// 検証済み archive が in-place 変更された場合、展開後の再検証で拒否することを確認する。
func TestMutationRestoreVerifiedWorkspaceRechecksArchiveAfterRestore(t *testing.T) {
	archiveRoot := t.TempDir()
	source := filepath.Join(archiveRoot, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(source, "aaa-marker"), "marker\n", 0o600)
	writeWorkspaceTestFile(t, filepath.Join(source, "zzz-large"), string(bytes.Repeat([]byte("payload\n"), 8*workspaceSnapshotHashChunk/8)), 0o600)
	archiveOwner, _, err := domain.OpenOwnedRoot(archiveRoot, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archiveOwner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), source, archiveRoot, testRootID, archiveOwner, "post-restore-archive-change", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), archiveRoot, archiveOwner, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = verified.Close() }()

	targetRoot := t.TempDir()
	target := filepath.Join(targetRoot, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	targetOwner, _, err := domain.OpenOwnedRoot(targetRoot, targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetOwner.Close() }()
	changed := mutateWorkspaceArchiveAfterMarker(filepath.Join(target, "aaa-marker"), snapshot.ArchivePath)
	err = RestoreVerifiedWorkspace(context.Background(), verified, target, targetRoot, targetOwner, nil)
	if mutationErr := <-changed; mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if !errors.Is(err, ErrWorkspaceSnapshotIntegrity) {
		t.Fatalf("restore accepted an archive changed after verification: %v", err)
	}
}

// TestMutationRestoreVerifiedWorkspaceRechecksTargetRoot は展開後に target root
// の path が別 inode へ置き換わった場合、復元を成功扱いにしないことを確認する。
func TestMutationRestoreVerifiedWorkspaceRechecksTargetRoot(t *testing.T) {
	archiveRoot := t.TempDir()
	source := filepath.Join(archiveRoot, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTestFile(t, filepath.Join(source, "aaa-marker"), "marker\n", 0o600)
	writeWorkspaceTestFile(t, filepath.Join(source, "zzz-large"), string(bytes.Repeat([]byte("payload\n"), 8*workspaceSnapshotHashChunk/8)), 0o600)
	archiveOwner, _, err := domain.OpenOwnedRoot(archiveRoot, archiveRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = archiveOwner.Close() }()
	snapshot, err := SnapshotWorkspaceAt(context.Background(), source, archiveRoot, testRootID, archiveOwner, "post-restore-target-change", nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	verified, err := OpenVerifiedWorkspaceSnapshotAt(context.Background(), archiveRoot, archiveOwner, snapshot, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = verified.Close() }()

	targetRoot := t.TempDir()
	target := filepath.Join(targetRoot, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	targetOwner, _, err := domain.OpenOwnedRoot(targetRoot, targetRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = targetOwner.Close() }()
	replaced := replaceWorkspaceRootAfterMarker(filepath.Join(target, "aaa-marker"), targetRoot)
	err = RestoreVerifiedWorkspace(context.Background(), verified, target, targetRoot, targetOwner, nil)
	if replacementErr := <-replaced; replacementErr != nil {
		t.Fatal(replacementErr)
	}
	if !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("restore accepted a replaced target root: %v", err)
	}
}

func mutateWorkspaceArchiveAfterMarker(marker, archivePath string) <-chan error {
	result := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				file, openErr := os.OpenFile(archivePath, os.O_WRONLY, 0o600)
				if openErr != nil {
					result <- openErr
					return
				}
				_, writeErr := file.WriteAt([]byte{0xff}, 0)
				closeErr := file.Close()
				if writeErr != nil {
					result <- writeErr
				} else {
					result <- closeErr
				}
				return
			}
			if time.Now().After(deadline) {
				result <- errors.New("timed out waiting for restored archive marker")
				return
			}
			runtime.Gosched()
		}
	}()
	return result
}

func replaceWorkspaceRootAfterMarker(marker, rootPath string) <-chan error {
	result := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				oldPath := rootPath + "-old"
				if err := os.Rename(rootPath, oldPath); err != nil {
					result <- err
					return
				}
				if err := os.Mkdir(rootPath, 0o700); err != nil {
					result <- err
					return
				}
				result <- nil
				return
			}
			if time.Now().After(deadline) {
				result <- errors.New("timed out waiting for restored target marker")
				return
			}
			runtime.Gosched()
		}
	}()
	return result
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
