package archive

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// TestMutationSnapshotLFSObjectsFailuresAndDedup は diff、cat-file batch、pointer
// parser の各失敗を snapshot へ返し、同じ blob OID は一度だけ読み取ることを確認する。
func TestMutationSnapshotLFSObjectsFailuresAndDedup(t *testing.T) {
	old := strings.Repeat("1", 40)
	oid := strings.Repeat("2", 40)
	pointerData := "version https://git-lfs.github.com/spec/v1\noid sha256:" + strings.Repeat("b", 64) + "\nsize 7\n"
	batch := oid + " blob " + strconv.Itoa(len(pointerData)) + "\n" + pointerData + "\n"
	diff := ":100644 100644 " + old + " " + oid + " M\x00weights-a.bin\x00" +
		":100644 100644 " + old + " " + oid + " M\x00weights-b.bin\x00"

	tests := []struct {
		name           string
		diff           string
		diffErr        error
		batch          string
		batchErr       error
		wantErr        string
		wantCandidates int
		wantCalls      int
		wantInput      string
		wantPaths      []string
	}{
		{name: "diff failure", diffErr: errors.New("diff failed"), wantErr: "list changed LFS paths"},
		{name: "malformed diff", diff: ":not-a-git-record\x00path\x00", wantErr: "invalid Git tree diff header"},
		{name: "no changes", diff: "", wantCalls: 1},
		{name: "cat-file failure", diff: diff, batchErr: errors.New("batch failed"), wantErr: "read changed LFS pointers", wantCalls: 2},
		{name: "malformed pointer batch", diff: diff, batch: oid + " malformed\n", wantErr: "invalid LFS pointer header", wantCalls: 2},
		{name: "deduplicated pointer batch", diff: diff, batch: batch, wantCandidates: 2, wantCalls: 2, wantInput: oid + "\n", wantPaths: []string{"weights-a.bin", "weights-b.bin"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var gotInput []byte
			run := func(_ []string, input []byte, args ...string) (gitx.Result, error) {
				if len(args) == 0 {
					t.Fatal("empty Git command")
				}
				switch args[0] {
				case "diff-tree":
					calls++
					return gitx.Result{Stdout: test.diff}, test.diffErr
				case "cat-file":
					calls++
					gotInput = append([]byte(nil), input...)
					return gitx.Result{Stdout: test.batch}, test.batchErr
				default:
					t.Fatalf("unexpected Git command %q", args)
					return gitx.Result{}, nil
				}
			}
			candidates, err := snapshotLFSObjects(run, []string{"GIT_INDEX_FILE=test"}, "HEAD", "WORKTREE")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error=%v want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != test.wantCandidates {
				t.Fatalf("candidate count=%d want %d", len(candidates), test.wantCandidates)
			}
			if test.wantInput != "" && string(gotInput) != test.wantInput {
				t.Fatalf("cat-file input=%q want %q", gotInput, test.wantInput)
			}
			if test.wantPaths != nil {
				for i, want := range test.wantPaths {
					if candidates[i].Path != want {
						t.Fatalf("candidate[%d].Path=%q want %q", i, candidates[i].Path, want)
					}
				}
			}
			if calls != test.wantCalls {
				t.Fatalf("Git commands=%d want %d", calls, test.wantCalls)
			}
		})
	}
}

// TestMutationParseSnapshotTreeDiffHandlesRenames は rename/copy の NUL path 2 件を
// 消費し、変更後の path を候補へ渡す parser の契約を固定する。
func TestMutationParseSnapshotTreeDiffHandlesRenames(t *testing.T) {
	old := strings.Repeat("1", 40)
	newOID := strings.Repeat("2", 40)
	output := ":100644 100644 " + old + " " + newOID + " R100\x00old.bin\x00new.bin\x00"
	changes, err := parseSnapshotTreeDiff(output)
	if err != nil || len(changes) != 1 || changes[0].path != "new.bin" || changes[0].newOID != newOID {
		t.Fatalf("rename changes=%+v err=%v", changes, err)
	}
	if _, err := parseSnapshotTreeDiff(":100644 100644 " + old + " " + newOID + " R100\x00old.bin\x00"); err == nil || !strings.Contains(err.Error(), "invalid Git tree rename record") {
		t.Fatalf("missing rename destination error=%v", err)
	}
	if _, err := parseSnapshotTreeDiff(":100644 100644 " + old + " " + newOID + " R100\x00old.bin"); err == nil || !strings.Contains(err.Error(), "invalid Git tree rename record") {
		t.Fatalf("unterminated rename destination error=%v", err)
	}
}

// TestMutationValidGitOIDRejectsMalformedHex は SHA-1/SHA-256 の長さだけでなく、
// hex 形式も検証して binary や短い OID を Git object として扱わないことを確認する。
func TestMutationValidGitOIDRejectsMalformedHex(t *testing.T) {
	for _, test := range []struct {
		name string
		oid  string
		want bool
	}{
		{name: "sha1", oid: strings.Repeat("a", 40), want: true},
		{name: "sha256", oid: strings.Repeat("b", 64), want: true},
		{name: "wrong length", oid: strings.Repeat("a", 39)},
		{name: "invalid hex", oid: strings.Repeat("g", 40)},
		{name: "mixed invalid hex", oid: strings.Repeat("a", 39) + "z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validGitOID(test.oid); got != test.want {
				t.Fatalf("validGitOID(%q)=%v want %v", test.oid, got, test.want)
			}
		})
	}
}
