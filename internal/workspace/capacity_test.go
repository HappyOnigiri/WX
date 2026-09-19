package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestParseLFSPointer(t *testing.T) {
	t.Parallel()
	valid := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("A", 64) + "\nsize 123\n"
	pointer, ok := ParseLFSPointer([]byte(valid))
	if !ok || pointer.OID != "sha256:"+strings.Repeat("a", 64) || pointer.Size != 123 {
		t.Fatalf("pointer=%+v ok=%v", pointer, ok)
	}
	zero := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("0", 64) + "\nsize 0\n"
	if pointer, ok := ParseLFSPointer([]byte(zero)); !ok || pointer.Size != 0 {
		t.Fatalf("zero-size pointer=%+v ok=%v", pointer, ok)
	}
	for _, invalid := range []string{
		"version https://git-lfs.github.com/spec/v1\noid sha256:bad\nsize 1\n",
		"version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize -1\n",
		"version https://example.invalid/v1\noid sha256:" + strings.Repeat("a", 64) + "\nsize 1\n",
	} {
		if _, ok := ParseLFSPointer([]byte(invalid)); ok {
			t.Fatalf("invalid pointer accepted: %q", invalid)
		}
	}
	if _, ok := parseLFSPointer([]byte(valid)); !ok {
		t.Fatal("unexported pointer parser rejected a valid pointer")
	}
}

// testlint:allow-serial -- t.SetenvでGit実行経路を一時差し替えるため
func TestEstimateCapacitySkipsAttributeLookupForEmptyTree(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.email", "wx@example.invalid")
	gitCommand(t, repository, "config", "user.name", "wx")
	gitCommand(t, repository, "commit", "--allow-empty", "-m", "empty")
	oid := capacityGitOutput(t, repository, "rev-parse", "HEAD")
	common := capacityGitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common), RelativePath: "."}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := "#!/bin/sh\n" +
		"if [ \"${1:-}\" = \"--no-optional-locks\" ]; then shift; fi\n" +
		"if [ \"${1:-}\" = \"check-attr\" ]; then exit 97; fi\n" +
		"exec \"$WX_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_TEST_REAL_GIT", realGit)
	t.Setenv("PATH", fmt.Sprintf("%s%c%s", bin, os.PathListSeparator, os.Getenv("PATH")))

	p := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: config.Defaults()}
	estimate, err := p.EstimateCapacity(context.Background(), repo, oid)
	if err != nil {
		t.Fatalf("empty tree estimate failed: %v", err)
	}
	if estimate.BlobBytes != 0 || estimate.WorktreeBytes != 0 || estimate.LFSObjects != 0 {
		t.Fatalf("empty tree estimate=%+v", estimate)
	}
}

func TestParseCapacityTreeAcceptsZeroSizedBlob(t *testing.T) {
	t.Parallel()
	entries, err := parseCapacityTree("100644 blob " + strings.Repeat("a", 40) + " 0\tzero\x00")
	if err != nil || len(entries) != 1 || entries[0].Path != "zero" || entries[0].Size != 0 {
		t.Fatalf("entries=%+v err=%v", entries, err)
	}
}

func TestParseLFSFilterPathsIgnoresIncompleteRecord(t *testing.T) {
	t.Parallel()
	paths := parseLFSFilterPaths("weights.bin\x00filter")
	if len(paths) != 0 {
		t.Fatalf("incomplete attributes produced paths=%v", paths)
	}
}

func TestParseLFSPointerBatchAcceptsZeroAndMaximumBlobSizes(t *testing.T) {
	t.Parallel()
	zeroOID := "zero"
	maximumOID := "maximum"
	pointerText := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("d", 64) + "\nsize 7\n"
	maximum := pointerText + strings.Repeat("x", maxLFSPointerBytes-len(pointerText))
	stdout := zeroOID + " blob 0\n\n" + maximumOID + " blob " + fmt.Sprint(maxLFSPointerBytes) + "\n" + maximum + "\n"
	pointers, err := parseLFSPointerBatch(stdout, []string{zeroOID, maximumOID}, false)
	if err != nil {
		t.Fatalf("batch parse failed: %v", err)
	}
	if _, ok := pointers[zeroOID]; ok {
		t.Fatalf("zero-sized non-pointer unexpectedly parsed: %v", pointers)
	}
	if pointer, ok := pointers[maximumOID]; !ok || pointer.OID != "sha256:"+strings.Repeat("d", 64) || pointer.Size != 7 {
		t.Fatalf("maximum-sized pointer=%+v ok=%v", pointer, ok)
	}
}

