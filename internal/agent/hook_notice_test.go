package agent

import (
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

func TestResumeHookPrintsOneLinePreviousWorktreeNotice(t *testing.T) {
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
	want := "wx notice: this conversation previously ran in /old/worktree; the workspace is now /new/worktree. Do not use the old path.\n"
	if output != want {
		t.Fatalf("resume notice=%q, want %q", output, want)
	}
	if strings.Count(strings.TrimSuffix(output, "\n"), "\n") != 0 {
		t.Fatalf("resume notice spans multiple lines: %q", output)
	}

	t.Setenv("WX_SESSION_ID", "wx-startup")
	startupOutput := captureHookStdout(t, func() {
		if err := RunHook(ctx, "session-start", strings.NewReader(`{"session_id":"agent-startup","source":"startup","cwd":"/new/worktree"}`)); err != nil {
			t.Fatal(err)
		}
	})
	if startupOutput != "" {
		t.Fatalf("startup emitted resume notice: %q", startupOutput)
	}
}
