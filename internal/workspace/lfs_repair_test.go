package workspace

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestLFSRepairLocksForSelectsConfiguredOrDefaultLock(t *testing.T) {
	t.Parallel()
	custom := &gitx.KeyedLocks{}
	if got := lfsRepairLocksFor(&Preparer{LFSLocks: custom}); got != custom {
		t.Fatal("configured LFS lock was not selected")
	}
	if got := lfsRepairLocksFor(&Preparer{}); got == nil {
		t.Fatal("default LFS lock was nil")
	}
	if got := lfsRepairLocksFor(nil); got == nil {
		t.Fatal("nil preparer default LFS lock was nil")
	}
}

func TestLFSRepairUsesFullOIDPathAndVerifiesSource(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("verified LFS content\n")
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	p := &Preparer{LFSLocks: &gitx.KeyedLocks{}}
	result, err := p.RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 0 || len(result.Repaired) != 1 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	destination := lfsCachePath(repo, oid)
	if !strings.HasSuffix(destination, fmt.Sprintf("%x", digest[:])) {
		t.Fatalf("cache path=%q does not use the full oid", destination)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(data) {
		t.Fatalf("cache content=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != fmt.Sprintf("%x", digest[:]) {
		t.Fatalf("cache entries=%v", entries)
	}
}

func TestLFSRepairDoesNotInstallHashMismatchAndTriesOtherCandidates(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	wrong, right := []byte("wrong data"), []byte("right data")
	for name, data := range map[string][]byte{"wrong.bin": wrong, "right.bin": right} {
		if err := os.WriteFile(filepath.Join(source, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(right)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(right)), Paths: []string{"wrong.bin", "right.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 0 || len(result.Repaired) != 1 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(lfsCachePath(repo, oid))
	if err != nil || string(got) != string(right) {
		t.Fatalf("cache content=%q err=%v", got, err)
	}
}

func TestLFSRepairLeavesDestinationForUnusableCandidate(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("expected bytes")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte("not the expected size"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairCandidateMissing {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	if message := result.Unresolved[0].Error(); !strings.Contains(message, string(LFSRepairCandidateMissing)) {
		t.Fatalf("repair failure message=%q", message)
	}
	if _, statErr := os.Stat(lfsCachePath(repo, oid)); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected cache object stat error=%v", statErr)
	}
}

func TestLFSRepairReplacesWrongSizedCacheObject(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("replacement content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := lfsCachePath(discovery.Repository{CommonDir: domain.CanonicalPath(common)}, oid)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheCorrupt}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 0 || len(result.Repaired) != 1 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(data) {
		t.Fatalf("cache content=%q err=%v", got, err)
	}
}

func TestLFSRepairDoesNotOverwriteSizeMatchingCacheObject(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("source content")
	destinationData := []byte("other contents")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if len(data) != len(destinationData) {
		t.Fatal("test data must have matching sizes")
	}
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := lfsCachePath(discovery.Repository{CommonDir: domain.CanonicalPath(common)}, oid)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, destinationData, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheHealthy, Cached: true, CacheSize: int64(len(data))}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 0 || len(result.Repaired) != 0 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(destinationData) {
		t.Fatalf("cache content=%q err=%v", got, err)
	}
}

func TestLFSRepairHashMismatchCleansTemporaryObject(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	wrong := []byte("same-size wrong")
	digest := sha256.Sum256([]byte("expected value"))
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), wrong, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(wrong)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairHashMismatch {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	cacheRoot := filepath.Join(common, "lfs", "objects")
	var temporary []string
	_ = filepath.WalkDir(cacheRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr == nil && entry != nil && strings.HasPrefix(entry.Name(), ".wx-lfs-") {
			temporary = append(temporary, path)
		}
		return nil
	})
	if len(temporary) != 0 {
		t.Fatalf("temporary cache files remain: %v", temporary)
	}
}

func TestDiagnoseLFSObjectsSkipsSymlinkAndDirectoryCandidates(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "real"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(source, "symlink")); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	objects := []LFSObjectInfo{
		{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"directory"}, CacheState: LFSCacheMissing},
		{OID: "sha256:" + strings.Repeat("b", 64), Size: 7, Paths: []string{"symlink"}, CacheState: LFSCacheMissing},
		{OID: "sha256:" + strings.Repeat("c", 64), Size: 7, Paths: []string{"real"}, CacheState: LFSCacheMissing},
	}
	diagnostics, err := DiagnoseLFSObjects(repo, objects)
	if err != nil || len(diagnostics.Objects) != len(objects) {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	if diagnostics.Objects[0].CandidatePath != "" || diagnostics.Objects[1].CandidatePath != "" || diagnostics.Objects[2].CandidatePath != "real" {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}

func TestVerifyLFSPathsAtRejectsPointerSizedMismatch(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "asset.bin"), []byte("pointer"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("a", 64), Size: 123, Paths: []string{"asset.bin"}}
	if err := VerifyLFSPathsAt(root, ".", []LFSObjectInfo{object}); err == nil {
		t.Fatal("pointer-sized worktree was accepted")
	}
}

func TestPreparerVerifyPreparedLFSUsesRepositoryObjects(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "asset.bin"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	repo := discovery.Repository{ID: "repo"}
	preparer := &Preparer{LFSObjects: map[string][]LFSObjectInfo{
		string(repo.ID): {{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"asset.bin"}}},
	}}
	if err := preparer.verifyPreparedLFS(root, ".", repo); err != nil {
		t.Fatalf("verify prepared LFS: %v", err)
	}
	if err := (&Preparer{}).verifyPreparedLFS(root, ".", discovery.Repository{ID: "other"}); err != nil {
		t.Fatalf("verify without repository objects: %v", err)
	}
	var nilPreparer *Preparer
	if err := nilPreparer.verifyPreparedLFS(root, ".", repo); err != nil {
		t.Fatalf("verify with nil preparer: %v", err)
	}
}

// repository に登録された LFS object のサイズ不一致は、準備完了後の検証で検出する。
func TestPreparerVerifyPreparedLFSRejectsWrongSize(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "asset.bin"), []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	repo := discovery.Repository{ID: "repo"}
	preparer := &Preparer{LFSObjects: map[string][]LFSObjectInfo{
		string(repo.ID): {{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"asset.bin"}}},
	}}
	if err := preparer.verifyPreparedLFS(root, ".", repo); err == nil {
		t.Fatal("wrong-sized LFS object was accepted")
	}
}

