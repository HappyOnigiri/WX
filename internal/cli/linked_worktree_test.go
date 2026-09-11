package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cwd が main worktree なら、貸出の基準と食い違わないので確認の対象にしない。
func TestDetectLinkedWorktreeBaseIgnoresMainWorktree(t *testing.T) {
	main := probeWorktreeFixture(t)
	client := linkedWorktreeClient(t, t.TempDir())
	if base, mismatched := client.detectLinkedWorktreeBase(context.Background(), main); mismatched {
		t.Fatalf("main worktree reported a mismatch: %+v", base)
	}
}

// HEAD が一致する linked worktree は、main の HEAD で貸し出しても実害がないので確認を出さない。
func TestDetectLinkedWorktreeBaseIgnoresMatchingHead(t *testing.T) {
	main := probeWorktreeFixture(t)
	linked := addLinkedWorktree(t, main, "review", "")
	client := linkedWorktreeClient(t, t.TempDir())
	if base, mismatched := client.detectLinkedWorktreeBase(context.Background(), linked); mismatched {
		t.Fatalf("linked worktree at the same HEAD reported a mismatch: %+v", base)
	}
}

// HEAD が異なる linked worktree では、cwd と貸出元の双方を確認へ載せられるように検出する。
func TestDetectLinkedWorktreeBaseReportsDivergedHead(t *testing.T) {
	main := probeWorktreeFixture(t)
	linked := addLinkedWorktree(t, main, "review", "second")
	client := linkedWorktreeClient(t, t.TempDir())
	base, mismatched := client.detectLinkedWorktreeBase(context.Background(), linked)
	if !mismatched {
		t.Fatal("diverged linked worktree was not detected")
	}
	if base.Path != linked || base.MainPath != main {
		t.Fatalf("base = %+v (linked %s, main %s)", base, linked, main)
	}
	if base.Head == "" || base.MainHead == "" || base.Head == base.MainHead {
		t.Fatalf("heads = %+v", base)
	}
}

// cwd がサブディレクトリでも、その linked worktree の root と HEAD で判定する。
func TestDetectLinkedWorktreeBaseAcceptsSubdirectory(t *testing.T) {
	main := probeWorktreeFixture(t)
	linked := addLinkedWorktree(t, main, "review", "second")
	sub := filepath.Join(linked, "nested")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	client := linkedWorktreeClient(t, t.TempDir())
	base, mismatched := client.detectLinkedWorktreeBase(context.Background(), sub)
	if !mismatched || base.Path != linked {
		t.Fatalf("base = %+v, mismatched = %v", base, mismatched)
	}
}

// wx が作った slot も linked worktree だが、貸出と snapshot で HEAD が動くのが前提なので確認しない。
func TestDetectLinkedWorktreeBaseIgnoresWXSlot(t *testing.T) {
	main := probeWorktreeFixture(t)
	worktreeRoot := t.TempDir()
	slot := addLinkedWorktree(t, main, filepath.Join(worktreeRoot, "slot"), "second")
	client := linkedWorktreeClient(t, worktreeRoot)
	if base, mismatched := client.detectLinkedWorktreeBase(context.Background(), slot); mismatched {
		t.Fatalf("wx slot reported a mismatch: %+v", base)
	}
}

// Git 管理外のディレクトリでは判定材料が無いので、確認も案内も出さない。
func TestDetectLinkedWorktreeBaseIgnoresNonRepository(t *testing.T) {
	client := linkedWorktreeClient(t, t.TempDir())
	if _, mismatched := client.detectLinkedWorktreeBase(context.Background(), t.TempDir()); mismatched {
		t.Fatal("non repository reported a mismatch")
	}
	if _, mismatched := client.detectLinkedWorktreeBase(context.Background(), ""); mismatched {
		t.Fatal("empty cwd reported a mismatch")
	}
}

// 端末が無い経路（test の stdin/stderr、--json）は notice を出して従来どおり続行する。
func TestConfirmLinkedWorktreeBaseContinuesWithoutTerminal(t *testing.T) {
	main := probeWorktreeFixture(t)
	linked := addLinkedWorktree(t, main, "review", "second")
	client := linkedWorktreeClient(t, t.TempDir())
	if !client.confirmLinkedWorktreeBase(context.Background(), linked, false) {
		t.Fatal("non interactive lease was cancelled")
	}
	if !client.confirmLinkedWorktreeBase(context.Background(), linked, true) {
		t.Fatal("lease without a terminal was cancelled")
	}
}

// 記録済み session の再開は当時の workspace を復元するため、cwd を貸出の基準にしない。
func TestLeaseBaseCWDSkipsRecordedResume(t *testing.T) {
	plan := launchPlan{cwd: "/cwd"}
	if got := plan.leaseBaseCWD(); got != "/cwd" {
		t.Fatalf("fresh launch base = %q", got)
	}
	plan = launchPlan{cwd: "/cwd", resuming: true, target: resumeTarget{WXSessionID: "wx1"}}
	if got := plan.leaseBaseCWD(); got != "" {
		t.Fatalf("recorded resume base = %q", got)
	}
	plan = launchPlan{cwd: "/cwd", resuming: true, target: resumeTarget{CWD: "/recorded"}}
	if got := plan.leaseBaseCWD(); got != "/recorded" {
		t.Fatalf("conversation resume base = %q", got)
	}
}

func linkedWorktreeClient(t *testing.T, worktreeRoot string) Client {
	t.Helper()
	client := Client{}
	client.Config.Discovery.Timeout.Duration = 30 * time.Second
	resolved, err := filepath.EvalSymlinks(worktreeRoot)
	if err != nil {
		t.Fatal(err)
	}
	client.Config.Storage.WorktreeRoot = resolved
	return client
}

// addLinkedWorktree は main worktree に linked worktree を足し、commit が空でなければ 1 つ進めた HEAD にする。
// path が相対なら main worktree の隣に作る。
func addLinkedWorktree(t *testing.T, main, path, commit string) string {
	t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(main), path)
	}
	probeGitCommand(t, main, "worktree", "add", "--detach", path)
	if commit != "" {
		if err := os.WriteFile(filepath.Join(path, "tracked"), []byte(commit+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		probeGitCommand(t, path, "add", ".")
		probeGitCommand(t, path, "commit", "-m", commit)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