func TestEstimateCapacityUsesLFSPointerSizeAndMissingCache(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.email", "wx@example.invalid")
	gitCommand(t, repository, "config", "user.name", "wx")
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("b", 64) + "\nsize 123\n"
	if err := os.WriteFile(filepath.Join(repository, "large.bin"), []byte(pointer), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "lfs pointer")
	oid := capacityGitOutput(t, repository, "rev-parse", "HEAD")
	common := capacityGitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common), RelativePath: "."}
	cfg := config.Defaults()
	cfg.Storage.CopyMode = config.CopyModeCopy
	p := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}
	estimate, err := p.EstimateCapacity(context.Background(), repo, oid)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.LFSObjects != 1 || estimate.MissingLFSObjects != 1 || estimate.LFSExpandedBytes != 123 || estimate.LFSCacheBytes != 123 {
		t.Fatalf("estimate=%+v", estimate)
	}
	if len(estimate.LFS) != 1 || estimate.LFS[0].Size != 123 || estimate.LFS[0].Cached {
		t.Fatalf("LFS details=%+v", estimate.LFS)
	}
	if estimate.WorktreeBytes < 123 {
		t.Fatalf("worktree bytes=%d, want smudged content", estimate.WorktreeBytes)
	}
	alias, err := p.CapacityEstimate(context.Background(), repo, oid)
	if err != nil || alias.WorktreeBytes != estimate.WorktreeBytes {
		t.Fatalf("capacity alias=%+v err=%v", alias, err)
	}
}

func TestEstimateCapacityExcludesSkippedSparseLFSPaths(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.email", "wx@example.invalid")
	gitCommand(t, repository, "config", "user.name", "wx")
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, oid := range map[string]string{
		"inside/kept.bin":     strings.Repeat("a", 64),
		"outside/skipped.bin": strings.Repeat("b", 64),
	} {
		path := filepath.Join(repository, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 123\n"
		if err := os.WriteFile(path, []byte(pointer), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "sparse LFS pointers")
	gitCommand(t, repository, "sparse-checkout", "set", "--no-cone", "/inside/")
	head := capacityGitOutput(t, repository, "rev-parse", "HEAD")
	common := capacityGitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common), RelativePath: "."}
	p := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: config.Defaults()}
	estimate, err := p.EstimateCapacity(context.Background(), repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if !estimate.Sparse || estimate.LFSObjects != 1 || estimate.MissingLFSObjects != 1 || estimate.LFSCacheBytes != 123 {
		t.Fatalf("sparse estimate=%+v, want only materialized LFS object", estimate)
	}
	if len(estimate.LFS) != 1 || len(estimate.LFS[0].Paths) != 1 || estimate.LFS[0].Paths[0] != "inside/kept.bin" {
		t.Fatalf("sparse LFS details=%+v", estimate.LFS)
	}
}

func TestEstimateRootCopyBytesFollowsMaterializeRules(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "copy.txt"), []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "nested-link"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../copy.txt", filepath.Join(source, "nested-link", "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("copy.txt", filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	rules := RootRules{Copy: []string{"copy.txt", "nested", "copy.txt"}, OptionalCopy: []string{"missing", "linked", "nested"}}
	got, err := EstimateRootCopyBytes(source, rules)
	if err != nil || got != int64(len("copy")+len("nested")) {
		t.Fatalf("root copy bytes=%d err=%v", got, err)
	}
	if _, err := EstimateRootCopyBytes(source, RootRules{Copy: []string{"missing"}}); err == nil {
		t.Fatal("missing explicit root copy source was accepted")
	}
	if _, err := EstimateRootCopyBytes(source, RootRules{Copy: []string{"nested-link"}}); err == nil {
		t.Fatal("nested root copy symlink was accepted")
	}
	if _, err := EstimateRootCopyBytes(source, RootRules{Copy: []string{"../outside"}}); err == nil {
		t.Fatal("unsafe root copy source was accepted")
	}
}

