package archive

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

func BenchmarkSnapshot(b *testing.B) {
	repository, repo, manager, _ := benchmarkArchive(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := manager.Snapshot(ctx, repo, repository, fmt.Sprintf("snapshot-%d", i), time.Now().Add(time.Hour)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRestore(b *testing.B) {
	repository, repo, manager, root := benchmarkArchive(b)
	ctx := context.Background()
	snapshot, err := manager.Snapshot(ctx, repo, repository, "restore-source", time.Now().Add(time.Hour))
	if err != nil {
		b.Fatal(err)
	}
	root = filepath.Join(root, "restores")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target := filepath.Join(root, fmt.Sprintf("slot-%d", i))
		if err := manager.Restore(ctx, repo, target, fmt.Sprintf("slot-%d", i), snapshot); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, err := manager.Git.Run(ctx, repository, "worktree", "unlock", target); err != nil {
			b.Fatal(err)
		}
		if _, err := manager.Git.Run(ctx, repository, "worktree", "remove", "--force", target); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func BenchmarkColdPrepare(b *testing.B) {
	repository, repo, manager, root := benchmarkArchive(b)
	ctx := context.Background()
	head := benchmarkGitCommand(b, repository, "rev-parse", "HEAD")
	root = filepath.Join(root, "prepared")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		target := filepath.Join(root, fmt.Sprintf("slot-%d", i))
		if err := manager.Preparer.Prepare(ctx, repo, target, head, fmt.Sprintf("slot-%d", i)); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if _, err := manager.Git.Run(ctx, repository, "worktree", "unlock", target); err != nil {
			b.Fatal(err)
		}
		if _, err := manager.Git.Run(ctx, repository, "worktree", "remove", "--force", target); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
}

func benchmarkArchive(b *testing.B) (string, discovery.Repository, *Manager, string) {
	b.Helper()
	repository := filepath.Join(b.TempDir(), "repository")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		b.Fatal(err)
	}
	benchmarkGitCommand(b, repository, "init", "-b", "main")
	benchmarkGitCommand(b, repository, "config", "user.name", "benchmark")
	benchmarkGitCommand(b, repository, "config", "user.email", "benchmark@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("content\n"), 0o600); err != nil {
		b.Fatal(err)
	}
	benchmarkGitCommand(b, repository, "add", ".")
	benchmarkGitCommand(b, repository, "commit", "-m", "initial")
	common := benchmarkGitCommand(b, repository, "rev-parse", "--path-format=absolute", "--git-common-dir")
	repo := discovery.Repository{ID: "repository", MainPath: domain.CanonicalPath(repository), CommonDir: domain.CanonicalPath(common), DefaultBranch: "main"}
	runner := &gitx.Runner{Timeout: 10 * time.Second}
	cfg := config.Defaults()
	worktreeRoot := filepath.Join(b.TempDir(), "worktrees")
	cfg.Storage.WorktreeRoot = worktreeRoot
	return repository, repo, &Manager{Git: runner, Preparer: &workspace.Preparer{Git: runner, Config: cfg, Ownership: allowOwnershipValidator{}}, Ownership: allowOwnershipValidator{}}, worktreeRoot
}

func benchmarkGitCommand(b *testing.B, directory string, arguments ...string) string {
	b.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		b.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}
