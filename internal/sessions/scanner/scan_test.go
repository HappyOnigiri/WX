package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/identity"
)

func copyFixture(t *testing.T, name, destination string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatalf("write fixture %q: %v", destination, err)
	}
}

func scanConfig(claudeRoot, codexRoot string) config.Config {
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{claudeRoot}
	cfg.Paths.Codex.Sessions = []string{codexRoot}
	return cfg
}

func TestScanClaudeAndCodexMetadata(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	codexRoot := filepath.Join(root, "codex")
	if err := os.MkdirAll(filepath.Join(claudeRoot, "subagents"), 0o700); err != nil {
		t.Fatalf("mkdir Claude fixture: %v", err)
	}
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatalf("mkdir Codex fixture: %v", err)
	}
	claudePath := filepath.Join(claudeRoot, "claude-native-001.jsonl")
	codexPath := filepath.Join(codexRoot, "rollout-2026-09-06T00-00-00-019e8bd5-4230-7403-b1aa-b48f42e564dc.jsonl")
	codexSubagentPath := filepath.Join(codexRoot, "rollout-2026-09-06T00-00-00-019ed787-436f-7d00-967d-25ad7a157502.jsonl")
	copyFixture(t, "claude/realistic.jsonl", claudePath)
	copyFixture(t, "codex/realistic.jsonl", codexPath)
	copyFixture(t, "claude/realistic.jsonl", filepath.Join(claudeRoot, "subagents", "ignored.jsonl"))
	subagent, err := os.ReadFile(filepath.Join("testdata", "codex", "realistic.jsonl"))
	if err != nil {
		t.Fatalf("read Codex subagent fixture: %v", err)
	}
	subagent = []byte(strings.Replace(string(subagent), `"originator":"codex-tui"`, `"thread_source":"subagent","originator":"codex-tui"`, 1))
	if err := os.WriteFile(codexSubagentPath, subagent, 0o600); err != nil {
		t.Fatalf("write Codex subagent fixture: %v", err)
	}
	if err := os.Chtimes(claudePath, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatalf("set Claude mtime: %v", err)
	}
	if err := os.Chtimes(codexPath, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
		t.Fatalf("set Codex mtime: %v", err)
	}

	sessions, err := Scan(context.Background(), scanConfig(claudeRoot, codexRoot))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("Scan returned %d sessions, want 2: %+v", len(sessions), sessions)
	}
	var claude, codex Session
	for _, session := range sessions {
		switch session.Tool {
		case "claude":
			claude = session
		case "codex":
			codex = session
		default:
			t.Fatalf("unexpected tool %q", session.Tool)
		}
	}
	if claude.SessionID != "claude-native-001" || claude.Title != "Implement authentication" || claude.CWD != "/Users/test/project" || claude.RawPath != claudePath {
		t.Errorf("Claude metadata = %+v", claude)
	}
	if claude.Mtime != 100 || claude.StableID != identity.ComputeSessionStableID("claude", claude.SessionID) {
		t.Errorf("Claude identity/mtime = %+v", claude)
	}
	if codex.SessionID != "019e8bd5-4230-7403-b1aa-b48f42e564dc" || codex.Title != "Fix the authentication bug" || codex.CWD != "/Users/test/project" || codex.RawPath != codexPath {
		t.Errorf("Codex metadata = %+v", codex)
	}
	if codex.Mtime != 200 || codex.StableID != identity.ComputeSessionStableID("codex", codex.SessionID) {
		t.Errorf("Codex identity/mtime = %+v", codex)
	}
	target := claude.Target()
	if !target.Resumable() || target.SourcePath != claudePath || target.Kind != "session" {
		t.Errorf("Claude target = %+v, want resumable session", target)
	}
	if (ResumeTarget{Tool: "cursor", Kind: "session", SessionID: "cursor-id"}).Resumable() {
		t.Fatal("non-supported tool unexpectedly resumable")
	}
}

