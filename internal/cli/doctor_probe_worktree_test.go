package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestParseGitlinksKeepsOnlySubmoduleEntries(t *testing.T) {
	output := strings.Join([]string{
		"100644 1111111111111111111111111111111111111111 0\ttracked",
		"160000 2222222222222222222222222222222222222222 0\tvendor/lib",
		"120000 3333333333333333333333333333333333333333 0\tlink",
		"",
	}, "\x00")
	gitlinks := parseGitlinks(output)
	if len(gitlinks) != 1 {
		t.Fatalf("gitlinks = %+v, want one", gitlinks)
	}
	if gitlinks[0].path != "vendor/lib" || gitlinks[0].oid != "2222222222222222222222222222222222222222" {
		t.Fatalf("gitlink = %+v", gitlinks[0])
	}
}

func TestParseGitlinksIgnoresMalformedEntries(t *testing.T) {
	if got := parseGitlinks("160000 oid\tvendor\x00not an entry\x00"); len(got) != 0 {
		t.Fatalf("gitlinks = %+v, want none", got)
	}
}

// index が commit を指しているのに実体が空の submodule は、準備完了時の tracked-status 検査を通り抜ける。
// この検査だけが、エージェントが submodule の中身を見られない worktree を捕まえられる。
func TestProbeSubmoduleFindingsReportEmptySubmoduleAsProblem(t *testing.T) {
	worktree := probeWorktreeFixture(t)
	if err := os.Mkdir(filepath.Join(worktree, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	probeGitCommand(t, worktree, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",vendor")
	findings := probeSubmoduleFindings(context.Background(), probeTestGit(), "/root", worktree)
	if len(findings) != 1 || findings[0].Severity != diag.SeverityProblem {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Check != diag.CheckProbeSubmodule || findings[0].Target != filepath.Join(worktree, "vendor") {
		t.Fatalf("finding = %+v", findings[0])
	}
	if !strings.Contains(findings[0].Cause, strings.Repeat("a", 40)) || findings[0].Action == "" {
		t.Fatalf("cause=%q action=%q", findings[0].Cause, findings[0].Action)
	}
}

func TestProbeSubmoduleFindingsAcceptPopulatedSubmodule(t *testing.T) {
	worktree := probeWorktreeFixture(t)
	if err := os.Mkdir(filepath.Join(worktree, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "vendor", "file"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeGitCommand(t, worktree, "update-index", "--add", "--cacheinfo", "160000,"+strings.Repeat("a", 40)+",vendor")
	findings := probeSubmoduleFindings(context.Background(), probeTestGit(), "/root", worktree)
	if len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
		t.Fatalf("findings = %+v", findings)
	}
}

// 準備は完了時に同じ検査を通しているため、ここでの差分は準備後に worktree が書き換わったことを意味する。
func TestProbeTrackedFindingsReportModifiedTrackedFile(t *testing.T) {
	worktree := probeWorktreeFixture(t)
	clean := probeTrackedFindings(context.Background(), probeTestGit(), worktree)
	if clean.Severity != diag.SeverityOK {
		t.Fatalf("clean worktree = %+v", clean)
	}
	if err := os.WriteFile(filepath.Join(worktree, "tracked"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirty := probeTrackedFindings(context.Background(), probeTestGit(), worktree)
	if dirty.Severity != diag.SeverityProblem || dirty.Check != diag.CheckProbeTracked {
		t.Fatalf("dirty worktree = %+v", dirty)
	}
	if !strings.Contains(dirty.Cause, "tracked") || dirty.Action == "" {
		t.Fatalf("cause=%q action=%q", dirty.Cause, dirty.Action)
	}
}

// 単一リポジトリ workspace の貸出 path は worktree そのもので、multi_repository ではその直下に並ぶ。
func TestProbeWorktreesFindBothLeaseLayouts(t *testing.T) {
	single := probeWorktreeFixture(t)
	found, err := probeWorktrees(context.Background(), probeTestGit(), single)
	if err != nil || len(found) != 1 || found[0] != single {
		t.Fatalf("single repository lease = %+v: %v", found, err)
	}
	multi := t.TempDir()
	first := probeWorktreeFixtureAt(t, filepath.Join(multi, "alpha"))
	second := probeWorktreeFixtureAt(t, filepath.Join(multi, "beta"))
	if err := os.WriteFile(filepath.Join(multi, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	found, err = probeWorktrees(context.Background(), probeTestGit(), multi)
	if err != nil || len(found) != 2 || found[0] != first || found[1] != second {
		t.Fatalf("multi repository lease = %+v: %v", found, err)
	}
}

// Git worktree を1つも持たない貸出は、準備が成功したと報告されていても使えない。
func TestProbeWorktreeFindingsReportLeaseWithoutWorktree(t *testing.T) {
	client := Client{}
	client.Config.Readiness.Timeout.Duration = 30 * time.Second
	findings := client.probeWorktreeFindings(context.Background(), "/root", t.TempDir())
	if len(findings) != 1 || findings[0].Severity != diag.SeverityProblem {
		t.Fatalf("findings = %+v", findings)
	}
	if findings[0].Cause == "" || findings[0].Action == "" {
		t.Fatalf("finding = %+v", findings[0])
	}
}

func probeTestGit() *gitx.Runner {
	return &gitx.Runner{Timeout: 30 * time.Second}
}

func probeWorktreeFixture(t *testing.T) string {
	t.Helper()
	return probeWorktreeFixtureAt(t, filepath.Join(t.TempDir(), "repository"))
}

func probeWorktreeFixtureAt(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	probeGitCommand(t, path, "init", "-b", "main")
	probeGitCommand(t, path, "config", "user.name", "test")
	probeGitCommand(t, path, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(path, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	probeGitCommand(t, path, "add", ".")
	probeGitCommand(t, path, "commit", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func probeGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
