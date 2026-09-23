package archive

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// submoduleState は子の観測可能な状態で、保存前と復元後の一致を比較する単位である。
type submoduleState struct {
	status, head, headRef string
	files                 map[string]string
}

// readSubmoduleState は子の status・HEAD・作業ファイルを読む。
// status は snapshot の判定と同じ条件で取り、未追跡 file の有無も比較に含める。
func readSubmoduleState(t *testing.T, worktree string) submoduleState {
	t.Helper()
	child := filepath.Join(worktree, submodulePath)
	out := submoduleState{
		status: gitCommand(t, child, "-c", "core.quotePath=false", "status", "--porcelain=v2", "--untracked-files=all"),
		head:   gitCommand(t, child, "rev-parse", "HEAD"),
		files:  map[string]string{},
	}
	if command := exec.Command("git", "symbolic-ref", "-q", "HEAD"); true {
		command.Dir = child
		if output, err := command.Output(); err == nil {
			out.headRef = strings.TrimSpace(string(output))
		}
	}
	if err := filepath.WalkDir(child, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		content, err := os.ReadFile(path) // #nosec G304 -- fixture paths under the test temporary directory.
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(child, path)
		if err != nil {
			return err
		}
		out.files[filepath.ToSlash(relative)] = string(content)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (s submoduleState) equal(other submoduleState) bool {
	if s.status != other.status || s.head != other.head || s.headRef != other.headRef || len(s.files) != len(other.files) {
		return false
	}
	for path, content := range s.files {
		if other.files[path] != content {
			return false
		}
	}
	return true
}

// snapshotWithSubmodules は snapshot を取り、子の capsule を復元が受け取る形へ畳んで返す。
func snapshotWithSubmodules(t *testing.T, manager *Manager, repo discovery.Repository, worktree, sessionID string) (state.Snapshot, []state.SubmoduleSnapshot, []UnsavedSubmodule) {
	t.Helper()
	var capsules []SubmoduleCapsule
	snapshot, unsaved, err := manager.SnapshotWithPersistence(context.Background(), repo, worktree, sessionID, time.Now().Add(time.Hour),
		func(_ state.Snapshot, saved []SubmoduleCapsule) error {
			capsules = saved
			return nil
		})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	submodules := make([]state.SubmoduleSnapshot, 0, len(capsules))
	for _, capsule := range capsules {
		submodules = append(submodules, capsule.Snapshot(sessionID, string(repo.ID)))
	}
	return snapshot, submodules, unsaved
}

// 子の未 stage 編集・部分 stage・未追跡 file・未 push commit・停止中 rebase は、
// 親と同じ snapshot・resume 契約で別 path の slot へ戻る。
func TestSnapshotAndRestorePreservesSubmoduleWork(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(t *testing.T, worktree string)
	}{
		{
			name: "unstaged edit in the submodule",
			arrange: func(t *testing.T, worktree string) {
				writeFile(t, filepath.Join(worktree, submodulePath, "tracked.txt"), "edited\n")
			},
		},
		{
			name: "partially staged edit in the submodule",
			arrange: func(t *testing.T, worktree string) {
				submodule := filepath.Join(worktree, submodulePath)
				writeFile(t, filepath.Join(submodule, "tracked.txt"), "staged\n")
				gitCommand(t, submodule, "add", "tracked.txt")
				writeFile(t, filepath.Join(submodule, "tracked.txt"), "staged then edited\n")
			},
		},
		{
			name: "untracked file in the submodule",
			arrange: func(t *testing.T, worktree string) {
				writeFile(t, filepath.Join(worktree, submodulePath, "scratch.txt"), "note\n")
			},
		},
		{
			// `git add -f` で index へ入れた ignored file は `add -A` が飛ばすため、
			// 最後の未 stage 内容が worktree tree から落ちて復元で消えていた。
			name: "force-added ignored file edited after staging",
			arrange: func(t *testing.T, worktree string) {
				submodule := filepath.Join(worktree, submodulePath)
				writeFile(t, filepath.Join(submodule, ".gitignore"), "generated/\n")
				mustMkdir(t, filepath.Join(submodule, "generated"))
				writeFile(t, filepath.Join(submodule, "generated", "keep.txt"), "staged\n")
				gitCommand(t, submodule, "add", "-f", "generated/keep.txt")
				writeFile(t, filepath.Join(submodule, "generated", "keep.txt"), "working\n")
			},
		},
		{
			name: "commit that was never pushed",
			arrange: func(t *testing.T, worktree string) {
				commitInSubmodule(t, worktree)
			},
		},
		{
			name: "commit on a branch of the submodule",
			arrange: func(t *testing.T, worktree string) {
				gitCommand(t, filepath.Join(worktree, submodulePath), "checkout", "-b", "work")
				commitInSubmodule(t, worktree)
			},
		},
		{
			name: "rebase stopped inside the submodule",
			arrange: func(t *testing.T, worktree string) {
				commitInSubmodule(t, worktree)
				stopSubmoduleRebaseAtEdit(t, filepath.Join(worktree, submodulePath))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktree, repo, manager, worktreeRoot := submoduleFixture(t)
			test.arrange(t, worktree)
			want := readSubmoduleState(t, worktree)
			snapshot, submodules, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "session")
			if len(unsaved) != 0 {
				t.Fatalf("unsaved submodules=%+v, want none", unsaved)
			}
			if len(submodules) != 1 || submodules[0].Path != submodulePath {
				t.Fatalf("submodule snapshots=%+v, want one entry for %s", submodules, submodulePath)
			}
			target := restoreIntoNewSlot(t, manager, repo, worktreeRoot, "restored", snapshot, submodules)
			if got := readSubmoduleState(t, target); !got.equal(want) {
				t.Fatalf("restored submodule state=%+v, want %+v", got, want)
			}
		})
	}
}

