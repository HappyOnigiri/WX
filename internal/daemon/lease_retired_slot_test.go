package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 会話に記録された cwd が畳まれた slot を指すとき、同じ workspace で新しい worktree を貸し出すことを確かめる。
// resume では実体を失った worktree の path が渡るため、ここで解決できないと会話を再開する手段がなくなる。
func TestLeaseWithPolicyResolvesPathBelowRetiredSlot(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "cold"
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	registerTestWorkspace(t, store, w)

	first, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil || first.Path == "" {
		t.Fatalf("first lease=%+v err=%v", first, err)
	}
	// slot を畳んだ後の cwd を模す。path の実体はなく、slot の配下という位置だけが残る。
	retired := filepath.Join(first.Path, "gone", "repo")

	second, err := m.leaseWithPolicy(ctx, retired, nil, "codex", os.Getpid(), false)
	if err != nil || second.SessionID == "" {
		t.Fatalf("lease from retired slot path=%+v err=%v", second, err)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("retired slot path reused session %s", first.SessionID)
	}
}

// slot に紐づかない path は逆引きできないため、解決の失敗をそのまま返すことを確かめる。
func TestLeaseWithPolicyKeepsResolveErrorOutsideSlots(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	initGitRepo(t, repo)
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "cold"
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	m := testManager(t, cfg, store)
	defer m.Close()

	if _, err := m.leaseWithPolicy(context.Background(), filepath.Join(root, "missing"), nil, "codex", os.Getpid(), false); err == nil {
		t.Fatal("lease from an unknown path succeeded")
	}
}