func TestScanMalformedAndLongLines(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	codexRoot := filepath.Join(root, "codex")
	if err := os.MkdirAll(claudeRoot, 0o700); err != nil {
		t.Fatalf("mkdir Claude fixture: %v", err)
	}
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatalf("mkdir Codex fixture: %v", err)
	}
	claudePath := filepath.Join(claudeRoot, "malformed-001.jsonl")
	claudeContent := "not json\n{" + "\"type\":\"user\",\"cwd\":\"/tmp/claude\",\"message\":{\"content\":\"Recover Claude\"}}\n"
	if err := os.WriteFile(claudePath, []byte(claudeContent), 0o600); err != nil {
		t.Fatalf("write malformed Claude fixture: %v", err)
	}
	codexPath := filepath.Join(codexRoot, "rollout-2026-09-06T00-00-00-019e8bd5-4230-7403-b1aa-b48f42e564dc.jsonl")
	longLine := strings.Repeat("x", 256*1024)
	codexContent := longLine + "\n" + `{"type":"session_meta","payload":{"id":"019e8bd5-4230-7403-b1aa-b48f42e564dc","cwd":"/tmp/codex"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Recover Codex"}]}}` + "\n"
	if err := os.WriteFile(codexPath, []byte(codexContent), 0o600); err != nil {
		t.Fatalf("write long Codex fixture: %v", err)
	}

	sessions, err := Scan(context.Background(), scanConfig(claudeRoot, codexRoot))
	if err != nil {
		t.Fatalf("Scan malformed/long fixture: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("Scan returned %d sessions, want 2: %+v", len(sessions), sessions)
	}
	for _, session := range sessions {
		if session.Title == "" || session.CWD == "" || session.SessionID == "" {
			t.Errorf("incomplete metadata: %+v", session)
		}
	}
}

func TestScanDeduplicatesNativeIDsByLatestMtime(t *testing.T) {
	root := t.TempDir()
	firstRoot := filepath.Join(root, "first")
	secondRoot := filepath.Join(root, "second")
	if err := os.MkdirAll(firstRoot, 0o700); err != nil {
		t.Fatalf("mkdir first root: %v", err)
	}
	if err := os.MkdirAll(secondRoot, 0o700); err != nil {
		t.Fatalf("mkdir second root: %v", err)
	}
	firstPath := filepath.Join(firstRoot, "duplicate-native.jsonl")
	secondPath := filepath.Join(secondRoot, "duplicate-native.jsonl")
	first := `{"type":"user","cwd":"/tmp/old","message":{"content":"Old title"}}` + "\n"
	second := `{"type":"user","cwd":"/tmp/new","message":{"content":"New title"}}` + "\n"
	if err := os.WriteFile(firstPath, []byte(first), 0o600); err != nil {
		t.Fatalf("write first duplicate: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte(second), 0o600); err != nil {
		t.Fatalf("write second duplicate: %v", err)
	}
	if err := os.Chtimes(firstPath, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatalf("set first duplicate mtime: %v", err)
	}
	if err := os.Chtimes(secondPath, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
		t.Fatalf("set second duplicate mtime: %v", err)
	}
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{firstRoot, secondRoot, firstRoot}
	cfg.Paths.Codex.Sessions = []string{filepath.Join(root, "missing-codex")}

	sessions, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Scan duplicate roots: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Scan returned %d duplicate sessions, want 1: %+v", len(sessions), sessions)
	}
	if got := sessions[0]; got.Title != "New title" || got.CWD != "/tmp/new" || got.RawPath != secondPath || got.Mtime != 200 {
		t.Fatalf("deduplicated session = %+v, want latest duplicate", got)
	}
}

func TestScanCancellation(t *testing.T) {
	root := t.TempDir()
	cfg := scanConfig(filepath.Join(root, "claude"), filepath.Join(root, "codex"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Scan(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan(canceled) = %v, want context.Canceled", err)
	}
}

func TestScanRestrictsSourceToRequestedAgent(t *testing.T) {
	root := t.TempDir()
	cfg := scanConfig(root, filepath.Join(root, "not-directory", "history"))
	if err := os.WriteFile(filepath.Join(root, "not-directory"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFixture(t, "claude/realistic.jsonl", filepath.Join(root, "11111111-1111-4111-8111-111111111111.jsonl"))
	items, err := Scan(context.Background(), cfg, "claude")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
	if _, err := Scan(context.Background(), cfg, "cursor"); err == nil {
		t.Fatal("unsupported agent accepted")
	}
}
