package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestCompactLFSObjectsReplacesVerifiedObject(t *testing.T) {
	t.Parallel()
	if !cowAvailable() {
		t.Skip("CoW is required")
	}
	root := t.TempDir()
	common := t.TempDir()
	worktree := filepath.Join(root, "slot", "repo")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("weight", 128))
	donorPath := filepath.Join(worktree, "weights.bin")
	if err := os.WriteFile(donorPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	value := hex.EncodeToString(hash[:])
	cacheDir := filepath.Join(common, "lfs", "objects", value[:2], value[2:4])
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, value)
	if err := os.WriteFile(cachePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := &Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg, RootPath: root, OwnedRoot: owner}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(root), CommonDir: domain.CanonicalPath(common), RelativePath: "."}
	result, err := preparer.CompactLFSObjects(context.Background(), repo, worktree, []LFSObjectCandidate{{
		Path: "weights.bin", Pointer: LFSPointer{OID: "sha256:" + value, Size: int64(len(data))},
	}})
	if err != nil || result.Replaced != 1 || result.ReclaimedBytes != int64(len(data)) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("verified LFS object was not atomically replaced")
	}
	if after.Mode().Perm() != 0o644 {
		t.Fatalf("cache mode=%o, want 644", after.Mode().Perm())
	}
	if got, err := os.ReadFile(cachePath); err != nil || string(got) != string(data) {
		t.Fatalf("cache bytes=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 || strings.HasPrefix(entries[0].Name(), cowTemporaryPrefix) {
		t.Fatalf("cache temporary entries=%v err=%v", entries, err)
	}
}

