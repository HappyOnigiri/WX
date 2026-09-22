package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeConfigFile は HOME 配下の設定ファイルへ document を書き、その path を返す。
func writeConfigFile(t *testing.T, document string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 未知キーの検出は yaml.v3 のエラー文言に依存するため、取りこぼすと「報告されない」という無言の失敗になる。
// トップレベル・既知節の下・動的キーの下の3形をここで押さえる。
func TestLoadRawReportsUnknownKeysWithTheirLines(t *testing.T) {
	document := "version: 2\nsystem:\n  lanugage: ja\n  pool:\n    preparation_concurrency: 2\n    worm_per_workspace: 3\nworkspaces:\n  demo:\n    worktree: hot\n    bogus: 1\n"
	writeConfigFile(t, document)
	raw, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	// 未知キーの隣に書かれた既知キーの値は落とさない。
	if raw.System.Pool.PreparationConcurrency != 2 {
		t.Fatalf("preparation_concurrency=%d, want 2", raw.System.Pool.PreparationConcurrency)
	}
	if got := raw.Workspaces["demo"].Worktree; got != "hot" {
		t.Fatalf("workspaces.demo.worktree=%q, want hot", got)
	}
	want := []UnknownKey{
		{Key: "system.lanugage", Line: 3},
		{Key: "system.pool.worm_per_workspace", Line: 6},
		{Key: "workspaces.demo.bogus", Line: 10},
	}
	got := raw.UnknownKeys()
	if len(got) != len(want) {
		t.Fatalf("unknown keys=%+v, want %+v", got, want)
	}
	for _, key := range want {
		if !slices.Contains(got, key) {
			t.Fatalf("unknown keys=%+v, want to contain %+v", got, key)
		}
	}
}

// 未知キーがあっても実効設定は作れ、CLIもdaemonも起動できる。未知キーは Merge 後も残す。
func TestLoadKeepsUnknownKeysInTheEffectiveConfig(t *testing.T) {
	writeConfigFile(t, "version: 2\nsystem:\n  lanugage: ja\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.UnknownKeys(); len(got) != 1 || got[0].Key != "system.lanugage" || got[0].Line != 3 {
		t.Fatalf("unknown keys=%+v", got)
	}
	// 未知キーの有無は実効値ではないため、reload の差分判定に影響させない。
	stripped := cfg
	stripped.unknown = nil
	if !cfg.EffectiveEqual(stripped) {
		t.Fatal("an unknown key changed the effective configuration")
	}
}

// 型の不一致や重複 document は値として解釈できないため、従来どおり load を失敗させる。
func TestLoadRawStillRejectsUninterpretableValues(t *testing.T) {
	writeConfigFile(t, "version: 2\nsystem:\n  pool:\n    preparation_concurrency: two\n")
	if _, err := LoadRaw(); err == nil {
		t.Fatal("a malformed value was accepted")
	}
}

// 保存で未知キーを消すと、旧版のwxで `wx config set` を通しただけで新版の設定が失われる。
func TestSaveKeepsUnknownKeysInPlace(t *testing.T) {
	document := "version: 2\nsystem:\n  lanugage: ja\n  pool:\n    workers: 4\nreporting:\n  level: verbose\n  targets:\n    - a\n    - b\n"
	path := writeConfigFile(t, document)
	raw, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw: %v", err)
	}
	if err := SetV2Field(&raw, V2ScopeSystem, "", "", "pool.preparation_concurrency", "3"); err != nil {
		t.Fatalf("SetV2Field: %v", err)
	}
	if err := Save(raw); err != nil {
		t.Fatalf("Save: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 既存の節の中の未知キーと、節ごと未知の場合の両方を保つ。後者は出力に無い親を作る経路になる。
	for _, want := range []string{"lanugage: ja", "reporting:", "level: verbose", "- a", "- b", "preparation_concurrency: 3"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved configuration does not contain %q:\n%s", want, saved)
		}
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw after Save: %v", err)
	}
	if got := reloaded.UnknownKeys(); len(got) != 3 {
		t.Fatalf("unknown keys after Save=%+v, want lanugage, reporting.level and reporting.targets", got)
	}
	if reloaded.System.Pool.PreparationConcurrency != 3 {
		t.Fatalf("preparation_concurrency=%d, want 3", reloaded.System.Pool.PreparationConcurrency)
	}
}

// 文言依存を1箇所へ閉じたので、そのfunctionの解析をここで固定する。
func TestParseUnknownField(t *testing.T) {
	for _, test := range []struct {
		name, message, field string
		line                 int
		ok                   bool
	}{
		{name: "unknown field", message: "line 2: field language not found in type config.SystemConfig", field: "language", line: 2, ok: true},
		{name: "name with a space", message: "line 12: field odd key not found in type config.Pool", field: "odd key", line: 12, ok: true},
		{name: "type mismatch", message: "line 3: cannot unmarshal !!str `two` into int"},
		{name: "duplicate field", message: "line 4: field pool already set in type config.Config"},
	} {
		t.Run(test.name, func(t *testing.T) {
			line, field, ok := parseUnknownField(test.message)
			if ok != test.ok || field != test.field || line != test.line {
				t.Fatalf("parseUnknownField(%q)=(%d, %q, %v), want (%d, %q, %v)", test.message, line, field, ok, test.line, test.field, test.ok)
			}
		})
	}
}

// commitConfigEdit は設定ファイルへ1件の編集を preview 経由で保存する。
// 未知キーの差し戻しは Save の経路でしか起きないため、テストも同じ経路を通す。
func commitConfigEdit(t *testing.T, request EditRequest) {
	t.Helper()
	preview, err := PreviewEdit(request)
	if err != nil {
		t.Fatalf("PreviewEdit(%+v): %v", request, err)
	}
	if err := CommitEdit(preview); err != nil {
		t.Fatalf("CommitEdit(%+v): %v", request, err)
	}
}

// readConfigDocument は保存された設定ファイルを、YAML の mapping 構造のまま読む。
func readConfigDocument(t *testing.T) (string, map[string]any) {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("saved config is not loadable YAML: %v\n%s", err, data)
	}
	return string(data), document
}