// 無視されたままの未追跡 file は子の index に無いため、capsule の保存対象にしない。
// force-added な ignored file の救済が、単に無視された生成物まで巻き込まないことを固定する。
func TestSnapshotSkipsIgnoredFilesTheSubmoduleIndexDoesNotHold(t *testing.T) {
	worktree, repo, manager, worktreeRoot := submoduleFixture(t)
	submodule := filepath.Join(worktree, submodulePath)
	writeFile(t, filepath.Join(submodule, ".gitignore"), "generated/\n")
	mustMkdir(t, filepath.Join(submodule, "generated"))
	writeFile(t, filepath.Join(submodule, "generated", "scratch.txt"), "throwaway\n")
	snapshot, submodules, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "ignored-only")
	if len(unsaved) != 0 {
		t.Fatalf("unsaved submodules=%+v, want none", unsaved)
	}
	target := restoreIntoNewSlot(t, manager, repo, worktreeRoot, "restored", snapshot, submodules)
	restored := readSubmoduleState(t, target)
	if _, ok := restored.files["generated/scratch.txt"]; ok {
		t.Fatalf("restored an ignored file the submodule index never held: %+v", restored.files)
	}
	if restored.files[".gitignore"] != "generated/\n" {
		t.Fatalf("restored .gitignore=%q, want the saved content", restored.files[".gitignore"])
	}
}

// 親が gitlink を commit した子の commit は source のローカル module に無い。
// capsule がそれを置くため、復元の read-tree はこの条件でも通る。
func TestRestoreSubmoduleCommitMissingFromTheLocalModule(t *testing.T) {
	worktree, repo, manager, worktreeRoot := submoduleFixture(t)
	commitInSubmodule(t, worktree)
	gitCommand(t, worktree, "add", submodulePath)
	gitCommand(t, worktree, "commit", "-m", "bump submodule")
	childHead := gitCommand(t, filepath.Join(worktree, submodulePath), "rev-parse", "HEAD")
	localModule := filepath.Join(string(repo.CommonDir), "modules", filepath.FromSlash(submoduleName))
	want := readSubmoduleState(t, worktree)
	snapshot, submodules, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "missing-object")
	if len(unsaved) != 0 {
		t.Fatalf("unsaved submodules=%+v, want none", unsaved)
	}
	if output := gitCommand(t, localModule, "--git-dir=.", "cat-file", "-t", childHead); output != "commit" {
		t.Fatalf("the capsule did not place the child commit in the local module: %s", output)
	}
	target := restoreIntoNewSlot(t, manager, repo, worktreeRoot, "restored", snapshot, submodules)
	if got := readSubmoduleState(t, target); !got.equal(want) {
		t.Fatalf("restored submodule state=%+v, want %+v", got, want)
	}
	if gitlink := gitCommand(t, target, "rev-parse", "HEAD:"+submodulePath); gitlink != childHead {
		t.Fatalf("restored gitlink=%s, want %s", gitlink, childHead)
	}
}