func TestVerifyLFSCloneChecksSizeAndHash(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Repeat("weight", 128))
	hash := sha256.Sum256(data)
	pointer := LFSPointer{OID: "sha256:" + hex.EncodeToString(hash[:]), Size: int64(len(data))}
	path := filepath.Join(t.TempDir(), "weights.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyLFSClone(context.Background(), file, pointer); err != nil {
		file.Close()
		t.Fatalf("matching LFS clone rejected: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong := pointer
	wrong.OID = "sha256:" + strings.Repeat("a", 64)
	if err := verifyLFSClone(context.Background(), file, wrong); !errors.Is(err, errLFSVerification) {
		file.Close()
		t.Fatalf("hash mismatch error=%v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	wrong = pointer
	wrong.Size++
	if err := verifyLFSClone(context.Background(), file, wrong); !errors.Is(err, errLFSVerification) {
		file.Close()
		t.Fatalf("size mismatch error=%v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCompactLFSObjectsLeavesCacheOnVerificationFailure(t *testing.T) {
	t.Parallel()
	if !cowAvailable() {
		t.Skip("CoW is required")
	}
	root := t.TempDir()
	common := t.TempDir()
	worktree := filepath.Join(root, "slot", "repo")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	donor := []byte(strings.Repeat("donor", 128))
	if err := os.WriteFile(filepath.Join(worktree, "weights.bin"), donor, 0o644); err != nil {
		t.Fatal(err)
	}
	wanted := sha256.Sum256([]byte(strings.Repeat("other", 128)))
	value := hex.EncodeToString(wanted[:])
	cacheDir := filepath.Join(common, "lfs", "objects", value[:2], value[2:4])
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(cacheDir, value)
	old := []byte(strings.Repeat("cache", 128))
	if err := os.WriteFile(cachePath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, cowTemporaryPrefix+"stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	preparer := &Preparer{Config: cfg, RootPath: root, OwnedRoot: owner}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(root), CommonDir: domain.CanonicalPath(common), RelativePath: "."}
	result, err := preparer.CompactLFSObjects(context.Background(), repo, worktree, []LFSObjectCandidate{{
		Path: "weights.bin", Pointer: LFSPointer{OID: "sha256:" + value, Size: int64(len(donor))},
	}})
	if err != nil || result.Replaced != 0 || result.Skipped != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := os.Stat(cachePath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("cache identity changed after verification failure: before=%v after=%v err=%v", before, after, err)
	}
	if got, err := os.ReadFile(cachePath); err != nil || string(got) != string(old) {
		t.Fatalf("cache bytes=%q err=%v", got, err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 || strings.HasPrefix(entries[0].Name(), cowTemporaryPrefix) {
		t.Fatalf("cache temporary entries=%v err=%v", entries, err)
	}
}

func TestCompactLFSObjectsSkipsWithoutCoW(t *testing.T) {
	t.Parallel()
	if cowAvailable() {
		t.Skip("Linux-only")
	}
	preparer := &Preparer{Config: config.Defaults()}
	repo := discovery.Repository{MainPath: "/repo", RelativePath: "."}
	result, err := preparer.CompactLFSObjects(context.Background(), repo, "/worktree", []LFSObjectCandidate{{
		Path:    "weights.bin",
		Pointer: LFSPointer{OID: "sha256:" + strings.Repeat("a", 64), Size: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 1 || result.Replaced != 0 || result.Failed != 0 {
		t.Fatalf("result=%+v", result)
	}
}

func TestCompactLFSObjectsHandlesEmptyAndNilPreparers(t *testing.T) {
	t.Parallel()
	var preparer *Preparer
	if result, err := preparer.CompactLFSObjects(context.Background(), discovery.Repository{}, "", nil); err != nil || result != (LFSCompactionResult{}) {
		t.Fatalf("empty candidates result=%+v err=%v", result, err)
	}
	_, err := preparer.CompactLFSObjects(context.Background(), discovery.Repository{}, "", []LFSObjectCandidate{{Path: "weights.bin"}})
	if err == nil || !strings.Contains(err.Error(), "requires a preparer") {
		t.Fatalf("nil preparer error=%v", err)
	}
}

func TestCompactLFSObjectsSkipsInCopyModeWithLog(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	cfg := config.Defaults()
	cfg.Storage.CopyMode = config.CopyModeCopy
	preparer := &Preparer{Config: cfg, Log: slog.New(slog.NewTextHandler(&logged, nil))}
	result, err := preparer.CompactLFSObjects(context.Background(), discovery.Repository{MainPath: "/repo", RelativePath: "."}, "/worktree", []LFSObjectCandidate{{Path: "weights.bin"}})
	if err != nil || result.Skipped != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(logged.String(), "reason=\"copy mode or unsupported platform\"") {
		t.Fatalf("log output=%q", logged.String())
	}
}

func TestLFSCacheRelativePartsMatchCapacityAndRepair(t *testing.T) {
	t.Parallel()
	value := strings.Repeat("ab", 32)
	directory, leaf, ok := lfsCacheRelativeParts("sha256:" + strings.ToUpper(value))
	if !ok {
		t.Fatal("valid SHA-256 object ID rejected")
	}
	want := filepath.Join("lfs", "objects", value[:2], value[2:4], value)
	if got := filepath.Join(directory, leaf); got != want {
		t.Fatalf("relative=%q, want %q", got, want)
	}
	repaired, ok := cacheLFSRelativePath("sha256:" + value)
	if !ok || repaired != want {
		t.Fatalf("repair relative=%q ok=%v, want %q", repaired, ok, want)
	}
	common := t.TempDir()
	repo := discovery.Repository{CommonDir: domain.CanonicalPath(common)}
	wantCache := filepath.Join(common, want)
	if got := lfsCachePath(repo, "sha256:"+value); got != wantCache {
		t.Fatalf("capacity path=%q, want %q", got, wantCache)
	}
	if _, _, ok := lfsCacheRelativeParts("sha256:" + strings.Repeat("z", 64)); ok {
		t.Fatal("non-hex object ID accepted")
	}
}

func TestLFSCompactionEnabledRequiresEligibleConfiguration(t *testing.T) {
	t.Parallel()
	if lfsCompactionEnabled(config.CopyModeCopy, true) {
		t.Fatal("copy mode unexpectedly enabled LFS CoW")
	}
	if !lfsCompactionEnabled(config.CopyModeAuto, true) {
		t.Fatal("available auto mode did not enable LFS CoW")
	}
	if lfsCompactionEnabled(config.CopyModeAuto, false) {
		t.Fatal("unsupported platform enabled LFS CoW")
	}
}

func TestLFSCompactionBatchAggregatesResults(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	preparer := &Preparer{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	failure := errors.New("test failure")
	candidates := []LFSObjectCandidate{
		{Path: "verified"},
		{Path: "failed"},
		{Path: "ineligible"},
		{Path: "replaced"},
	}
	compact := func(_ context.Context, _, _ *os.Root, candidate LFSObjectCandidate) (bool, int64, error) {
		switch candidate.Path {
		case "verified":
			return false, 0, fmt.Errorf("%w: test", errLFSVerification)
		case "failed":
			return false, 0, failure
		case "replaced":
			return true, 7, nil
		default:
			return false, 0, nil
		}
	}
	result, err := preparer.compactLFSBatch(context.Background(), nil, nil, candidates, compact)
	if !errors.Is(err, failure) {
		t.Fatalf("batch error=%v", err)
	}
	if result.Replaced != 1 || result.ReclaimedBytes != 7 || result.Skipped != 2 || result.Failed != 1 {
		t.Fatalf("batch result=%+v", result)
	}
	output := logged.String()
	for _, message := range []string{"LFS cache CoW skipped", "LFS cache CoW failed", "LFS cache CoW replacement"} {
		if !strings.Contains(output, message) {
			t.Fatalf("log output=%q missing %q", output, message)
		}
	}
}

func TestLFSCompactionBatchCountsCanceledCandidates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	preparer := &Preparer{}
	result, err := preparer.compactLFSBatch(ctx, nil, nil, []LFSObjectCandidate{{Path: "one"}, {Path: "two"}}, func(context.Context, *os.Root, *os.Root, LFSObjectCandidate) (bool, int64, error) {
		t.Fatal("canceled batch called compact function")
		return false, 0, nil
	})
	if !errors.Is(err, context.Canceled) || result.Failed != 2 {
		t.Fatalf("canceled result=%+v err=%v", result, err)
	}
}

// 先頭候補の後で中断した場合は、未処理候補だけを Failed として数える。
func TestLFSCompactionBatchCountsOnlyRemainingCanceledCandidates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	preparer := &Preparer{}
	result, err := preparer.compactLFSBatch(ctx, nil, nil, []LFSObjectCandidate{{Path: "one"}, {Path: "two"}}, func(_ context.Context, _, _ *os.Root, candidate LFSObjectCandidate) (bool, int64, error) {
		if candidate.Path == "one" {
			cancel()
			return true, 0, nil
		}
		t.Fatal("canceled batch called compact function for remaining candidate")
		return false, 0, nil
	})
	if !errors.Is(err, context.Canceled) || result.Replaced != 1 || result.Failed != 1 {
		t.Fatalf("canceled result=%+v err=%v, want one replaced and one failed", result, err)
	}
}

// 置換・skip・失敗を積み上げた後の中断では、未処理候補だけを Failed として数える。
func TestLFSCompactionBatchCountsCanceledCandidatesAfterEarlierResults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	preparer := &Preparer{}
	candidates := []LFSObjectCandidate{{Path: "replaced"}, {Path: "skipped"}, {Path: "failed"}, {Path: "pending"}}
	result, err := preparer.compactLFSBatch(ctx, nil, nil, candidates, func(_ context.Context, _, _ *os.Root, candidate LFSObjectCandidate) (bool, int64, error) {
		switch candidate.Path {
		case "replaced":
			return true, 0, nil
		case "skipped":
			return false, 0, errLFSVerification
		case "failed":
			cancel()
			return false, 0, errors.New("compaction failed")
		default:
			t.Fatal("canceled batch called compact function for remaining candidate")
			return false, 0, nil
		}
	})
	if err == nil || (!errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "compaction failed")) {
		t.Fatalf("canceled result error=%v", err)
	}
	if result.Replaced != 1 || result.Skipped != 1 || result.Failed != 2 {
		t.Fatalf("canceled result=%+v, want one replaced, one skipped, and two failed", result)
	}
}

func TestLFSCompactionLogsSkipAndFailure(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	preparer := &Preparer{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	candidate := LFSObjectCandidate{Path: "weights.bin", Pointer: LFSPointer{OID: "sha256:" + strings.Repeat("a", 64)}}
	preparer.logLFSCompactionSkip(candidate, errLFSNotEligible)
	preparer.logLFSCompactionFailure(candidate, errors.New("test failure"))
	output := logged.String()
	if !strings.Contains(output, "LFS cache CoW skipped") || !strings.Contains(output, "LFS cache CoW failed") {
		t.Fatalf("log output=%q", output)
	}
}
