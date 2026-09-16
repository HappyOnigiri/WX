package cli

import (
	"reflect"
	"testing"
)

// exec だけの入力は、後続引数を読むことなく明示的な resume の前置として扱う。
func TestCodexResumeShapeAcceptsExecWithoutFollowingArguments(t *testing.T) {
	got, ok := codexResumeShape([]string{"exec"})
	want := codexResumeLocation{resumeIndex: -1, exec: true, prefixEnd: 1}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("codexResumeShape([exec])=(%+v, %v), want (%+v, true)", got, ok, want)
	}
}

// 末尾の値付き flag と exec 後の末尾 flag は、次の引数なしで形を確定する。
func TestCodexResumeShapeKeepsTerminalFlagBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want codexResumeLocation
		ok   bool
	}{
		{name: "global flag", args: []string{"--model"}},
		{name: "exec flag", args: []string{"exec", "--model"}, want: codexResumeLocation{resumeIndex: -1, exec: true, prefixEnd: 2}, ok: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := codexResumeShape(tt.args)
			if ok != tt.ok || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("codexResumeShape(%v)=(%+v, %v), want (%+v, %v)", tt.args, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// 末尾の値付き flag は値を持たないまま agent 側へ渡し、入力外を参照しない。
func TestParseCodexResumeTailKeepsTerminalValueFlag(t *testing.T) {
	got := parseCodexResumeTail([]string{"--model"})
	want := resumeIntent{Kind: resumeIntentPicker, Rest: []string{"--model"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCodexResumeTail([--model])=%#v, want %#v", got, want)
	}
}

// 末尾の --cd は取り除くだけで、後続の値がない入力を読み飛ばさない。
func TestParseCodexResumeTailKeepsTerminalCDFlag(t *testing.T) {
	got := parseCodexResumeTail([]string{"--cd"})
	want := resumeIntent{Kind: resumeIntentPicker}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseCodexResumeTail([--cd])=%#v, want %#v", got, want)
	}
}

// resume の前置から末尾の --cd を除いても空の引数列を nil として返す。
func TestStripCodexCDArgsKeepsTerminalCDFlag(t *testing.T) {
	if got := stripCodexCDArgs([]string{"--cd"}); got != nil {
		t.Fatalf("stripCodexCDArgs([--cd])=%v, want nil", got)
	}
}

// exec 前置が空なら、resume の再構成で使わないことを示す -1 を返す。
func TestCodexExecIndexReturnsMinusOneForEmptyArguments(t *testing.T) {
	if got := codexExecIndex(nil); got != -1 {
		t.Fatalf("codexExecIndex(nil)=%d, want -1", got)
	}
}

// exec 前置の末尾に値付き flag だけがあっても、存在しない値を追加で読まない。
func TestCodexExecIndexKeepsTerminalValueFlag(t *testing.T) {
	if got := codexExecIndex([]string{"--model"}); got != -1 {
		t.Fatalf("codexExecIndex([--model])=%d, want -1", got)
	}
}

// exec resume の位置は必ず exec より後ろなので、前置と後続を正しく分ける。
func TestCodexExecResumePartsSplitsExistingResume(t *testing.T) {
	prefix, rest, ok := codexExecResumeParts([]string{"exec", "resume", "session-id", "prompt"})
	if !ok || !reflect.DeepEqual(prefix, []string{"exec"}) || !reflect.DeepEqual(rest, []string{"prompt"}) {
		t.Fatalf("codexExecResumeParts=%v, %v, %v, want [exec], [prompt], true", prefix, rest, ok)
	}
}
