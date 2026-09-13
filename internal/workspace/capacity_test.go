package workspace

import (
	"context"
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

func TestParseLFSPointer(t *testing.T) {
	t.Parallel()
	valid := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("A", 64) + "\nsize 123\n"
	pointer, ok := ParseLFSPointer([]byte(valid))
	if !ok || pointer.OID != "sha256:"+strings.Repeat("a", 64) || pointer.Size != 123 {
		t.Fatalf("pointer=%+v ok=%v", pointer, ok)
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

func capacityGitOutput(t *testing.T, directory string, args ...string) string {
	t.Helper()
	result, err := (&gitx.Runner{Timeout: 5 * time.Second}).Run(context.Background(), directory, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(result.Stdout)
}
