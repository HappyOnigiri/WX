package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
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
	printCleanTargets(&out, nil)
	if !strings.Contains(out.String(), "no managed worktrees to clear") {
		t.Fatalf("empty output=%q", out.String())
	}
	out.Reset()
	printCleanTargets(&out, []cleanTargetView{
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
	if got := cleanSummaryLine(nil); got != "0 targets" {
		t.Fatalf("empty summary=%q", got)
	}
	got := cleanSummaryLine(map[string]int{"total": 3, "DONE": 2, "SKIPPED": 1, "FAILED": 0})
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

func TestWaitForCleanReportsPollFailuresWithTheLastKnownState(t *testing.T) {
	failure := errors.New("socket closed")
	call := func(context.Context, string, any, any) error { return failure }
	accepted := cleanReplyView{RunID: "run", State: "RUNNING", Targets: []cleanTargetView{{SlotID: "abc", State: "PENDING"}}}
	final, err := waitForClean(context.Background(), call, accepted)
	if !errors.Is(err, failure) || final.RunID != "run" || len(final.Targets) != 1 {
		t.Fatalf("final=%+v err=%v", final, err)
	}
}

func TestRunClearRejectsPositionalArguments(t *testing.T) {
	if code := runClean(context.Background(), []string{"extra"}); code != 2 {
		t.Fatalf("exit code=%d", code)
	}
	if code := runClean(context.Background(), []string{"--unknown"}); code != 2 {
		t.Fatalf("unknown flag exit code=%d", code)
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
