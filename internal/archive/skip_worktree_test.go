package archive

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
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

func TestNulPathListEncodesNulSeparatedEntries(t *testing.T) {
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

// TestSnapshotAndRestoreLeaveHookBlindedPathsToTheHook は、flag を立てる post-checkout hook を持つ repository の往復を検証する。
// 編集なしの返却と同じく clean 短絡が効き、復元先では hook が置いた個人版と flag がそのまま残る必要がある。
// flag を外して復元すると個人版を tree の内容で上書きし、記録すると個人版が source の recovery object に残る。
// commentlint:allow-long -- hook 運用での往復の期待と、外した場合・記録した場合の害を説明する
func TestSnapshotAndRestoreLeaveHookBlindedPathsToTheHook(t *testing.T) {
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
			if snapshot.WorktreeOID != head {
				t.Fatalf("clean shortcut was not taken for an index-flagged path: worktree=%s head=%s", snapshot.WorktreeOID, head)
			}
			target := filepath.Join(worktreeRoot, "restore", "root")
			pointAtSlot(t, manager, worktreeRoot, target)
			if err := manager.Restore(context.Background(), repo, target, "restore-slot", snapshot); err != nil {
				t.Fatalf("restore into a worktree with flagged index entries: %v", err)
			}
			if listing := gitCommand(t, target, "ls-files", "-v", "tracked"); listing != test.tag {
				t.Fatalf("index stat flag was not reinstated after restore: %q, want %q", listing, test.tag)
			}
			// skip-worktree は hook が置いた個人版を保つ。assume-unchanged は flag を保っていても
			// `read-tree --reset -u` が tree の内容で書き戻すため、git 側の仕様として内容までは保証しない。
			if test.flag != "--skip-worktree" {
				return
			}
			if data, err := os.ReadFile(filepath.Join(target, "tracked")); err != nil || string(data) != "personal\n" {
				t.Fatalf("restore overwrote the skip-worktree file: data=%q err=%v", data, err)
			}
		})
	}
}
