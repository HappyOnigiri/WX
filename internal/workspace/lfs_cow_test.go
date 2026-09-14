package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	cachePath := filepath.Join(cacheDir, value[4:])
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
	cachePath := filepath.Join(cacheDir, value[4:])
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
