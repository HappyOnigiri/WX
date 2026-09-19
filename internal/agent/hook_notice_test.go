package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func captureHookStdout(t *testing.T, run func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = original }()
	run()
	if err := write.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(read)
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func TestSessionStartHookPrintsWorkspaceAndResumeNotices(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{response: map[string]string{"previous_worktree": "/old/worktree"}}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_SESSION_ID", "wx-resume")
	output := captureHookStdout(t, func() {
		if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-resume","source":"resume","cwd":"/new/worktree"}`)); err != nil {
			t.Fatal(err)
		}
	})
	want := "wx workspace notice:\n" +
		"- This is a wx-managed detached worktree. Its HEAD, index, and tracked files are isolated from the source checkout. Keep HEAD detached; to publish, run `git branch <name> HEAD` and push.\n" +
		"- Paths materialized by wx link rules are symlinks to the source workspace. Changes through them affect the source immediately and are not included in wx snapshots.\n" +
		"wx notice: this conversation previously ran in /old/worktree; the workspace is now /new/worktree. Do not use the old path.\n"
	if output != want {
		t.Fatalf("resume notice=%q, want %q", output, want)
	}

	t.Setenv("WX_SESSION_ID", "wx-startup")
	startupOutput := captureHookStdout(t, func() {
		if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-startup","source":"startup","cwd":"/new/worktree"}`)); err != nil {
			t.Fatal(err)
		}
	})
	wantStartup := "wx workspace notice:\n" +
		"- This is a wx-managed detached worktree. Its HEAD, index, and tracked files are isolated from the source checkout. Keep HEAD detached; to publish, run `git branch <name> HEAD` and push.\n" +
		"- Paths materialized by wx link rules are symlinks to the source workspace. Changes through them affect the source immediately and are not included in wx snapshots.\n"
	if startupOutput != wantStartup {
		t.Fatalf("startup notice=%q, want %q", startupOutput, wantStartup)
	}
}

func TestSessionStartHookPrintsRecoveryNoticeOnlyForInitialSources(t *testing.T) {
	clearHookEnvironment(t)
	handler := &recordingHandler{}
	ctx := startHookServer(t, handler)
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_RECOVERY_DISCARDED", "1")

	want := "wx workspace notice:\n" +
		"- This is a wx-managed detached worktree. Its HEAD, index, and tracked files are isolated from the source checkout. Keep HEAD detached; to publish, run `git branch <name> HEAD` and push.\n" +
		"- Paths materialized by wx link rules are symlinks to the source workspace. Changes through them affect the source immediately and are not included in wx snapshots.\n" +
		"wx recovery notice: this conversation is running from the current base branch; prior uncommitted local workspace state was not restored.\n"

	for _, test := range []struct {
		name, source, want string
	}{
		{name: "startup", source: "startup", want: want},
		{name: "resume", source: "resume", want: want},
		{name: "compact", source: "compact"},
		{name: "unknown", source: "future"},
		{name: "empty", source: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("WX_SESSION_ID", "wx-recovery-"+test.name)
			output := captureHookStdout(t, func() {
				payload := `{"session_id":"agent-recovery-` + test.name + `","source":"` + test.source + `"}`
				if err := RunHook(ctx, "session-start", strings.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			})
			if output != test.want {
				t.Fatalf("source=%q output=%q, want %q", test.source, output, test.want)
			}
		})
	}
}

func TestSessionStartHookPrintsNoNoticeWhenBindingFails(t *testing.T) {
	clearHookEnvironment(t)
	ctx := startHookServer(t, &recordingHandler{err: errors.New("bind failed")})
	t.Setenv("WX_SESSION_ID", "wx-bind-failure")
	t.Setenv("WX_SESSION_TOKEN", "token")
	t.Setenv("WX_RECOVERY_DISCARDED", "1")
	var hookErr error
	output := captureHookStdout(t, func() {
		hookErr = RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-bind-failure","source":"startup"}`))
	})
	if hookErr == nil || !strings.Contains(hookErr.Error(), "bind failed") {
		t.Fatalf("hook error=%v, want bind failure", hookErr)
	}
	if output != "" {
		t.Fatalf("binding failure emitted notice: %q", output)
	}
}

func TestSessionStartHookWithoutWXEnvironmentPrintsNoNotice(t *testing.T) {
	clearHookEnvironment(t)
	output := captureHookStdout(t, func() {
		if err := RunHook(context.Background(), "session-start", strings.NewReader(`{"session_id":"agent","source":"startup"}`)); err != nil {
			t.Fatal(err)
		}
	})
	if output != "" {
		t.Fatalf("hook without wx environment emitted notice: %q", output)
	}
}
