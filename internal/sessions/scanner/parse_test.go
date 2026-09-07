package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name, body, tool string) fileMeta {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	meta, err := readMetadata(context.Background(), file, path, tool)
	if err != nil {
		t.Fatalf("readMetadata %s: %v", name, err)
	}
	return meta
}

func TestReadMetadataSkipsNoiseAndFallsBackToSlashCommands(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"user","isMeta":true,"cwd":"/workspace","message":{"content":"メタ行"}}`,
		`{"type":"user","cwd":"/workspace","message":{"content":"<local-command-stdout>ignored"}}`,
		`{"type":"user","cwd":"/workspace","message":{"content":"/compact"}}`,
	}, "\n") + "\n"
	meta := readFixture(t, "slash.jsonl", body, "claude")
	if meta.title != "/compact" || meta.cwd != "/workspace" {
		t.Fatalf("slash-command fallback meta = %+v", meta)
	}

	titled := readFixture(t, "ai-title.jsonl", `{"type":"ai-title","cwd":"/workspace","aiTitle":"AI が付けた題\n二行目"}`+"\n", "claude")
	if titled.title != "AI が付けた題" {
		t.Fatalf("ai-title meta = %+v", titled)
	}
}

func TestReadMetadataTruncatesLongTitlesAndAcceptsTextParts(t *testing.T) {
	long := strings.Repeat("あ", maxTitleRunes+10)
	meta := readFixture(t, "long.jsonl", `{"type":"user","cwd":"/w","message":{"content":[{"type":"text","text":"`+long+`"}]}}`+"\n", "claude")
	if len([]rune(meta.title)) != maxTitleRunes {
		t.Fatalf("title runes = %d, want %d", len([]rune(meta.title)), maxTitleRunes)
	}
}

func TestReadMetadataUnwrapsCodexWrappersAndSubagents(t *testing.T) {
	body := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"codex-native","cwd":"/tmp/codex","thread_source":"subagent"}}`,
		`{"type":"response_item","payload":{"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"<user_query>包まれた依頼</user_query>"}]}}}`,
	}, "\n") + "\n"
	meta := readFixture(t, "codex-wrapped.jsonl", body, "codex")
	if meta.nativeID != "codex-native" || meta.title != "包まれた依頼" || !meta.subagent {
		t.Fatalf("codex meta = %+v", meta)
	}

	turnContext := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"codex-native"}}`,
		`{"type":"turn_context","payload":{"cwd":"/tmp/turn"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":"assistant は題にしない"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":"<codex_internal_context><objective>目的</objective></codex_internal_context>"}}`,
	}, "\n") + "\n"
	meta = readFixture(t, "codex-turn.jsonl", turnContext, "codex")
	if meta.cwd != "/tmp/turn" || meta.title != "目的" {
		t.Fatalf("turn_context meta = %+v", meta)
	}
}

func TestCodexIDFromPathAcceptsOnlyUUIDSuffixes(t *testing.T) {
	for path, want := range map[string]string{
		"/h/rollout-2026-09-06T00-00-00-019e8bd5-4230-7403-b1aa-b48f42e564dc.jsonl": "019e8bd5-4230-7403-b1aa-b48f42e564dc",
		"/h/rollout-short.jsonl": "",
		"/h/rollout-2026-09-06T00-00-00-019e8bd5-4230-7403-b1aa-zzzzzzzzzzzz.jsonl": "",
		"/h/rollout-2026-09-06T00-00-00-019e8bd5-4230-7403-b1aa-b48f42e5.jsonl":     "",
	} {
		if got := codexIDFromPath(path); got != want {
			t.Errorf("codexIDFromPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestXMLBlockAndExpandHomeHandleMissingInput(t *testing.T) {
	for _, tc := range []struct{ text, tag, want string }{
		{"<user_query>本文</user_query>", "user_query", "本文"},
		{"閉じていない<user_query>本文", "user_query", ""},
		{"開始がない", "user_query", ""},
		{"<user_query", "user_query", ""},
	} {
		if got := xmlBlock(tc.text, tc.tag); got != tc.want {
			t.Errorf("xmlBlock(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q, want %q", got, home)
	}
	if got := expandHome("~/history"); got != filepath.Join(home, "history") {
		t.Errorf("expandHome(~/history) = %q", got)
	}
	if got := expandHome("/absolute"); got != "/absolute" {
		t.Errorf("expandHome(/absolute) = %q", got)
	}
}
