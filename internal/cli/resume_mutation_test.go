package cli

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
)

const resumeParserTestTimeout = time.Second

// runResumeParserWithTimeout は変異で parser が無限ループしても、テスト全体を待たせない。
// 正常な parser は即時に返るため、ここでの失敗は変異を KILLED と判定させる。
func runResumeParserWithTimeout[T any](t *testing.T, fn func() T) T {
	t.Helper()
	result := make(chan T, 1)
	go func() {
		result <- fn()
	}()
	timer := time.NewTimer(resumeParserTestTimeout)
	defer timer.Stop()
	select {
	case value := <-result:
		return value
	case <-timer.C:
		t.Fatalf("resume parser did not return within %s", resumeParserTestTimeout)
		var zero T
		return zero
	}
}

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

func TestResolveDirectResumeMutationBoundariesReportTheRecordedCWD(t *testing.T) {
	for _, tt := range []struct {
		name            string
		conversationCWD string
		wantNotice      string
	}{
		{name: "conversation cwd", conversationCWD: "/conversation", wantNotice: "/conversation"},
		{name: "fallback source cwd", wantNotice: "/source"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			handler := &resumeLaunchHandler{}
			client, stop := serveResumeLaunchRPC(t, handler)
			defer stop()
			stderr := captureStderrForLease(t, func() {
				got, ok := client.resolveDirectResume(context.Background(), "/source", tt.conversationCWD)
				if !ok || got.cwd == "" {
					t.Fatalf("direct resume=%+v ok=%t, want a worktree-free start", got, ok)
				}
			})
			if !strings.Contains(stderr, tt.wantNotice) {
				t.Fatalf("stderr=%q, want recorded path %q", stderr, tt.wantNotice)
			}
			if methods := handler.methodsSnapshot(); len(methods) == 0 || methods[len(methods)-1] != "WorktreePolicy" {
				t.Fatalf("methods=%v, want a policy lookup", methods)
			}
		})
	}
}

func TestResumeWorktreePolicyMutationBoundariesKeepDaemonReply(t *testing.T) {
	handler := &resumeLaunchHandler{policy: daemon.WorktreePolicyReply{Root: "/workspace", Mode: "cold", Resolved: true}}
	client, stop := serveResumeLaunchRPC(t, handler)
	defer stop()
	if got := client.resumeWorktreePolicy(context.Background(), "/conversation"); !reflect.DeepEqual(got, handler.policy) {
		t.Fatalf("policy=%+v, want %+v", got, handler.policy)
	}
}

func TestValidateResumeOptionsMutationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name     string
		intent   resumeIntent
		explicit string
		fresh    bool
		branches []string
		wantErr  string
	}{
		{name: "ordinary launch", intent: resumeIntent{Kind: resumeIntentNone}},
		{name: "fresh requires resume", intent: resumeIntent{Kind: resumeIntentNone}, fresh: true, wantErr: "fresh"},
		{name: "intent is a resume", intent: resumeIntent{Kind: resumeIntentLookup}, fresh: true},
		{name: "explicit is a resume", explicit: "session", fresh: true},
		{name: "branch needs fresh", intent: resumeIntent{Kind: resumeIntentLookup}, branches: []string{"main"}, wantErr: "fresh"},
		{name: "fresh branch is valid", intent: resumeIntent{Kind: resumeIntentLookup}, branches: []string{"main"}, fresh: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResumeOptions(tt.intent, tt.explicit, tt.fresh, tt.branches)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("err=%v, want nil", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err=%v, want %q", err, tt.wantErr)
			}
		})
	}
}
