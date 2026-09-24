package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installUpdateFlagHook は、checkout のたびに tracked file を個人版へ置き換えて index flag を張る source 側 hook を入れる。
// once を立てると 2 回目以降は何もしないので、hook が flag を張り直さない構成を再現できる。
// 呼び出し回数は共通 git directory の counter へ記録し、worktree 側の untracked を増やさない。
func installUpdateFlagHook(t *testing.T, repository, flag string, once bool) string {
	t.Helper()
	counter := filepath.Join(repository, ".git", "wx-post-checkout-runs")
	guard := ""
	if once {
		guard = "if [ -e \"$common/wx-hook-done\" ]; then exit 0; fi\n: > \"$common/wx-hook-done\"\n"
	}
	script := "#!/bin/sh\ncommon=\"$(git rev-parse --git-common-dir)\"\n" +
		"printf 'x' >> \"$common/wx-post-checkout-runs\"\n" + guard +
		"printf 'personal\\n' > conf\nchmod 600 conf\ngit update-index " + flag + " conf\n"
	if err := os.WriteFile(filepath.Join(repository, ".git", "hooks", "post-checkout"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return counter
}

func hookRuns(t *testing.T, counter string) int {
	t.Helper()
	data, err := os.ReadFile(counter)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(data)
}

// commitConf は tracked file conf を書き換えて commit し、その OID を返す。
func commitConf(t *testing.T, repository, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repository, "conf"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, repository, "add", "conf")
	cowGit(t, repository, "commit", "-m", "conf "+body)
	return cowGit(t, repository, "rev-parse", "HEAD")
}

// hook が個人設定を置いて index flag を張る repository でも、その設定 file を触った commit への更新が通る。
// 段階1 まではこの更新を書込み前に弾いていたため、standby が毎回 Cold Start に落ちていた。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestUpdateKeepsFlaggedPathsWhenTheHookReinstatesThem(t *testing.T) {
	ctx := context.Background()
	p, repo, _, target := cowFixture(t)
	main := string(repo.MainPath)
	baseOID := commitConf(t, main, "repo\n")
	counter := installUpdateFlagHook(t, main, "--skip-worktree", false)
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	newOID := commitConf(t, main, "repo2\n")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("an update over a hook-flagged path must stay eligible: %v", err)
	}
	if _, err := p.UpdateLocked(ctx, repo, target, baseOID, newOID, testSlotID, nil, nil); err != nil {
		t.Fatalf("update over a hook-flagged path: %v", err)
	}
	if runs := hookRuns(t, counter); runs != 2 {
		t.Fatalf("post-checkout ran %d times, want one prepare and one update", runs)
	}
	if data, err := os.ReadFile(filepath.Join(target, "conf")); err != nil || string(data) != "personal\n" {
		t.Fatalf("hook did not regenerate the personal file: data=%q err=%v", data, err)
	}
	if listing := cowGit(t, target, "ls-files", "-v", "conf"); listing != "S conf" {
		t.Fatalf("index flag after the update=%q, want %q", listing, "S conf")
	}
	if status := cowGit(t, target, "status", "--porcelain=v1"); status != "" {
		t.Fatalf("updated worktree is not clean: %q", status)
	}
	if head := cowGit(t, target, "rev-parse", "HEAD"); head != newOID {
		t.Fatalf("HEAD=%s, want %s", head, newOID)
	}
}

// hook が flag を張り直さない構成でも、更新は個人設定の内容・mode・flag を残す。
// 利用者が手で flag を立てた場合がこれに当たり、解除したまま返すと個人設定を失う。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestUpdateRestoresFlaggedPathsWhenTheHookDoesNot(t *testing.T) {
	for _, test := range []struct{ name, flag, tag string }{
		{name: "skip-worktree", flag: "--skip-worktree", tag: "S conf"},
		{name: "assume-unchanged", flag: "--assume-unchanged", tag: "h conf"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			p, repo, _, target := cowFixture(t)
			main := string(repo.MainPath)
			baseOID := commitConf(t, main, "repo\n")
			counter := installUpdateFlagHook(t, main, test.flag, true)
			if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
				t.Fatal(err)
			}
			newOID := commitConf(t, main, "repo2\n")
			if _, err := p.UpdateLocked(ctx, repo, target, baseOID, newOID, testSlotID, nil, nil); err != nil {
				t.Fatalf("update over a flagged path the hook leaves alone: %v", err)
			}
			if runs := hookRuns(t, counter); runs != 2 {
				t.Fatalf("post-checkout ran %d times, want one prepare and one update", runs)
			}
			info, err := os.Stat(filepath.Join(target, "conf"))
			if err != nil {
				t.Fatal(err)
			}
			if mode := info.Mode().Perm(); mode != 0o600 {
				t.Fatalf("restored mode=%o, want 600", mode)
			}
			if data, err := os.ReadFile(filepath.Join(target, "conf")); err != nil || string(data) != "personal\n" {
				t.Fatalf("update did not restore the personal file: data=%q err=%v", data, err)
			}
			if listing := cowGit(t, target, "ls-files", "-v", "conf"); listing != test.tag {
				t.Fatalf("index flag after the update=%q, want %q", listing, test.tag)
			}
			if status := cowGit(t, target, "status", "--porcelain=v1"); status != "" {
				t.Fatalf("updated worktree is not clean: %q", status)
			}
		})
	}
}