func TestLFSRepairRejectsInvalidOID(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: "not-an-oid", Size: 7, Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
}

func TestLFSRepairLeavesSymlinkDestinationUntouched(t *testing.T) {
	t.Parallel()
	source, common, outside := t.TempDir(), t.TempDir(), t.TempDir()
	data := []byte("content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := lfsCachePath(discovery.Repository{CommonDir: domain.CanonicalPath(common)}, oid)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(outside, "untouched")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, destination); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	info, err := os.Lstat(destination)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cache destination changed: info=%v err=%v", info, err)
	}
	got, err := os.ReadFile(outsideFile)
	if err != nil || string(got) != "outside" {
		t.Fatalf("symlink target changed: content=%q err=%v", got, err)
	}
}

func TestDiagnoseLFSObjectsReportsMissingSourceRepository(t *testing.T) {
	t.Parallel()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(filepath.Join(t.TempDir(), "missing"))}
	_, err := DiagnoseLFSObjects(repo, []LFSObjectInfo{{OID: "sha256:" + strings.Repeat("a", 64), Size: 1, Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}})
	if err == nil {
		t.Fatal("diagnosis succeeded for a missing source repository")
	}
}

func TestLFSRepairFailureMessageIncludesSourceAndCause(t *testing.T) {
	t.Parallel()
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("a", 64)}
	withSource := LFSRepairFailure{Object: object, SourcePath: "asset.bin", Reason: LFSRepairReadFailure, Err: os.ErrPermission}
	message := withSource.Error()
	if !strings.Contains(message, "asset.bin") || !strings.Contains(message, os.ErrPermission.Error()) {
		t.Fatalf("failure message=%q", message)
	}
	withoutSource := LFSRepairFailure{Object: object, Reason: LFSRepairCandidateMissing}
	if message := withoutSource.Error(); strings.Contains(message, "from ") {
		t.Fatalf("failure message=%q names a source path", message)
	}
}