func TestEstimateRootCopyBytesCountsOverlappingDestinationsOnce(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("nested"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := EstimateRootCopyBytes(source, RootRules{
		Copy:         []string{"nested/value"},
		OptionalCopy: []string{"nested"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(len("nested")); got != want {
		t.Fatalf("overlapping root copy bytes=%d, want %d", got, want)
	}
}

func TestCapacityHelpersHandleCacheModesAndOverflow(t *testing.T) {
	t.Parallel()
	if _, _, err := capacityCacheState(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(t.TempDir(), "object")
	if err := os.WriteFile(regular, []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cached, size, err := capacityCacheState(regular); err != nil || !cached || size != 6 {
		t.Fatalf("regular cache state=(%v,%d) err=%v", cached, size, err)
	}
	directory := t.TempDir()
	if cached, _, err := capacityCacheState(directory); err != nil || cached {
		t.Fatalf("directory cache state=(%v) err=%v", cached, err)
	}
	if got := addBytes(1, -1); got != int64(^uint64(0)>>1) {
		t.Fatalf("negative add=%d", got)
	}
	if got := addBytes(1, 0); got != 1 {
		t.Fatalf("zero add=%d", got)
	}
	if got := addBytes(int64(^uint64(0)>>1), 1); got != int64(^uint64(0)>>1) {
		t.Fatalf("overflow add=%d", got)
	}
	if got := lfsCachePath(discovery.Repository{CommonDir: "/tmp/common"}, "bad"); got != "/tmp/common/lfs/objects" {
		t.Fatalf("invalid LFS cache path=%q", got)
	}
	p := Preparer{Config: config.Defaults()}
	p.Config.Readiness.EarlyPaths = []string{"large.bin"}
	early := p.capacityEarlyPaths(discovery.Repository{MainPath: "/repo"}, []capacityTreeEntry{{Path: "large.bin"}, {Path: "src/.gitattributes"}, {Path: "src/other"}})
	if !early["large.bin"] || !early["src/.gitattributes"] || early["src/other"] {
		t.Fatalf("early paths=%v", early)
	}
	if p.capacityCOWEnabled(discovery.Repository{}, "false", map[string]bool{"path": true}, nil) {
		t.Fatal("conversion attributes unexpectedly enabled CoW")
	}
	p.Config.Storage.CopyMode = config.CopyModeCopy
	if p.capacityCOWEnabled(discovery.Repository{}, "false", nil, nil) {
		t.Fatal("copy mode unexpectedly enabled CoW")
	}
}

func TestRefreshLFSCacheStateKeepsEstimateUnchangedWhenInspectionFails(t *testing.T) {
	t.Parallel()
	healthyPath := filepath.Join(t.TempDir(), "healthy")
	if err := os.WriteFile(healthyPath, []byte("123"), 0o600); err != nil {
		t.Fatal(err)
	}
	estimate := CapacityEstimate{
		LFSObjects:        7,
		MissingLFSObjects: 8,
		LFSCacheBytes:     9,
		LFS: []LFSObjectInfo{
			{OID: "sha256:first", Size: 3, CachePath: healthyPath, CacheState: LFSCacheMissing},
			{OID: "sha256:second", Size: 4, CachePath: "\x00", CacheState: LFSCacheMissing},
		},
	}
	wantLFS := append([]LFSObjectInfo(nil), estimate.LFS...)
	wantObjects, wantMissing, wantCacheBytes := estimate.LFSObjects, estimate.MissingLFSObjects, estimate.LFSCacheBytes
	if err := RefreshLFSCacheState(&estimate); err == nil {
		t.Fatal("refresh with an invalid cache path succeeded")
	}
	if !reflect.DeepEqual(estimate.LFS, wantLFS) || estimate.LFSObjects != wantObjects || estimate.MissingLFSObjects != wantMissing || estimate.LFSCacheBytes != wantCacheBytes {
		t.Fatalf("failed refresh partially updated estimate=%+v, want LFS=%+v objects=%d missing=%d cache=%d", estimate, wantLFS, wantObjects, wantMissing, wantCacheBytes)
	}
}

func capacityGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	result, err := (&gitx.Runner{Timeout: 5 * time.Second}).Run(context.Background(), directory, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(result.Stdout)
}

// cache に object がある場合の分類（size 一致=healthy、不一致=corrupt）を、
// 修復側が根拠にする CacheState として確認する。
func TestEstimateCapacityClassifiesCachedLFSObjects(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.email", "wx@example.invalid")
	gitCommand(t, repository, "config", "user.name", "wx")
	if err := os.WriteFile(filepath.Join(repository, ".gitattributes"), []byte("*.bin filter=lfs\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	healthyOID, corruptOID := strings.Repeat("b", 64), strings.Repeat("c", 64)
	for name, oid := range map[string]string{"healthy.bin": healthyOID, "corrupt.bin": corruptOID} {
		pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 123\n"
		if err := os.WriteFile(filepath.Join(repository, name), []byte(pointer), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "lfs pointers")
	head := capacityGitOutput(t, repository, "rev-parse", "HEAD")
	common := capacityGitOutput(t, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common), RelativePath: "."}
	for oid, size := range map[string]int{healthyOID: 123, corruptOID: 7} {
		cached := lfsCachePath(repo, "sha256:"+oid)
		if err := os.MkdirAll(filepath.Dir(cached), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cached, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Storage.CopyMode = config.CopyModeCopy
	p := Preparer{Git: &gitx.Runner{Timeout: 5 * time.Second}, Config: cfg}
	estimate, err := p.EstimateCapacity(context.Background(), repo, head)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.LFSObjects != 2 || estimate.MissingLFSObjects != 1 {
		t.Fatalf("estimate=%+v", estimate)
	}
	states := map[string]LFSCacheState{}
	for _, object := range estimate.LFS {
		states[object.OID] = object.CacheState
	}
	if states["sha256:"+healthyOID] != LFSCacheHealthy || states["sha256:"+corruptOID] != LFSCacheCorrupt {
		t.Fatalf("LFS cache states=%v", states)
	}
}
