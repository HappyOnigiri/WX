package metacache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func testCache(t *testing.T) (*Cache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache", "sessions.db")
	cache, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache, path
}

func validator() Validator {
	return Validator{Identity: "vol:1:/", Size: 10, MtimeNS: 20, CtimeNS: 30, ParserVersion: 1}
}

// lookup は Snapshot 経由で 1 件を引き、検証値が一致するときだけ hit として返す。
func lookup(t *testing.T, cache *Cache, agent, path string, want Validator) (Record, bool) {
	t.Helper()
	entry, ok := cache.Snapshot(context.Background(), agent)[path]
	if !ok || entry.Validator != want {
		return Record{}, false
	}
	return entry.Record, true
}

func save(t *testing.T, cache *Cache, agent, path string, validator Validator, record Record) {
	t.Helper()
	cache.Save(context.Background(), agent, map[string]Entry{path: {Validator: validator, Record: record}})
}

func TestCacheReusesOnlyFullyMatchingAttributes(t *testing.T) {
	cache, _ := testCache(t)
	base := validator()
	record := Record{NativeID: "native", Title: "タイトル", CWD: "/workspace"}
	save(t, cache, "claude", "/history/a.jsonl", base, record)

	got, ok := lookup(t, cache, "claude", "/history/a.jsonl", base)
	if !ok || got != record {
		t.Fatalf("lookup = %+v ok=%v, want the saved record", got, ok)
	}
	for name, changed := range map[string]Validator{
		"identity": {Identity: "vol:2:/", Size: 10, MtimeNS: 20, CtimeNS: 30, ParserVersion: 1},
		"size":     {Identity: "vol:1:/", Size: 11, MtimeNS: 20, CtimeNS: 30, ParserVersion: 1},
		"mtime":    {Identity: "vol:1:/", Size: 10, MtimeNS: 21, CtimeNS: 30, ParserVersion: 1},
		"ctime":    {Identity: "vol:1:/", Size: 10, MtimeNS: 20, CtimeNS: 31, ParserVersion: 1},
		"parser":   {Identity: "vol:1:/", Size: 10, MtimeNS: 20, CtimeNS: 30, ParserVersion: 2},
	} {
		if _, ok := lookup(t, cache, "claude", "/history/a.jsonl", changed); ok {
			t.Errorf("changed %s was reused", name)
		}
	}
	if _, ok := lookup(t, cache, "codex", "/history/a.jsonl", base); ok {
		t.Error("another agent reused the same path")
	}

	// 除外の判定も保存し、次回の解析を省く。
	save(t, cache, "claude", "/history/b.jsonl", base, Record{Excluded: true})
	if got, ok := lookup(t, cache, "claude", "/history/b.jsonl", base); !ok || !got.Excluded {
		t.Fatalf("excluded lookup = %+v ok=%v", got, ok)
	}
}

func TestCachePruneRemovesOnlyUnseenEntriesUnderRoot(t *testing.T) {
	cache, _ := testCache(t)
	ctx := context.Background()
	base := validator()
	paths := []string{"/history/root/keep.jsonl", "/history/root/gone.jsonl", "/history/other/keep.jsonl"}
	for _, path := range paths {
		save(t, cache, "claude", path, base, Record{NativeID: "native", Title: "t"})
	}
	cache.Prune(ctx, "claude", "/history/root", map[string]struct{}{"/history/root/keep.jsonl": {}})
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/history/root/keep.jsonl", true}, {"/history/root/gone.jsonl", false}, {"/history/other/keep.jsonl", true},
	} {
		if _, ok := lookup(t, cache, "claude", tc.path, base); ok != tc.want {
			t.Errorf("%s present=%v, want %v", tc.path, ok, tc.want)
		}
	}
}

func TestOpenRecreatesCorruptedDatabaseAndReportsUnusablePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache, err := Open(path)
	if err != nil {
		t.Fatalf("Open corrupted: %v", err)
	}
	save(t, cache, "claude", "/history/a.jsonl", validator(), Record{NativeID: "native", Title: "t"})
	if _, ok := lookup(t, cache, "claude", "/history/a.jsonl", validator()); !ok {
		t.Fatal("recreated cache did not store the record")
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	// path を作れない場所では error を返し、呼び出し側は cache 無しで走査を続ける。
	if _, err := Open(filepath.Join(path, "nested", "sessions.db")); err == nil {
		t.Fatal("Open under a regular file unexpectedly succeeded")
	}
}

func TestNilCacheAndClosedCacheStayHarmless(t *testing.T) {
	ctx := context.Background()
	var absent *Cache
	if _, ok := lookup(t, absent, "claude", "/history/a.jsonl", validator()); ok {
		t.Fatal("nil cache reported a hit")
	}
	absent.Save(ctx, "claude", map[string]Entry{"/history/a.jsonl": {}})
	absent.Prune(ctx, "claude", "/history", nil)
	if err := absent.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}

	cache, _ := testCache(t)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	// 書き込みに失敗した cache はそれ以降使わず、読み取りも外れ続ける。
	save(t, cache, "claude", "/history/a.jsonl", validator(), Record{NativeID: "native", Title: "t"})
	cache.Prune(ctx, "claude", "/history", nil)
	if _, ok := lookup(t, cache, "claude", "/history/a.jsonl", validator()); ok {
		t.Fatal("closed cache reported a hit")
	}
}

func TestDefaultPathLivesUnderTheUserCacheDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "wx", "sessions.db") {
		t.Fatalf("DefaultPath = %q, want a wx-specific file under %q", path, dir)
	}
}
