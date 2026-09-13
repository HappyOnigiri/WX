package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestCleanExitCodeSeparatesFailuresFromExcludedSessions(t *testing.T) {
	for _, test := range []struct {
		name  string
		reply cleanReplyView
		want  int
	}{
		{name: "nothing to do", reply: cleanReplyView{State: "DONE"}, want: 0},
		{name: "all removed", reply: cleanReplyView{State: "DONE", Targets: []cleanTargetView{{State: "DONE"}}}, want: 0},
		{name: "session in use is not a failure", reply: cleanReplyView{State: "DONE", Targets: []cleanTargetView{{State: "DONE"}, {State: "SKIPPED"}}}, want: 0},
		{name: "failure", reply: cleanReplyView{State: "DONE", Targets: []cleanTargetView{{State: "FAILED"}}}, want: 1},
		{name: "quarantined", reply: cleanReplyView{State: "DONE", Targets: []cleanTargetView{{State: "QUARANTINED"}}}, want: 1},
		{name: "incomplete", reply: cleanReplyView{State: "RUNNING", Targets: []cleanTargetView{{State: "DONE"}}}, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := cleanExitCode(test.reply); got != test.want {
				t.Fatalf("exit code=%d want %d", got, test.want)
			}
		})
	}
}

func TestPrintCleanTargetsShowsPathsAndReasons(t *testing.T) {
	var out bytes.Buffer
	r := newTextRenderer(&out, i18n.English)
	printCleanTargets(r, nil)
	if !strings.Contains(out.String(), "no managed worktrees to clear") {
		t.Fatalf("empty output=%q", out.String())
	}
	out.Reset()
	printCleanTargets(r, []cleanTargetView{
		{SlotID: "abc", Path: "/wx/workspace/abc", State: "DONE"},
		{SlotID: "def", Path: "/wx/workspace/def", State: "SKIPPED", Reason: "session xyz is in use"},
	})
	text := out.String()
	for _, want := range []string{"DONE", "/wx/workspace/abc", "SKIPPED", "session xyz is in use"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q is missing %q", text, want)
		}
	}
}

func TestCleanSummaryLineOmitsEmptyStates(t *testing.T) {
	r := newTextRenderer(io.Discard, i18n.English)
	if got := cleanSummaryLine(r, nil); got != "0 targets" {
		t.Fatalf("empty summary=%q", got)
	}
	got := cleanSummaryLine(r, map[string]int{"total": 3, "DONE": 2, "SKIPPED": 1, "FAILED": 0})
	if got != "3 target(s), DONE 2, SKIPPED 1" {
		t.Fatalf("summary=%q", got)
	}
}

func TestWaitForCleanPollsUntilTheRunCloses(t *testing.T) {
	calls := 0
	call := func(_ context.Context, method string, _, result any) error {
		calls++
		if method != "CleanStatus" {
			t.Fatalf("unexpected method %s", method)
		}
		reply, ok := result.(*cleanReplyView)
		if !ok {
			t.Fatalf("result type %T", result)
		}
		if calls < 2 {
			*reply = cleanReplyView{RunID: "run", State: "RUNNING"}
			return nil
		}
		*reply = cleanReplyView{RunID: "run", State: "DONE", Targets: []cleanTargetView{{State: "DONE"}}}
		return nil
	}
	final, err := waitForClean(context.Background(), call, cleanReplyView{RunID: "run", State: "RUNNING"})
	if err != nil || final.State != "DONE" || calls != 2 {
		t.Fatalf("final=%+v calls=%d err=%v", final, calls, err)
	}
	if code := cleanExitCode(final); code != 0 {
		t.Fatalf("exit code=%d", code)
	}
}

// run が閉じても補充再開が残っている間はポーリングを続ける。
// ここで抜けると、再開の結果を一切見ずに終わってしまう。
func TestWaitForCleanKeepsPollingWhileReplenishmentIsPending(t *testing.T) {
	calls := 0
	call := func(_ context.Context, _ string, _, result any) error {
		calls++
		reply := result.(*cleanReplyView)
		if calls < 2 {
			*reply = cleanReplyView{RunID: "run", State: "DONE", ReplenishPending: true}
			return nil
		}
		*reply = cleanReplyView{RunID: "run", State: "DONE", Replenish: cleanReplenishView{
			Workspaces: []retryStandbyView{{Root: "/wx/workspace", Generation: 2, Resumed: true, Scheduled: true}},
		}}
		return nil
	}
	final, err := waitForClean(context.Background(), call, cleanReplyView{RunID: "run", State: "DONE", ReplenishPending: true})
	if err != nil || calls != 2 || len(final.Replenish.Workspaces) != 1 {
		t.Fatalf("final=%+v calls=%d err=%v", final, calls, err)
	}
	if code := cleanExitCode(final); code != 0 {
		t.Fatalf("exit code=%d", code)
	}
}

