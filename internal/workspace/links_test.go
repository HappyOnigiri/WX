package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
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
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot, Log: logger}
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
	// 省略の理由が daemon log に残らないと、利用者は `dir/` 形の rule が原因だと辿れない。
	if !strings.Contains(logged.String(), "not ignored by the destination worktree") || !strings.Contains(logged.String(), "shared") {
		t.Fatalf("destination ignore skip was not logged: %q", logged.String())
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

// TestWorktreeLinksExpandGlobPatterns は事故の回帰を押さえる。
// `.claude/skills/local-*/` のような行は展開前には「そういう名前の path が無い」として無言で skip され、
// symlink が 1 つも張られなかった。展開後は各 match が link になり、fingerprint も match 集合の増減で動く。
// 併せて、展開後の path が配置先で ignore されない場合に理由がパス付きで log へ残ることも確かめる。
// commentlint:allow-long -- 修正前の症状と受け入れ条件を 1 箇所に残す
func TestWorktreeLinksExpandGlobPatterns(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	worktreeRoot := filepath.Join(base, "worktrees")
	if err := os.MkdirAll(filepath.Join(repository, ".claude", "skills", "tracked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(worktreeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, ".claude", "skills", "tracked", "SKILL.md"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "tracked skill")
	head := gitOutput(t, repository, "rev-parse", "HEAD")
	// `shared/` は directory 限定形なので wx が置く symlink を ignore できない。事故当時の構成をそのまま再現する。
	exclude := ".claude/skills/local-*\n.claude/skills/shared/\n"
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte(exclude), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte(".claude/skills/local-*/\n.claude/skills/shared/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"local-a", "local-b", "shared"} {
		if err := os.Mkdir(filepath.Join(repository, ".claude", "skills", name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repository, ".claude", "skills", name, "SKILL.md"), []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(worktreeRoot, "slot")
	gitCommand(t, repository, "worktree", "add", "--detach", target, head)
	owner, _, err := domain.OpenOwnedRoot(worktreeRoot, worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = worktreeRoot
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	preparer := Preparer{Git: &gitx.Runner{Timeout: 10 * time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot, Log: logger}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}

	before, err := Fingerprint(1, head, repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := preparer.createLinksAt(context.Background(), repo, owner, "slot", true); err != nil {
		t.Fatalf("glob .worktreelink was rejected: %v", err)
	}
	for _, name := range []string{"local-a", "local-b"} {
		path := filepath.Join(target, ".claude", "skills", name)
		link, readErr := os.Readlink(path)
		if readErr != nil || link != filepath.Join(repository, ".claude", "skills", name) {
			t.Fatalf("glob match %s link=%q err=%v", name, link, readErr)
		}
	}
	if _, err := os.Lstat(filepath.Join(target, ".claude", "skills", "shared")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory-only exclude materialized a symlink: %v", err)
	}
	if !strings.Contains(logged.String(), "not ignored by the destination worktree in link form") || !strings.Contains(logged.String(), ".claude/skills/shared") {
		t.Fatalf("destination ignore skip was not logged with its path: %q", logged.String())
	}
	if content, err := os.ReadFile(filepath.Join(target, ".claude", "skills", "tracked", "SKILL.md")); err != nil || string(content) != "tracked\n" {
		t.Fatalf("tracked skill was not checked out content=%q: %v", content, err)
	}

	// link の内容は hash に入らないので、match の内容だけを変えても fingerprint は動かない。
	if err := os.WriteFile(filepath.Join(repository, ".claude", "skills", "local-a", "SKILL.md"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if unchanged, err := Fingerprint(1, head, repo, cfg); err != nil || unchanged != before {
		t.Fatalf("link content changed fingerprint before=%s after=%s err=%v", before, unchanged, err)
	}
	// match 集合が増えると fingerprint は動き、再準備で link が増える。
	if err := os.Mkdir(filepath.Join(repository, ".claude", "skills", "local-c"), 0o700); err != nil {
		t.Fatal(err)
	}
	grown, err := Fingerprint(1, head, repo, cfg)
	if err != nil || grown == before {
		t.Fatalf("new glob match did not change fingerprint before=%s after=%s err=%v", before, grown, err)
	}
	if err := preparer.createLinksAt(context.Background(), repo, owner, "slot", true); err != nil {
		t.Fatalf("re-preparing with a new match failed: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(target, ".claude", "skills", "local-c")); err != nil || link != filepath.Join(repository, ".claude", "skills", "local-c") {
		t.Fatalf("new glob match link=%q err=%v", link, err)
	}
	// match 集合が減っても動く。match 外の追加では動かない。
	if err := os.RemoveAll(filepath.Join(repository, ".claude", "skills", "local-c")); err != nil {
		t.Fatal(err)
	}
	if shrunk, err := Fingerprint(1, head, repo, cfg); err != nil || shrunk != before {
		t.Fatalf("removing a glob match did not restore fingerprint before=%s after=%s err=%v", before, shrunk, err)
	}
	if err := os.Mkdir(filepath.Join(repository, ".claude", "skills", "other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if outside, err := Fingerprint(1, head, repo, cfg); err != nil || outside != before {
		t.Fatalf("a path outside the glob changed fingerprint before=%s after=%s err=%v", before, outside, err)
	}
}

// TestWorktreeLinksHandleGlobMatchEdges は、展開後の 1 件の性質で prepare 全体の可否が決まる境界を固定する。
// symlink に当たった match は warn 付きで skip して続行し、配置先に実体がある match は target collision で失敗する。
func TestWorktreeLinksHandleGlobMatchEdges(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repository, ".gitignore"), []byte("link-*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("link-*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "link-real"), []byte("real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repository, "link-real"), filepath.Join(repository, "link-sym")); err != nil {
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
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))
	preparer := Preparer{Git: &gitx.Runner{Timeout: 10 * time.Second}, Config: cfg, OwnedRoot: owner, RootPath: worktreeRoot, Log: logger}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}

	if err := preparer.createLinks(context.Background(), repo, target); err != nil {
		t.Fatalf("a symlink match must not fail the preparation: %v", err)
	}
	if link, err := os.Readlink(filepath.Join(target, "link-real")); err != nil || link != filepath.Join(repository, "link-real") {
		t.Fatalf("regular match link=%q err=%v", link, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "link-sym")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink match was materialized: %v", err)
	}
	if !strings.Contains(logged.String(), ".worktreelink source is a symlink") || !strings.Contains(logged.String(), "link-sym") {
		t.Fatalf("symlink match skip was not logged with its path: %q", logged.String())
	}

	collision := filepath.Join(worktreeRoot, "collision")
	if err := os.Mkdir(collision, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(collision, "link-real"), []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = preparer.createLinks(context.Background(), repo, collision)
	if err == nil || !strings.Contains(err.Error(), "target collision") || !strings.Contains(err.Error(), "link-real") {
		t.Fatalf("an existing entry at an expanded match must fail with a named collision: %v", err)
	}
}