// ドットを含む workspace root の下の未知キーは、ドット連結したキーを分割し直すと
// 別の mapping へ移る。無関係な編集を保存しても書かれた階層に留まることを固定する。
func TestSaveKeepsUnknownKeysUnderDottedMappingKeys(t *testing.T) {
	writeConfigFile(t, "version: 2\nsystem:\n  pool:\n    preparation_concurrency: 2\nworkspaces:\n  \"$HOME/project.v2\":\n    worktree: hot\n    future:\n      note: keep-me\n")
	commitConfigEdit(t, EditRequest{Scope: V2ScopeSystem, V2: true, Key: "pool.preparation_concurrency", Value: "3", Operation: EditSet})
	_, document := readConfigDocument(t)
	workspaces, ok := document["workspaces"].(map[string]any)
	if !ok || len(workspaces) != 1 {
		t.Fatalf("workspaces=%#v, want the single dotted root", document["workspaces"])
	}
	workspace, ok := workspaces["$HOME/project.v2"].(map[string]any)
	if !ok {
		t.Fatalf("workspaces=%#v, want key $HOME/project.v2", workspaces)
	}
	future, ok := workspace["future"].(map[string]any)
	if !ok || future["note"] != "keep-me" {
		t.Fatalf("workspaces[$HOME/project.v2]=%#v, want future.note=keep-me", workspace)
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw after Save: %v", err)
	}
	if got := reloaded.UnknownKeys(); len(got) != 1 || got[0].Key != "workspaces.$HOME/project.v2.future" {
		t.Fatalf("unknown keys after Save=%+v", got)
	}
}

// 既知キーは Go 値から組み直すため anchor 定義が出力に残らない。未知 alias を
// そのまま書き戻すと保存した設定を次回読めなくなるので、展開して自己完結させる。
func TestSaveMaterializesUnknownAliasValues(t *testing.T) {
	writeConfigFile(t, "version: 2\nsystem:\n  language: &chosen_language en\n  future_note: *chosen_language\nworkspace_defaults:\n  copy: &shared_copy\n    - a.txt\n  future_copy: *shared_copy\n")
	commitConfigEdit(t, EditRequest{Scope: V2ScopeSystem, V2: true, Key: "pool.preparation_concurrency", Value: "3", Operation: EditSet})
	data, document := readConfigDocument(t)
	if strings.Contains(data, "*chosen_language") || strings.Contains(data, "*shared_copy") {
		t.Fatalf("saved config still references anchors:\n%s", data)
	}
	if system, ok := document["system"].(map[string]any); !ok || system["future_note"] != "en" {
		t.Fatalf("system=%#v, want future_note=en", document["system"])
	}
	defaults, ok := document["workspace_defaults"].(map[string]any)
	if !ok {
		t.Fatalf("workspace_defaults=%#v", document["workspace_defaults"])
	}
	if copies, ok := defaults["future_copy"].([]any); !ok || len(copies) != 1 || copies[0] != "a.txt" {
		t.Fatalf("workspace_defaults=%#v, want future_copy=[a.txt]", defaults)
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatalf("LoadRaw after Save: %v", err)
	}
	if got := reloaded.UnknownKeys(); len(got) != 2 {
		t.Fatalf("unknown keys after Save=%+v, want system.future_note and workspace_defaults.future_copy", got)
	}
}

func TestResolveUnknownAliasesKeepsEmptyNodeContentNil(t *testing.T) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Value: "empty"}
	got := resolveUnknownAliases(node, map[*yaml.Node]bool{})
	if got.Content != nil {
		t.Fatalf("empty node content=%#v, want nil", got.Content)
	}
}