// capsule の取り込みは source repository の HEAD・index・追跡 file を変えない。
// ソースリポジトリ不変の不変条件は保存経路でも成り立つ必要がある。
func TestSnapshotLeavesTheSourceRepositoryUnchangedWhileSavingSubmodules(t *testing.T) {
	worktree, repo, manager, _ := submoduleFixture(t)
	source := string(repo.MainPath)
	sourceChild := filepath.Join(source, submodulePath)
	before := map[string]string{
		"source status": gitCommand(t, source, "status", "--porcelain=v2", "--untracked-files=all"),
		"source head":   gitCommand(t, source, "rev-parse", "HEAD"),
		"source index":  gitCommand(t, source, "write-tree"),
		"child status":  gitCommand(t, sourceChild, "status", "--porcelain=v2", "--untracked-files=all"),
		"child head":    gitCommand(t, sourceChild, "rev-parse", "HEAD"),
	}
	commitInSubmodule(t, worktree)
	writeFile(t, filepath.Join(worktree, submodulePath, "scratch.txt"), "note\n")
	if _, submodules, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "source-unchanged"); len(unsaved) != 0 || len(submodules) != 1 {
		t.Fatalf("snapshot saved=%+v unsaved=%+v, want exactly one saved submodule", submodules, unsaved)
	}
	for label, want := range before {
		var got string
		switch label {
		case "source status":
			got = gitCommand(t, source, "status", "--porcelain=v2", "--untracked-files=all")
		case "source head":
			got = gitCommand(t, source, "rev-parse", "HEAD")
		case "source index":
			got = gitCommand(t, source, "write-tree")
		case "child status":
			got = gitCommand(t, sourceChild, "status", "--porcelain=v2", "--untracked-files=all")
		case "child head":
			got = gitCommand(t, sourceChild, "rev-parse", "HEAD")
		}
		if got != want {
			t.Fatalf("%s changed: got %q want %q", label, got, want)
		}
	}
}

// 保存できない条件は未保全の記録として残り、slot は自動回収から外れたままになる。
func TestSnapshotRecordsSubmoduleWorkItCannotSave(t *testing.T) {
	for _, test := range []struct {
		name    string
		arrange func(t *testing.T, worktree string, repo discovery.Repository)
		path    string
		want    string
	}{
		{
			name: "the source repository has no local module",
			arrange: func(t *testing.T, worktree string, repo discovery.Repository) {
				writeFile(t, filepath.Join(worktree, submodulePath, "tracked.txt"), "edited\n")
				moduleDir := filepath.Join(string(repo.CommonDir), "modules", filepath.FromSlash(submoduleName))
				if err := os.Rename(moduleDir, moduleDir+".moved"); err != nil {
					t.Fatal(err)
				}
			},
			path: submodulePath, want: ReasonModified,
		},
		{
			name: "the submodule index has unmerged entries",
			arrange: func(t *testing.T, worktree string, _ discovery.Repository) {
				conflictInSubmodule(t, worktree)
			},
			path: submodulePath, want: ReasonUnmergedIndex,
		},
		{
			name: "a nested submodule holds work",
			arrange: func(t *testing.T, worktree string, _ discovery.Repository) {
				addNestedSubmodule(t, worktree)
			},
			path: submodulePath + "/nested", want: ReasonUntracked,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			worktree, repo, manager, _ := submoduleFixture(t)
			test.arrange(t, worktree, repo)
			_, _, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "unsavable")
			index := slices.IndexFunc(unsaved, func(entry UnsavedSubmodule) bool { return entry.Path == test.path })
			if index < 0 {
				t.Fatalf("unsaved submodules=%+v, want an entry for %s", unsaved, test.path)
			}
			if !slices.Contains(unsaved[index].Reasons, test.want) {
				t.Fatalf("reasons for %s=%v, want to include %s", test.path, unsaved[index].Reasons, test.want)
			}
		})
	}
}

// restoreIntoNewSlot は別 path の slot へ復元し、復元先 worktree の path を返す。
func restoreIntoNewSlot(t *testing.T, manager *Manager, repo discovery.Repository, worktreeRoot, slotID string, snapshot state.Snapshot, submodules []state.SubmoduleSnapshot) string {
	t.Helper()
	target := filepath.Join(worktreeRoot, slotID, "root")
	pointAtSlot(t, manager, worktreeRoot, target)
	if err := manager.Restore(context.Background(), repo, target, slotID, snapshot, submodules); err != nil {
		t.Fatalf("restore: %v", err)
	}
	return target
}

