package main

import (
	"context"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// --unmanaged は対象範囲が他の mode と重ならないので併用を受け付けない。
// 受け付けると、どちらの範囲を消したのかが結果から読めなくなる。
func TestRunClearRejectsUnmanagedCombinedWithOtherModes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, flag := range []string{"--all", "--standby", "--discard"} {
		if code := runClean(context.Background(), []string{"--unmanaged", flag}); code != 2 {
			t.Fatalf("%s exit code=%d", flag, code)
		}
	}
}

// 終了コードは、消せなかった対象と見えていない root のどちらも成功として扱わない。
func TestUnmanagedExitCodeFailsOnFailuresAndUninspectedRoots(t *testing.T) {
	t.Parallel()
	done := unmanagedReplyView{Targets: []unmanagedTargetView{{State: "DONE"}, {State: "SKIPPED"}}}
	if code := unmanagedExitCode(done); code != 0 {
		t.Fatalf("completed run exit code=%d", code)
	}
	if code := unmanagedExitCode(unmanagedReplyView{DryRun: true}); code != 0 {
		t.Fatalf("empty dry run exit code=%d", code)
	}
	failed := unmanagedReplyView{Targets: []unmanagedTargetView{{State: "DONE"}, {State: "FAILED"}}}
	if code := unmanagedExitCode(failed); code != 1 {
		t.Fatalf("failed target exit code=%d", code)
	}
	// 列挙できなかった root がある dry-run も、対象が無いことの確認にはならない。
	uninspected := unmanagedReplyView{DryRun: true, Errors: []string{"inspect root /gone: no such file or directory"}}
	if code := unmanagedExitCode(uninspected); code != 1 {
		t.Fatalf("uninspected root exit code=%d", code)
	}
}

// 対象が無いときは clear の通常経路とは別の文言を出す。管理 worktree の話ではないためである。
func TestPrintUnmanagedTargetsSeparatesKindAndReason(t *testing.T) {
	t.Parallel()
	var empty strings.Builder
	printUnmanagedTargets(newTextRenderer(&empty, i18n.English), nil)
	if !strings.Contains(empty.String(), "wx namespaces") {
		t.Fatalf("empty output=%q", empty.String())
	}
	var listed strings.Builder
	printUnmanagedTargets(newTextRenderer(&listed, i18n.English), []unmanagedTargetView{
		{Path: "/root/_recovery/workspace-snapshots/a.tar", Kind: "workspace_snapshot", State: "DONE"},
		{Path: "/root/ws/slot", Kind: "slot_directory", State: "SKIPPED", Reason: "a slot was registered at this path"},
	})
	output := listed.String()
	for _, want := range []string{"workspace_snapshot", "slot_directory", "a slot was registered at this path"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output=%q missing %q", output, want)
		}
	}
}
