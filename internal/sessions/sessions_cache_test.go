package sessions

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
)

func historyConfig(t *testing.T, root string) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{root}
	cfg.Paths.Codex.Sessions = []string{filepath.Join(root, "absent-codex")}
	return cfg
}

func writeClaudeHistory(t *testing.T, root, id, title string) {
	t.Helper()
	body := `{"type":"user","cwd":"/workspace","message":{"content":"` + title + `"}}` + "\n"
	if err := os.WriteFile(filepath.Join(root, id+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestSessionsAgreeBetweenColdAndWarmCache は、cache の有無で一覧・順序・最新会話・ID 検索が変わらないことを確かめる。
func TestSessionsAgreeBetweenColdAndWarmCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := filepath.Join(home, "history")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := historyConfig(t, root)
	ids := []string{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"}
	for _, id := range ids {
		writeClaudeHistory(t, root, id, "履歴")
	}
	opts := PickOptions{Tool: "claude"}
	cold, err := list(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := list(context.Background(), cfg, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(cold) != 2 || len(warm) != len(cold) {
		t.Fatalf("cold=%d warm=%d sessions, want 2", len(cold), len(warm))
	}
	for i := range cold {
		if cold[i] != warm[i] {
			t.Fatalf("warm[%d] = %+v, want %+v", i, warm[i], cold[i])
		}
	}
	target, found, err := Lookup(context.Background(), cfg, "claude", ids[1])
	if err != nil || !found || target.SessionID != ids[1] {
		t.Fatalf("warm Lookup = %+v found=%v err=%v", target, found, err)
	}
}

// TestSessionsWorkWithoutAUsableCache は、cache の path を決められない環境でも走査へ戻ることを確かめる。
func TestSessionsWorkWithoutAUsableCache(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "history")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "33333333-3333-4333-8333-333333333333"
	writeClaudeHistory(t, root, id, "履歴")
	cfg := historyConfig(t, root)

	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	if items, err := list(context.Background(), cfg, PickOptions{Tool: "claude"}); err != nil || len(items) != 1 {
		t.Fatalf("without cache directory items=%d err=%v", len(items), err)
	}

	// cache directory を作れない path でも会話は欠落しない。
	blocked := filepath.Join(home, "blocked")
	if err := os.WriteFile(blocked, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocked)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(blocked, "cache"))
	target, found, err := Lookup(context.Background(), cfg, "claude", id)
	if err != nil || !found || target.SessionID != id {
		t.Fatalf("without writable cache target=%+v found=%v err=%v", target, found, err)
	}
}