// stopSubmoduleRebaseAtEdit は子の中で `rebase -i` を先頭の edit で停止させる。
// 子の gitdir は `.git` file 越しにあるため、停止したかは制御ファイルの実体ではなく rebase 状態の有無で確かめる。
func stopSubmoduleRebaseAtEdit(t *testing.T, child string) {
	t.Helper()
	editor := filepath.Join(t.TempDir(), "sequence-editor")
	script := "#!/bin/sh\nawk 'NR==1{sub(/^pick/,\"edit\")}1' \"$1\" > \"$1.wx\" && mv \"$1.wx\" \"$1\"\n"
	if err := os.WriteFile(editor, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "-c", "user.name=test", "-c", "user.email=test@example.com", "rebase", "-i", "HEAD~1")
	command.Dir = child
	command.Env = append(os.Environ(), "GIT_SEQUENCE_EDITOR="+editor)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start interactive rebase in the submodule: %v\n%s", err, output)
	}
	gitDir := gitCommand(t, child, "rev-parse", "--absolute-git-dir")
	if _, err := os.Lstat(filepath.Join(gitDir, "rebase-merge")); err != nil {
		t.Fatalf("the interactive rebase in the submodule did not stop: %v", err)
	}
}

// conflictInSubmodule は子の index に未解消の衝突を残す。
func conflictInSubmodule(t *testing.T, worktree string) {
	t.Helper()
	child := filepath.Join(worktree, submodulePath)
	identity := []string{"-c", "user.name=test", "-c", "user.email=test@example.com"}
	base := gitCommand(t, child, "rev-parse", "HEAD")
	gitCommand(t, child, "checkout", "-b", "theirs")
	writeFile(t, filepath.Join(child, "tracked.txt"), "theirs\n")
	gitCommand(t, child, append(append([]string{}, identity...), "commit", "-am", "theirs")...)
	gitCommand(t, child, "checkout", "-b", "ours", base)
	writeFile(t, filepath.Join(child, "tracked.txt"), "ours\n")
	gitCommand(t, child, append(append([]string{}, identity...), "commit", "-am", "ours")...)
	command := exec.Command("git", append(append([]string{}, identity...), "merge", "theirs")...)
	command.Dir = child
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("the merge in the submodule did not conflict:\n%s", output)
	}
}

// addNestedSubmodule は子の中へ入れ子 submodule を実体化し、その中に未追跡 file を置く。
// wx は `--recursive` で孫を実体化しないので、この配置は利用者が自分で行った状態を表す。
func addNestedSubmodule(t *testing.T, worktree string) {
	t.Helper()
	grandchild := filepath.Join(t.TempDir(), "grandchild")
	mustMkdir(t, grandchild)
	initRepository(t, grandchild, "leaf.txt")
	child := filepath.Join(worktree, submodulePath)
	gitCommand(t, child, "-c", "protocol.file.allow=always", "-c", "user.name=test", "-c", "user.email=test@example.com",
		"submodule", "add", grandchild, "nested")
	writeFile(t, filepath.Join(child, "nested", "scratch.txt"), "note\n")
}

