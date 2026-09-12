package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

func TestWorktreeLinksRespectDestinationIgnoreRule(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	worktreeRoot := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	hooksPath := filepath.Join(base, "hooks")
	if err := os.Mkdir(hooksPath, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "config", "core.hooksPath", hooksPath)
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("shared/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "directory ignore")
	oldHead := gitOutput(t, repository, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".gitignore")
	gitCommand(t, repository, "commit", "-m", "symlink ignore")
	currentHead := gitOutput(t, repository, "rev-parse", "HEAD")
	if err := os.Mkdir(filepath.Join(repository, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "shared", "value"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTarget := filepath.Join(worktreeRoot, "old")
	currentTarget := filepath.Join(worktreeRoot, "current")
	gitCommand(t, repository, "worktree", "add", "--detach", oldTarget, oldHead)
	gitCommand(t, repository, "worktree", "add", "--detach", currentTarget, currentHead)
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	if err := os.Symlink(filepath.Join(repository, "shared"), filepath.Join(oldTarget, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinksAt(context.Background(), repo, owner, "old", true); err != nil {
		t.Fatalf("directory-only destination ignore: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(oldTarget, "shared")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory-only ignore materialized a symlink: %v", err)
	}
	if err := preparer.createLinksAt(context.Background(), repo, owner, "current", true); err != nil {
		t.Fatalf("symlink destination ignore: %v", err)
	}
	link, err := os.Readlink(filepath.Join(currentTarget, "shared"))
	if err != nil || link != filepath.Join(repository, "shared") {
		t.Fatalf("symlink destination link=%q err=%v", link, err)
	}
}

func TestWorktreeLinksSkipMissingSourcesAndTrackPresence(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	worktreeRoot := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("appearing-file\nappearing-dir/\npresent-dir/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("missing-file\nmissing-dir/child\nappearing-file\nappearing-dir\npresent-dir\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "present-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "present-dir", "value"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(worktreeRoot, "slot")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}

	before, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("missing worktree links should be skipped: %v", err)
	}
	for _, name := range []string{"missing-file", "missing-dir", "appearing-file", "appearing-dir"} {
		if _, err := os.Lstat(filepath.Join(target, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("missing link %s touched destination: %v", name, err)
		}
	}
	link, err := os.Readlink(filepath.Join(target, "present-dir"))
	if err != nil || link != filepath.Join(repository, "present-dir") {
		t.Fatalf("existing directory link=%q err=%v", link, err)
	}

	if err := os.WriteFile(filepath.Join(repository, "present-dir", "value"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || unchanged != before {
		t.Fatalf("link source contents changed fingerprint before=%s after=%s err=%v", before, unchanged, err)
	}
	if err := os.WriteFile(filepath.Join(repository, "appearing-file"), []byte("file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repository, "appearing-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || after == before {
		t.Fatalf("link source presence did not change fingerprint before=%s after=%s err=%v", before, after, err)
	}
	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("present ignored links should be created: %v", err)
	}
	for _, name := range []string{"appearing-file", "appearing-dir"} {
		link, err := os.Readlink(filepath.Join(target, name))
		if err != nil || link != filepath.Join(repository, name) {
			t.Fatalf("appearing %s link=%q err=%v", name, link, err)
		}
	}
}