func TestLFSDiagnosisAndRepairRejectIncompleteInput(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("a", 64), Size: 1, Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	if diagnostics, err := DiagnoseLFSObjects(repo, nil); err != nil || len(diagnostics.Objects) != 0 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	if _, err := DiagnoseLFSObjects(discovery.Repository{ID: "repo"}, []LFSObjectInfo{object}); err == nil {
		t.Fatal("diagnosis succeeded without a source repository")
	}
	if result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, nil); err != nil || len(result.Repaired) != 0 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	noCommon := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source)}
	if _, err := (&Preparer{}).RepairLFSObjects(context.Background(), noCommon, []LFSObjectInfo{object}); err == nil {
		t.Fatal("repair succeeded without a common directory")
	}
}

// CacheState を持たない呼び出し元のために、Cached と CacheSize からも修復要否を決める。
func TestDiagnoseLFSObjectsFallsBackToCacheSizeWithoutState(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	objects := []LFSObjectInfo{
		{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"asset.bin"}, Cached: true, CacheSize: 7},
		{OID: "sha256:" + strings.Repeat("b", 64), Size: 7, Paths: []string{"asset.bin"}, Cached: true, CacheSize: 3},
	}
	diagnostics, err := DiagnoseLFSObjects(repo, objects)
	if err != nil || len(diagnostics.Objects) != 1 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	if diagnostics.Objects[0].Object.OID != objects[1].OID {
		t.Fatalf("diagnostics=%+v", diagnostics)
	}
}

func TestDiagnoseLFSObjectsSkipsUnsafeAndDuplicatePaths(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	object := LFSObjectInfo{
		OID:        "sha256:" + strings.Repeat("a", 64),
		Size:       7,
		Paths:      []string{"../outside.bin", "asset.bin", "./asset.bin"},
		CacheState: LFSCacheMissing,
	}
	diagnostics, err := DiagnoseLFSObjects(repo, []LFSObjectInfo{object})
	if err != nil || len(diagnostics.Objects) != 1 {
		t.Fatalf("diagnostics=%+v err=%v", diagnostics, err)
	}
	if paths := diagnostics.Objects[0].CandidatePaths; len(paths) != 1 || paths[0] != "asset.bin" {
		t.Fatalf("candidate paths=%v", paths)
	}
}

func TestLFSRepairReportsUnopenableSourceAndCache(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("a", 64), Size: 1, Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	missingSource := discovery.Repository{
		ID:        "repo",
		MainPath:  domain.CanonicalPath(filepath.Join(base, "missing")),
		CommonDir: domain.CanonicalPath(base),
	}
	if _, err := (&Preparer{}).RepairLFSObjects(context.Background(), missingSource, []LFSObjectInfo{object}); err == nil {
		t.Fatal("repair succeeded for a missing source repository")
	}
	commonFile := filepath.Join(base, "common-file")
	if err := os.WriteFile(commonFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileCommon := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(base), CommonDir: domain.CanonicalPath(commonFile)}
	if _, err := (&Preparer{}).RepairLFSObjects(context.Background(), fileCommon, []LFSObjectInfo{object}); err == nil {
		t.Fatal("repair succeeded for a non-directory common directory")
	}
}

func TestLFSRepairRejectsNonHexOID(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("z", 64), Size: 7, Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(filepath.Join(common, "lfs")); !os.IsNotExist(statErr) {
		t.Fatalf("unexpected cache directory stat error=%v", statErr)
	}
}

