package archive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// TestMutationAutoMergeTreeFailures は AUTO_MERGE の欠落・空値・不正 tree を
// conflict tree へ混入させず、存在確認済みの tree だけを採用することを確認する。
func TestMutationAutoMergeTreeFailures(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	tests := []struct {
		name      string
		content   *string
		runErr    error
		want      string
		wantCalls int
	}{
		{name: "missing control file", wantCalls: 0},
		{name: "empty control file", content: stringPtr("\n"), wantCalls: 0},
		{name: "missing tree object", content: stringPtr("tree\n"), runErr: errors.New("not a tree"), wantCalls: 1},
		{name: "verified tree object", content: stringPtr("tree\n"), want: "tree", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.Remove(filepath.Join(rootPath, "AUTO_MERGE")); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if test.content != nil {
				if err := os.WriteFile(filepath.Join(rootPath, "AUTO_MERGE"), []byte(*test.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			run := func(_ []string, _ []byte, args ...string) (gitx.Result, error) {
				calls++
				if test.runErr != nil {
					return gitx.Result{}, test.runErr
				}
				if len(args) != 3 || args[0] != "cat-file" || args[1] != "-e" || args[2] != "tree^{tree}" {
					t.Fatalf("unexpected command %q", args)
				}
				return gitx.Result{}, nil
			}
			if got := autoMergeTree(root, run); got != test.want {
				t.Fatalf("autoMergeTree=%q want %q", got, test.want)
			}
			if calls != test.wantCalls {
				t.Fatalf("cat-file calls=%d want %d", calls, test.wantCalls)
			}
		})
	}
}

// TestMutationConflictTreeRejectsDeletedStages は削除を表す conflict stage を
// recovery tree に書けないことと、通常 stage は実際に tree へ保存されることを固定する。
func TestMutationConflictTreeRejectsDeletedStages(t *testing.T) {
	rootPath := t.TempDir()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	valid := conflictIndexEntry{mode: "100644", oid: strings.Repeat("a", 40), stage: 2, path: "tracked"}
	deleted := []conflictIndexEntry{
		{mode: "0", oid: strings.Repeat("b", 40), stage: 2, path: "deleted-by-ours"},
		{mode: "100644", oid: emptyObjectOID, stage: 3, path: "deleted-by-theirs"},
	}
	for _, test := range []struct {
		name     string
		entries  []conflictIndexEntry
		wantErr  string
		wantTree bool
	}{
		{name: "mode zero", entries: deleted[:1], wantErr: "cannot preserve deleted conflict stage"},
		{name: "empty oid", entries: deleted[1:], wantErr: "cannot preserve deleted conflict stage"},
		{name: "regular stage", entries: []conflictIndexEntry{valid}, wantTree: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []string{}
			run := func(_ []string, _ []byte, args ...string) (gitx.Result, error) {
				calls = append(calls, strings.Join(args, " "))
				if len(args) > 0 && args[0] == "write-tree" {
					return gitx.Result{Stdout: "tree\n"}, nil
				}
				return gitx.Result{}, nil
			}
			got, err := writeConflictTree(root, run, test.entries)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error=%v want %q", err, test.wantErr)
				}
				return
			}
			if err != nil || !test.wantTree || got != "tree" {
				t.Fatalf("tree=%q err=%v", got, err)
			}
			if !containsMutationCommand(calls, "update-index --add --cacheinfo") {
				t.Fatalf("regular stage was not written: %v", calls)
			}
		})
	}
}

// TestMutationRestoreConflictIndexPreservesParseErrors は壊れた capsule listing を
// 空 tree として扱わず、update-index を開始する前に失敗させることを確認する。
func TestMutationRestoreConflictIndexPreservesParseErrors(t *testing.T) {
	calledUpdate := false
	run := func(_ []string, _ []byte, args ...string) (gitx.Result, error) {
		if len(args) > 0 && args[0] == "ls-tree" {
			return gitx.Result{Stdout: "malformed listing\x00"}, nil
		}
		if len(args) > 0 && args[0] == "update-index" {
			calledUpdate = true
		}
		return gitx.Result{}, nil
	}
	value := func([]string, ...string) (string, error) {
		t.Fatal("index verification must not run after a malformed capsule")
		return "", nil
	}
	err := restoreConflictIndex(value, run, "conflict-tree")
	if err == nil || !strings.Contains(err.Error(), "invalid conflict state tree record") {
		t.Fatalf("restoreConflictIndex error=%v", err)
	}
	if calledUpdate {
		t.Fatal("malformed capsule reached update-index")
	}
}

// TestMutationRestoreConflictIndexVerifiesTheReturnedIndex は update-index が
// 成功しても、直後の index 検証を壊れた listing のまま通過させないことを確認する。
func TestMutationRestoreConflictIndexVerifiesTheReturnedIndex(t *testing.T) {
	oid := strings.Repeat("a", 40)
	calledUpdate := false
	run := func(_ []string, _ []byte, args ...string) (gitx.Result, error) {
		if len(args) > 0 && args[0] == "ls-tree" {
			return gitx.Result{Stdout: "100644 blob " + oid + "\tstages/1/tracked\x00"}, nil
		}
		if len(args) > 0 && args[0] == "update-index" {
			calledUpdate = true
		}
		return gitx.Result{}, nil
	}
	value := func([]string, ...string) (string, error) {
		return "malformed index\x00", nil
	}
	err := restoreConflictIndex(value, run, "conflict-tree")
	if err == nil || !strings.Contains(err.Error(), "invalid Git index entry") {
		t.Fatalf("restoreConflictIndex error=%v", err)
	}
	if !calledUpdate {
		t.Fatal("restoreConflictIndex did not write the staged entries before verifying")
	}
}

// TestMutationConflictParsersKeepPathAndStageOrder は Git の conflict/index listing を
// path、stage の順に正規化し、復元時の比較を順序に依存させないことを確認する。
func TestMutationConflictParsersKeepPathAndStageOrder(t *testing.T) {
	oid := strings.Repeat("a", 40)
	index, err := parseIndexEntries(strings.Join([]string{
		"100644 " + oid + " 3\tb",
		"100644 " + oid + " 1\tb",
		"100644 " + oid + " 2\ta",
	}, "\x00") + "\x00")
	if err != nil {
		t.Fatal(err)
	}
	if len(index) != 3 || index[0].path != "a" || index[1].stage != 1 || index[2].stage != 3 {
		t.Fatalf("index order=%+v", index)
	}

	tree, err := parseConflictTreeEntries(strings.Join([]string{
		"100644 blob " + oid + "\tstages/3/b",
		"100644 blob " + oid + "\tstages/1/b",
		"100644 blob " + oid + "\tstages/2/a",
		"040000 tree " + oid + "\tauto-merge/tree",
	}, "\x00") + "\x00")
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 3 || tree[0].path != "a" || tree[1].stage != 1 || tree[2].stage != 3 {
		t.Fatalf("conflict tree order=%+v", tree)
	}
}

func stringPtr(value string) *string { return &value }

func containsMutationCommand(commands []string, want string) bool {
	for _, command := range commands {
		if strings.Contains(command, want) {
			return true
		}
	}
	return false
}
