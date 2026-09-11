package archive

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseIndexFlagsSeparatesSkipWorktreeFromAssumeUnchanged(t *testing.T) {
	listing := "H plain\x00S skipped\x00h assumed\x00s both\x00M unmerged\x00S skipped\x00\x00"
	flags := parseIndexFlags(listing)
	if want := []string{"skipped", "both"}; !reflect.DeepEqual(flags.skipWorktree, want) {
		t.Fatalf("skip-worktree=%v, want %v", flags.skipWorktree, want)
	}
	if want := []string{"assumed", "both"}; !reflect.DeepEqual(flags.assumeUnchanged, want) {
		t.Fatalf("assume-unchanged=%v, want %v", flags.assumeUnchanged, want)
	}
	if want := []string{"skipped", "assumed", "both"}; !reflect.DeepEqual(flags.flagged(), want) {
		t.Fatalf("flagged=%v, want %v", flags.flagged(), want)
	}
	if !flags.blinding() {
		t.Fatal("listing with stat flags was not reported as blinding")
	}
	if want := []string{"plain", "both"}; !reflect.DeepEqual(flags.retain([]string{"plain", "gone", "both"}), want) {
		t.Fatalf("retain kept %v, want %v", flags.retain([]string{"plain", "gone", "both"}), want)
	}
}

func TestParseIndexFlagsIgnoresEmptyListing(t *testing.T) {
	flags := parseIndexFlags("")
	if flags.blinding() || len(flags.paths) != 0 {
		t.Fatalf("empty listing produced %+v", flags)
	}
}

func TestPathInputsEncodeNulSeparatedEntries(t *testing.T) {
	if got, want := string(literalPathspecs([]string{"a*.txt", "b"})), ":(literal)a*.txt\x00:(literal)b\x00"; got != want {
		t.Fatalf("literalPathspecs=%q, want %q", got, want)
	}
	if got, want := string(nulPathList([]string{"a*.txt", "b"})), "a*.txt\x00b\x00"; got != want {
		t.Fatalf("nulPathList=%q, want %q", got, want)
	}
}

// installSkipWorktreeHook は、checkout のたびに tracked file を個人版へ置き換えて skip-worktree を付ける source 側 hook を入れる。
// wx が作る clean base はこの hook を通るため、restore は復元先 index に flag が付いた状態から始まる。
func installSkipWorktreeHook(t *testing.T, commonDir, flag string) {
	t.Helper()
	script := "#!/bin/sh\nprintf 'personal\\n' > tracked\ngit update-index " + flag + " tracked\n"
	if err := os.WriteFile(filepath.Join(commonDir, "hooks", "post-checkout"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// blindSource は、source worktree を hook 後の状態（個人版 + skip-worktree）にしてから session の編集を載せる。
func blindSource(t *testing.T, repository, flag, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "update-index", flag, "tracked")
	if status := gitCommand(t, repository, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("fixture does not actually blind git status: %q", status)
	}
}

// TestRestoreReappliesStatFlagsAroundTreeApplication は、flag 付き path のラウンドトリップを検証する。
// flag を外さない read-tree は file を書き換えずに素通りするか拒否され、resume がそのたびに失敗する。
func TestRestoreReappliesStatFlagsAroundTreeApplication(t *testing.T) {
	for _, test := range []struct{ name, flag, tag string }{
		{name: "skip-worktree", flag: "--skip-worktree", tag: "S tracked"},
		{name: "assume-unchanged", flag: "--assume-unchanged", tag: "h tracked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository, repo, manager, worktreeRoot := archiveFixture(t)
			installSkipWorktreeHook(t, string(repo.CommonDir), test.flag)
			blindSource(t, repository, test.flag, "session\n")
			head := gitCommand(t, repository, "rev-parse", "HEAD")
			snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "blinded", time.Now().Add(time.Hour), nil)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.WorktreeOID == head {
				t.Fatal("clean shortcut discarded content hidden by an index stat flag")
			}
			target := filepath.Join(worktreeRoot, "restore", "root")
			pointAtSlot(t, manager, worktreeRoot, target)
			if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
				t.Fatalf("restore into a worktree with flagged index entries: %v", err)
			}
			restored, err := os.ReadFile(filepath.Join(target, "tracked"))
			if err != nil || string(restored) != "session\n" {
				t.Fatalf("restored tracked=%q err=%v, want %q", restored, err, "session\n")
			}
			if listing := gitCommand(t, target, "ls-files", "-v", "tracked"); listing != test.tag {
				t.Fatalf("index stat flag was not reapplied after restore: %q, want %q", listing, test.tag)
			}
		})
	}
}

// TestSnapshotTakesCleanShortcutWhenFlaggedPathMatchesHead は、flag があるだけでは recovery commit を作らないことを検証する。
// 編集なしの返却で全件走査と recovery object 生成が起きると、SNAPSHOT が数十秒に伸びて job 枠を占有する。
func TestSnapshotTakesCleanShortcutWhenFlaggedPathMatchesHead(t *testing.T) {
	repository, repo, manager, _ := archiveFixture(t)
	blindSource(t, repository, "--skip-worktree", "base\n")
	head := gitCommand(t, repository, "rev-parse", "HEAD")
	headTree := gitCommand(t, repository, "rev-parse", "HEAD^{tree}")
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "unchanged", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.WorktreeOID != head {
		t.Fatalf("snapshot fabricated a recovery commit for an unmodified skip-worktree path: %s", snapshot.WorktreeOID)
	}
	if snapshot.IndexTreeOID != headTree {
		t.Fatalf("snapshot index tree=%s, want HEAD tree %s", snapshot.IndexTreeOID, headTree)
	}
}

// TestSnapshotRecordsDeletionOfFlaggedPath は、絞った add -A が削除も記録することを検証する。
func TestSnapshotRecordsDeletionOfFlaggedPath(t *testing.T) {
	repository, repo, manager, worktreeRoot := archiveFixture(t)
	installSkipWorktreeHook(t, string(repo.CommonDir), "--skip-worktree")
	blindSource(t, repository, "--skip-worktree", "personal\n")
	if err := os.Remove(filepath.Join(repository, "tracked")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.SnapshotWithPersistence(context.Background(), repo, repository, "deleted", time.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if listing := gitCommand(t, repository, "ls-tree", "--name-only", snapshot.WorktreeOID); strings.Contains(listing, "tracked") {
		t.Fatalf("snapshot kept a deleted skip-worktree path: %q", listing)
	}
	target := filepath.Join(worktreeRoot, "restore", "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
		t.Fatalf("restore a snapshot that deletes a skip-worktree path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "tracked")); !os.IsNotExist(err) {
		t.Fatalf("deleted path survived restore: %v", err)
	}
}