// 補充再開の結果は retry-standby と同じ 1 行で出し、失敗があれば stderr へ回す。
func TestPrintCleanReplenishUsesTheRetryStandbyLines(t *testing.T) {
	var out, errOut bytes.Buffer
	r, errors := newTextRenderer(&out, i18n.English), newTextRenderer(&errOut, i18n.English)
	reply := cleanReplyView{Replenish: cleanReplenishView{
		Workspaces: []retryStandbyView{{Root: "/wx/workspace", Generation: 2, Resumed: true, Scheduled: true}},
	}}
	reply.Replenish.Failures = append(reply.Replenish.Failures, struct {
		Root  string `json:"root"`
		Error string `json:"error"`
	}{Root: "/wx/other", Error: "disabled"})
	// `--replenish` 付きの run へ合流した実行にも、戻った workspace は見せる。
	printCleanReplenish(r, errors, false, reply)
	if !strings.Contains(out.String(), "/wx/workspace") || !strings.Contains(errOut.String(), "/wx/other") {
		t.Fatalf("joined output: %q %q", out.String(), errOut.String())
	}
	out.Reset()
	errOut.Reset()
	printCleanReplenish(r, errors, true, reply)
	if !strings.Contains(out.String(), "standby replenishment resumed for /wx/workspace") {
		t.Fatalf("stdout=%q", out.String())
	}
	if !strings.Contains(errOut.String(), "retry-standby /wx/other") {
		t.Fatalf("stderr=%q", errOut.String())
	}
	// 失敗が残った再開は retry-standby --all に合わせて 1 とする。
	if code := cleanExitCode(cleanReplyView{State: "DONE", Replenish: reply.Replenish}); code != 1 {
		t.Fatalf("exit code=%d", code)
	}
	// 対象が 0 件だったことは、--replenish を指定した実行にだけ伝える。
	out.Reset()
	printCleanReplenish(r, newTextRenderer(io.Discard, i18n.English), false, cleanReplyView{})
	if out.Len() != 0 {
		t.Fatalf("empty replenishment reported to a joined run: %q", out.String())
	}
	printCleanReplenish(r, newTextRenderer(io.Discard, i18n.English), true, cleanReplyView{})
	if !strings.Contains(out.String(), "no workspace needed standby replenishment resumed") {
		t.Fatalf("empty replenishment output=%q", out.String())
	}
}

func TestWaitForCleanReportsPollFailuresWithTheLastKnownState(t *testing.T) {
	failure := errors.New("socket closed")
	call := func(context.Context, string, any, any) error { return failure }
	accepted := cleanReplyView{RunID: "run", State: "RUNNING", Targets: []cleanTargetView{{SlotID: "abc", State: "PENDING"}}}
	final, err := waitForClean(context.Background(), call, accepted)
	if !errors.Is(err, failure) || final.RunID != "run" || len(final.Targets) != 1 {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}

func TestRunClearRejectsUnsupportedOptionCombinations(t *testing.T) {
	// 表示言語は設定から読むため、英語の表示を検査するテストは空のホームを見る。
	t.Setenv("HOME", t.TempDir())
	// workspace path は 1 つまでで、--unmanaged は範囲指定とも他の mode とも重ならない。
	// --replenish は待機用 worktree を削除する mode でしか意味を持たない。
	for name, args := range map[string][]string{
		"two paths":            {"/tmp/one", "/tmp/two"},
		"unmanaged with path":  {"--unmanaged", "/tmp/one"},
		"unmanaged with mode":  {"--unmanaged", "--standby"},
		"replenish alone":      {"--replenish"},
		"replenish with paths": {"--replenish", "/tmp/one"},
		"unknown flag":         {"--unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			if code := runClean(context.Background(), args); code != 2 {
				t.Fatalf("exit code=%d", code)
			}
		})
	}
	help := captureStdout(t, func() {
		if code := runClean(context.Background(), []string{"--help"}); code != 0 {
			t.Fatalf("help exit code=%d", code)
		}
	})
	if !strings.Contains(help, "Usage: wx clear") {
		t.Fatalf("help=%q", help)
	}
}