// capsule ref はローカル module で公開され、snapshot の回収で親の ref と一緒に消える。
func TestDeleteSnapshotRefsRemovesTheSubmoduleCapsule(t *testing.T) {
	worktree, repo, manager, _ := submoduleFixture(t)
	commitInSubmodule(t, worktree)
	snapshot, submodules, unsaved := snapshotWithSubmodules(t, manager, repo, worktree, "collect")
	if len(unsaved) != 0 || len(submodules) != 1 {
		t.Fatalf("snapshot saved=%+v unsaved=%+v, want exactly one saved submodule", submodules, unsaved)
	}
	localModule := filepath.Join(string(repo.CommonDir), "modules", filepath.FromSlash(submoduleName))
	if got := gitCommand(t, localModule, "--git-dir=.", "rev-parse", "--verify", submodules[0].CapsuleRef); got != submodules[0].CapsuleOID {
		t.Fatalf("published capsule ref=%s, want %s", got, submodules[0].CapsuleOID)
	}
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot, submodules); err != nil {
		t.Fatalf("delete snapshot refs: %v", err)
	}
	refs := gitCommand(t, localModule, "--git-dir=.", "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
	if refs != "" {
		t.Fatalf("capsule refs left in the local module:\n%s", refs)
	}
	// 期限切れ GC の中断で ref が先に消えていても、回収はやり直せる。
	if err := manager.DeleteSnapshotRefs(context.Background(), repo, snapshot, submodules); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestDeleteSubmoduleCapsuleRefsReportsCompareAndDeleteRace は、確認後に ref が進んだとき
// compare-and-delete の失敗を返し、新しい target を残すことを確認する。
func TestDeleteSubmoduleCapsuleRefsReportsCompareAndDeleteRace(t *testing.T) {
	_, repo, manager, _ := archiveFixture(t)
	moduleDir := filepath.Join(string(repo.CommonDir), "modules", "child")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, moduleDir, "init", "--bare")
	ref := "refs/wx/recovery/session/repository/submodule/child"
	objectPath := filepath.Join(t.TempDir(), "capsule-object")
	if err := os.WriteFile(objectPath, []byte("capsule"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantOID := gitCommand(t, moduleDir, "--git-dir=.", "hash-object", "-w", objectPath)
	if err := os.WriteFile(objectPath, []byte("advanced capsule"), 0o600); err != nil {
		t.Fatal(err)
	}
	advancedOID := gitCommand(t, moduleDir, "--git-dir=.", "hash-object", "-w", objectPath)
	gitCommand(t, moduleDir, "update-ref", ref, wantOID)

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "replaced")
	wrapper := filepath.Join(bin, "git")
	script := `#!/bin/sh
set -eu
if [ "$1" = "--git-dir=." ] && [ "$2" = "update-ref" ] && [ "$3" = "-d" ] && [ "$4" = "$WX_CAPSULE_REF" ] && [ ! -e "$WX_CAPSULE_RACE_MARKER" ]; then
  : > "$WX_CAPSULE_RACE_MARKER"
  "$WX_REAL_GIT" --git-dir=. update-ref "$WX_CAPSULE_REF" "$WX_CAPSULE_ADVANCED_OID" "$WX_CAPSULE_EXPECTED_OID"
fi
exec "$WX_REAL_GIT" "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WX_REAL_GIT", gitPath)
	t.Setenv("WX_CAPSULE_REF", ref)
	t.Setenv("WX_CAPSULE_RACE_MARKER", marker)
	t.Setenv("WX_CAPSULE_ADVANCED_OID", advancedOID)
	t.Setenv("WX_CAPSULE_EXPECTED_OID", wantOID)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	snapshot := state.SubmoduleSnapshot{Name: "child", CapsuleRef: ref, CapsuleOID: wantOID}
	err = manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, []state.SubmoduleSnapshot{snapshot})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("race wrapper did not advance the capsule ref: %v", err)
	}
	if got := gitCommand(t, moduleDir, "--git-dir=.", "show-ref", "--verify", "--hash", ref); got != advancedOID {
		t.Fatalf("capsule ref after failed deletion=%s, want advanced target %s", got, advancedOID)
	}
	if err == nil {
		t.Fatal("compare-and-delete race was reported as success")
	}
}

func TestDeleteSubmoduleCapsuleRefsDeletesEveryRef(t *testing.T) {
	_, repo, manager, _ := archiveFixture(t)
	moduleDir := filepath.Join(string(repo.CommonDir), "modules", "child")
	if err := os.MkdirAll(moduleDir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, moduleDir, "init", "--bare")
	objectPath := filepath.Join(t.TempDir(), "capsule-object")
	if err := os.WriteFile(objectPath, []byte("capsule"), 0o600); err != nil {
		t.Fatal(err)
	}
	oid := gitCommand(t, moduleDir, "--git-dir=.", "hash-object", "-w", objectPath)
	refs := []string{
		"refs/wx/recovery/session/repository/submodule/first",
		"refs/wx/recovery/session/repository/submodule/second",
	}
	snapshots := make([]state.SubmoduleSnapshot, 0, len(refs))
	for _, ref := range refs {
		gitCommand(t, moduleDir, "update-ref", ref, oid)
		snapshots = append(snapshots, state.SubmoduleSnapshot{Name: "child", CapsuleRef: ref, CapsuleOID: oid})
	}

	if err := manager.deleteSubmoduleCapsuleRefs(context.Background(), repo, snapshots); err != nil {
		t.Fatalf("delete capsule refs: %v", err)
	}
	remaining := gitCommand(t, moduleDir, "--git-dir=.", "for-each-ref", "--format=%(refname)", "refs/wx/recovery")
	if remaining != "" {
		t.Fatalf("capsule refs left in the local module:\n%s", remaining)
	}
}