func TestEnsureLFSCacheDirectoryRejectsUnsafeAndBlockedPaths(t *testing.T) {
	t.Parallel()
	common := t.TempDir()
	root, err := OpenPhysicalRoot(common)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	if err := ensureLFSCacheDirectory(root, "../escape"); err == nil {
		t.Fatal("unsafe cache directory was accepted")
	}
	if err := os.WriteFile(filepath.Join(common, "lfs"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLFSCacheDirectory(root, filepath.Join("lfs", "objects")); err == nil {
		t.Fatal("a regular file was accepted as a cache directory")
	}
	if err := os.Remove(filepath.Join(common, "lfs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(common, "lfs"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(common, "lfs"), 0o700) })
	if err := ensureLFSCacheDirectory(root, filepath.Join("lfs", "objects")); err == nil {
		t.Fatal("cache directory creation succeeded under a read-only parent")
	}
	if err := os.Chmod(filepath.Join(common, "lfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLFSCacheDirectory(root, filepath.Join("lfs", "objects")); err == nil {
		t.Fatal("cache directory inspection succeeded under an untraversable parent")
	}
}

// 診断が cache 欠落と判定した後に健全な object が現れた場合は、書き直さず成功として扱う。
func TestLFSRepairKeepsCacheObjectThatAppearedAfterDiagnosis(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("cached content")
	cached := []byte("stale contents")
	if len(data) != len(cached) {
		t.Fatal("test data must have matching sizes")
	}
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	destination := lfsCachePath(discovery.Repository{CommonDir: domain.CanonicalPath(common)}, oid)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, cached, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 0 || len(result.Repaired) != 0 {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != string(cached) {
		t.Fatalf("cache content=%q err=%v", got, err)
	}
}

func TestLFSRepairReportsUnreadableCacheDestination(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(common, "lfs", "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(objects, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0o700) })
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
}

func TestLFSRepairReportsUnwritableCacheDirectory(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	leaf := filepath.Dir(lfsCachePath(repo, oid))
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(leaf, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(leaf, 0o700) })
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
}

func TestLFSRepairReportsUncreatableCacheDirectory(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	if err := os.WriteFile(filepath.Join(source, "asset.bin"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	lfs := filepath.Join(common, "lfs")
	if err := os.Mkdir(lfs, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lfs, 0o700) })
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairWriteFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
}

func TestLFSRepairReportsUnreadableSourceCandidate(t *testing.T) {
	t.Parallel()
	source, common := t.TempDir(), t.TempDir()
	data := []byte("content")
	digest := sha256.Sum256(data)
	oid := "sha256:" + fmt.Sprintf("%x", digest[:])
	candidate := filepath.Join(source, "asset.bin")
	if err := os.WriteFile(candidate, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(candidate, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(candidate, 0o600) })
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(source), CommonDir: domain.CanonicalPath(common)}
	object := LFSObjectInfo{OID: oid, Size: int64(len(data)), Paths: []string{"asset.bin"}, CacheState: LFSCacheMissing}
	result, err := (&Preparer{}).RepairLFSObjects(context.Background(), repo, []LFSObjectInfo{object})
	if err != nil || len(result.Unresolved) != 1 || result.Unresolved[0].Reason != LFSRepairReadFailure {
		t.Fatalf("repair result=%+v err=%v", result, err)
	}
}

func TestVerifyLFSPathsAtRejectsMissingRootAndUnsafePaths(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	object := LFSObjectInfo{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"asset.bin"}}
	if err := VerifyLFSPathsAt(nil, ".", []LFSObjectInfo{object}); err == nil {
		t.Fatal("verification succeeded without a worktree root")
	}
	unsafe := LFSObjectInfo{OID: object.OID, Size: 7, Paths: []string{"../outside.bin"}}
	if err := VerifyLFSPathsAt(root, ".", []LFSObjectInfo{unsafe}); err == nil {
		t.Fatal("verification accepted a path outside the worktree")
	}
	if err := VerifyLFSPathsAt(root, ".", []LFSObjectInfo{object}); err == nil {
		t.Fatal("verification accepted a missing path")
	}
}

func TestPreparerVerifyPreparedLFSIgnoresOtherRepositories(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	preparer := &Preparer{LFSObjects: map[string][]LFSObjectInfo{
		"other": {{OID: "sha256:" + strings.Repeat("a", 64), Size: 7, Paths: []string{"missing.bin"}}},
	}}
	if err := preparer.verifyPreparedLFS(root, ".", discovery.Repository{ID: "repo"}); err != nil {
		t.Fatalf("verify prepared LFS: %v", err)
	}
}
