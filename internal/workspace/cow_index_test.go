package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
)

func cowIndexOutput(lines ...string) string {
	return strings.Join(lines, "\x00") + "\x00"
}

func TestCOWIndexEntriesKeepShareableBlobs(t *testing.T) {
	t.Parallel()
	stdout := cowIndexOutput(
		"100644 aaa 0\tfile",
		"100755 bbb 0\tdir/script",
		"120000 ccc 0\tlink",
		"160000 ddd 0\tmodule",
		"100644 eee 1\tconflict",
	)
	entries, err := parseCOWIndexEntries(stdout)
	if err != nil {
		t.Fatal(err)
	}
	want := []cowIndexEntry{{name: "file", oid: "aaa"}, {name: "dir/script", oid: "bbb"}}
	if len(entries) != len(want) {
		t.Fatalf("entries=%v", entries)
	}
	for index, entry := range entries {
		if entry != want[index] {
			t.Fatalf("entry %d = %v, want %v", index, entry, want[index])
		}
	}
}

// 宛先 path の逸脱は skip では済まないため、解析の時点で失敗させる。
func TestCOWIndexEntriesRejectUnsafeOutput(t *testing.T) {
	t.Parallel()
	for name, stdout := range map[string]string{
		"missing_tab":   cowIndexOutput("100644 aaa 0 file"),
		"short_header":  cowIndexOutput("100644 aaa\tfile"),
		"parent_escape": cowIndexOutput("100644 aaa 0\t../outside"),
		"absolute":      cowIndexOutput("100644 aaa 0\t/etc/passwd"),
		"unclean":       cowIndexOutput("100644 aaa 0\t./file"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCOWIndexEntries(stdout); err == nil {
				t.Fatal("unsafe index output was accepted")
			}
		})
	}
}

// main 側は事前 skip の材料でしかないので、解釈できない行は表から落として共有対象外へ倒す。
func TestCOWSourceIndexOIDsDropUnparsableEntries(t *testing.T) {
	t.Parallel()
	oids := parseCOWSourceIndexOIDs(cowIndexOutput(
		"100644 aaa 0\tfile",
		"100644 bbb 0 broken",
		"120000 ccc 0\tlink",
		"100644 ddd 0\t../outside",
	))
	if len(oids) != 2 || oids["file"] != "aaa" || oids["../outside"] != "ddd" {
		t.Fatalf("oids=%v", oids)
	}
}

func TestCOWCandidatesKeepMatchingOIDs(t *testing.T) {
	t.Parallel()
	entries := []cowIndexEntry{
		{name: "same", oid: "aaa"},
		{name: "changed", oid: "bbb"},
		{name: "missing", oid: "ccc"},
	}
	candidates := selectCOWCandidates(entries, map[string]string{"same": "aaa", "changed": "zzz"})
	if len(candidates) != 1 || candidates[0].name != "same" {
		t.Fatalf("candidates=%v", candidates)
	}
	// 表が無い回は事前 skip を行わず、従来どおり全 entry を走査する。
	if got := selectCOWCandidates(entries, nil); len(got) != len(entries) {
		t.Fatalf("candidates without a source index=%v", got)
	}
}

// main 側 index の blob が違う path は、内容が一致していても clone せずに除外する。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestCOWSkipsPathsWithADifferentSourceIndexBlob(t *testing.T) {
	p, repo, oid, target := cowFixture(t)
	p.Config.Storage.CopyMode = config.CopyModeCopy
	if err := p.Prepare(context.Background(), repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(string(repo.MainPath), "file")
	if err := os.WriteFile(source, []byte(cowBody+"staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cowGit(t, string(repo.MainPath), "add", "file")
	// index だけを進めて作業ツリーは宛先と同内容へ戻す。bytes は一致するが index の blob は一致しない。
	if err := os.WriteFile(source, []byte(cowBody), 0o644); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	var before, after unix.Stat_t
	if err := unix.Stat(filepath.Join(target, "file"), &before); err != nil {
		t.Fatal(err)
	}
	if err := p.compactOwnedWorktree(context.Background(), repo, target, oid, testSlotID, preparePhaseCreate, identity, nil); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Join(target, "file"), &after); err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino {
		t.Fatalf("a path with a different source index blob was shared: %d -> %d", before.Ino, after.Ino)
	}
}

// scope は今回の更新が書き直した path の entry だけを候補にする。
// nil は限定なしで、宛先に共有済みの実体を持たない新規準備と復元がこの経路を使う。
func TestCOWScopeNarrowsCandidatesToRewrittenPaths(t *testing.T) {
	t.Parallel()
	entries := []cowIndexEntry{{name: "rewritten", oid: "aaa"}, {name: "dir/kept", oid: "bbb"}}
	scope := &cowScope{rewritten: map[string]bool{"rewritten": true, "absent": true}}
	candidates := scope.narrow(entries)
	if len(candidates) != 1 || candidates[0].name != "rewritten" {
		t.Fatalf("candidates=%v", candidates)
	}
	if got := (&cowScope{}).narrow(entries); len(got) != 0 {
		t.Fatalf("an empty scope kept candidates=%v", got)
	}
	var unlimited *cowScope
	if got := unlimited.narrow(entries); len(got) != len(entries) {
		t.Fatalf("candidates without a scope=%v", got)
	}
}
