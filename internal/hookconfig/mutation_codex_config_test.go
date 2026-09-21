package hookconfig

import (
	"reflect"
	"testing"
)

func TestMutationCodexHooksConfigStateHonorsFeaturesTable(t *testing.T) {
	for _, test := range []struct {
		name         string
		data         string
		wantEnabled  bool
		wantParsable bool
	}{
		{name: "enabled", data: "[features]\nhooks = true", wantEnabled: true, wantParsable: true},
		{name: "disabled", data: "[features]\nhooks = false", wantEnabled: false, wantParsable: true},
		{name: "codex hooks disabled", data: "[features]\ncodex_hooks = false", wantEnabled: false, wantParsable: true},
		{name: "other table", data: "[other]\nhooks = false", wantEnabled: true, wantParsable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			enabled, parsable := codexHooksConfigState([]byte(test.data))
			if enabled != test.wantEnabled || parsable != test.wantParsable {
				t.Fatalf("codexHooksConfigState(%q)=(%v, %v), want (%v, %v)", test.data, enabled, parsable, test.wantEnabled, test.wantParsable)
			}
		})
	}
}

// TestMutationStripTOMLCommentKeepsEscapedDoubleQuotes は、escaped quote の直後にある
// # を文字列の一部として残すことを確認する。quote の終了判定を境界変異すると、ここで
// 文字列を誤って閉じて # 以降をコメントとして捨てる。
func TestMutationStripTOMLCommentKeepsEscapedDoubleQuotes(t *testing.T) {
	input := `key = "escaped \" quote # stays" # comment`
	want := `key = "escaped \" quote # stays" `
	if got := stripTOMLComment(input); got != want {
		t.Fatalf("stripTOMLComment(%q)=%q, want %q", input, got, want)
	}
}

func TestMutationScanTOMLValueDepthQuoteBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, line string
	}{
		{name: "single quote closes", line: `'value' trailing`},
		{name: "escaped double quote stays quoted", line: `"a\"b" trailing`},
	} {
		t.Run(test.name, func(t *testing.T) {
			depth, rest, ok := scanTOMLValueDepth(test.line, 0)
			if depth != 0 || rest != "" || !ok {
				t.Fatalf("scanTOMLValueDepth(%q)=(%d, %q, %v), want (0, %q, true)", test.line, depth, rest, ok, "")
			}
		})
	}
}

func TestMutationSplitTOMLInlineFieldsPreservesEscapedQuotesAndSeparators(t *testing.T) {
	input := `hooks = "a\"b", codex_hooks = true`
	want := []string{`hooks = "a\"b"`, "codex_hooks = true"}
	got, ok := splitTOMLInlineFields(input)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("splitTOMLInlineFields(%q)=(%#v, %v), want (%#v, true)", input, got, ok, want)
	}
}