// flag を 1 件も解除しない更新では post-checkout を実行しない。
// 更新は外部コマンドを条件付きでのみ動かす流儀で、通常の更新の挙動とコストを変えないためである。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestUpdateWithoutFlaggedPathsSkipsPostCheckout(t *testing.T) {
	ctx := context.Background()
	p, repo, _, target := cowFixture(t)
	main := string(repo.MainPath)
	baseOID := commitConf(t, main, "repo\n")
	counter := installUpdateFlagHook(t, main, "--skip-worktree", false)
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	// hook が張った flag を外し、差分も flag の無い path だけにする。
	cowGit(t, target, "update-index", "--no-skip-worktree", "conf")
	if err := os.WriteFile(filepath.Join(target, "conf"), []byte("repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, "file"), []byte(cowBody+"after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, main, "add", "file")
	cowGit(t, main, "commit", "-m", "rewrite a tracked file without index flags")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	before := hookRuns(t, counter)
	if _, err := p.UpdateLocked(ctx, repo, target, baseOID, newOID, testSlotID, nil, nil); err != nil {
		t.Fatalf("update without flagged paths: %v", err)
	}
	if runs := hookRuns(t, counter); runs != before {
		t.Fatalf("post-checkout ran %d times during an update without flagged paths", runs-before)
	}
	if data, err := os.ReadFile(filepath.Join(target, "file")); err != nil || !strings.HasSuffix(string(data), "after\n") {
		t.Fatalf("update did not rewrite the changed path: err=%v", err)
	}
}

// flag 付き path の件数と退避 byte 数は、上限ちょうどまで更新対象として受理する。
// 境界値を拒否すると、安全に復元できる standby まで Cold Start へ落ちる。
// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestValidateUpdateCandidateAcceptsExactFlaggedPathLimits(t *testing.T) {
	ctx := context.Background()
	p, repo, _, target := cowFixture(t)
	main := string(repo.MainPath)
	directory := filepath.Join(main, "flagged")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, maxFlaggedUpdatePaths)
	fileSize := maxFlaggedUpdateBytes / maxFlaggedUpdatePaths
	base := bytes.Repeat([]byte{'a'}, fileSize)
	for i := range maxFlaggedUpdatePaths {
		path := filepath.Join("flagged", fmt.Sprintf("%02d", i))
		paths = append(paths, path)
		if err := os.WriteFile(filepath.Join(main, path), base, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cowGit(t, main, "add", "flagged")
	cowGit(t, main, "commit", "-m", "add boundary-sized flagged files")
	baseOID := cowGit(t, main, "rev-parse", "HEAD")
	if err := p.Prepare(ctx, repo, target, baseOID, testSlotID); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"update-index", "--skip-worktree", "--"}, paths...)
	cowGit(t, target, args...)

	updated := bytes.Repeat([]byte{'b'}, fileSize)
	for _, path := range paths {
		if err := os.WriteFile(filepath.Join(main, path), updated, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cowGit(t, main, "add", "flagged")
	cowGit(t, main, "commit", "-m", "update boundary-sized flagged files")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	if err := validateCollisionCandidate(ctx, p, repo, target, baseOID, newOID, nil, nil); err != nil {
		t.Fatalf("exact flag limits must stay eligible: %v", err)
	}
}

// stashFlaggedPaths も退避 byte 数の上限ちょうどを受理する。
// 事前検査後に同じ境界を拒否すると、書込み開始前の適格判定と更新処理が食い違う。
func TestStashFlaggedPathsAcceptsExactByteLimit(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "conf"), bytes.Repeat([]byte{'x'}, maxFlaggedUpdateBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	stashed, err := stashFlaggedPaths(root, []string{"conf"}, IndexFlags{SkipWorktree: []string{"conf"}})
	if err != nil {
		t.Fatalf("stash at the exact byte limit: %v", err)
	}
	if len(stashed) != 1 || len(stashed[0].data) != maxFlaggedUpdateBytes {
		t.Fatalf("stashed=%d entries/%d bytes, want 1/%d", len(stashed), len(stashed[0].data), maxFlaggedUpdateBytes)
	}
}

// flag 付き path が通常 file でない場合は、読み取りエラーへ潰さず形状違反として返す。
func TestStashFlaggedPathsRejectsNonRegularPath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "conf"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	_, err = stashFlaggedPaths(root, []string{"conf"}, IndexFlags{SkipWorktree: []string{"conf"}})
	if !errors.Is(err, ErrUpdateIneligible) || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("non-regular flagged path error=%v, want a shape error", err)
	}
}

// 複数 path の合計が上限を1 byteでも越える場合は、2件目を退避済みとして返さない。
func TestStashFlaggedPathsRejectsAggregateByteOverflow(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "first"), []byte{'a'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "second"), bytes.Repeat([]byte{'b'}, maxFlaggedUpdateBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	_, err = stashFlaggedPaths(root, []string{"first", "second"}, IndexFlags{SkipWorktree: []string{"first", "second"}})
	if !errors.Is(err, ErrUpdateIneligible) || !strings.Contains(err.Error(), "exceed the update limit") {
		t.Fatalf("aggregate flagged path error=%v, want the byte limit", err)
	}
}
