package scanner

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/metacache"
)

func cachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache", "sessions.db")
}

func openCache(t *testing.T, path string) *metacache.Cache {
	t.Helper()
	cache, err := metacache.Open(path)
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func writeClaude(t *testing.T, root, id, title string) string {
	t.Helper()
	path := filepath.Join(root, id+".jsonl")
	body := `{"type":"user","cwd":"/workspace","message":{"content":"` + title + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// claudeRoot は Claude 履歴だけを持つ走査先の設定を返す。Codex 側は存在しない path にして無効化する。
func claudeRoot(t *testing.T) (string, config.Config) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{root}
	cfg.Paths.Codex.Sessions = []string{filepath.Join(root, "absent-codex")}
	return root, cfg
}

func scanWithCache(t *testing.T, cfg config.Config, cache *metacache.Cache) ([]Session, Stats) {
	t.Helper()
	stats := Stats{}
	sessions, err := ScanWith(context.Background(), cfg, Options{Cache: cache, Stats: &stats}, "claude")
	if err != nil {
		t.Fatalf("ScanWith: %v", err)
	}
	return sessions, stats
}

func TestScanReusesUnchangedMetadataAcrossInvocations(t *testing.T) {
	root, cfg := claudeRoot(t)
	first := writeClaude(t, root, "11111111-1111-4111-8111-111111111111", "First")
	writeClaude(t, root, "22222222-2222-4222-8222-222222222222", "Second")
	cache := openCache(t, cachePath(t))

	cold, coldStats := scanWithCache(t, cfg, cache)
	if coldStats.Enumerated != 2 || coldStats.Parsed != 2 || coldStats.Reused != 0 {
		t.Fatalf("cold stats = %+v, want 2 enumerated and 2 parsed", coldStats)
	}
	warm, warmStats := scanWithCache(t, cfg, cache)
	if warmStats.Enumerated != 2 || warmStats.Parsed != 0 || warmStats.Reused != 2 {
		t.Fatalf("warm stats = %+v, want 0 parsed and 2 reused", warmStats)
	}
	if !reflect.DeepEqual(cold, warm) {
		t.Fatalf("warm sessions = %+v, want the cold result %+v", warm, cold)
	}

	// 追記は size と mtime が変わるため再解析される。
	file, err := os.OpenFile(first, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"type":"user","cwd":"/workspace","message":{"content":"Later"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, stats := scanWithCache(t, cfg, cache); stats.Parsed != 1 || stats.Reused != 1 {
		t.Fatalf("append stats = %+v, want 1 parsed", stats)
	}

	// 新規ファイルは解析され、既存は再利用される。
	writeClaude(t, root, "33333333-3333-4333-8333-333333333333", "Third")
	added, stats := scanWithCache(t, cfg, cache)
	if stats.Parsed != 1 || stats.Reused != 2 || len(added) != 3 {
		t.Fatalf("new history stats = %+v sessions=%d, want 1 parsed and 3 sessions", stats, len(added))
	}
}

func TestScanDetectsSameSizeAndMtimeReplacement(t *testing.T) {
	root, cfg := claudeRoot(t)
	id := "44444444-4444-4444-8444-444444444444"
	path := writeClaude(t, root, id, "Alpha")
	cache := openCache(t, cachePath(t))
	scanWithCache(t, cfg, cache)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 同じ inode を同じ長さで上書きし mtime も戻す。ctime だけが変わる置換を検出できなければ古いタイトルが残る。
	writeClaude(t, root, id, "Omega")
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	replaced, stats := scanWithCache(t, cfg, cache)
	if stats.Parsed != 1 || len(replaced) != 1 || replaced[0].Title != "Omega" {
		t.Fatalf("replacement stats = %+v sessions = %+v, want the new title", stats, replaced)
	}
}

func TestScanReflectsRenameAndRemoval(t *testing.T) {
	root, cfg := claudeRoot(t)
	old := writeClaude(t, root, "55555555-5555-4555-8555-555555555555", "Renamed")
	keep := writeClaude(t, root, "66666666-6666-4666-8666-666666666666", "Kept")
	cache := openCache(t, cachePath(t))
	scanWithCache(t, cfg, cache)

	renamed := filepath.Join(root, "77777777-7777-4777-8777-777777777777.jsonl")
	if err := os.Rename(old, renamed); err != nil {
		t.Fatal(err)
	}
	sessions, stats := scanWithCache(t, cfg, cache)
	if stats.Parsed != 1 || stats.Reused != 1 || len(sessions) != 2 {
		t.Fatalf("rename stats = %+v sessions=%d, want the renamed file parsed", stats, len(sessions))
	}
	ids := map[string]bool{}
	for _, session := range sessions {
		ids[session.SessionID] = true
	}
	if !ids["77777777-7777-4777-8777-777777777777"] || ids["55555555-5555-4555-8555-555555555555"] {
		t.Fatalf("session IDs = %v, want only the new file name", ids)
	}

	if err := os.Remove(keep); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := scanWithCache(t, cfg, cache); len(sessions) != 1 {
		t.Fatalf("after removal sessions = %+v, want 1", sessions)
	}
}

func TestScanKeepsCacheEntriesWhenRootScanDoesNotComplete(t *testing.T) {
	root, cfg := claudeRoot(t)
	writeClaude(t, root, "88888888-8888-4888-8888-888888888888", "Kept")
	cache := openCache(t, cachePath(t))
	scanWithCache(t, cfg, cache)

	// 完走しなかった走査は未走査の entry を消してはいけない。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := &scanState{opts: Options{Cache: cache}, agent: "claude", entries: cache.Snapshot(ctx, "claude"), volumes: map[string]string{}}
	if _, err := scanRoot(ctx, root, state, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scanRoot = %v, want context.Canceled", err)
	}
	if _, stats := scanWithCache(t, cfg, cache); stats.Reused != 1 {
		t.Fatalf("stats after cancellation = %+v, want the entry reused", stats)
	}
}

func TestScanFallsBackWhenCacheIsCorruptedOrUnusable(t *testing.T) {
	root, cfg := claudeRoot(t)
	writeClaude(t, root, "99999999-9999-4999-8999-999999999999", "Fallback")
	path := cachePath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 壊れた cache は作り直され、会話は黙って欠落しない。
	cache := openCache(t, path)
	if sessions, stats := scanWithCache(t, cfg, cache); len(sessions) != 1 || stats.Parsed != 1 {
		t.Fatalf("corrupted cache sessions = %+v stats = %+v", sessions, stats)
	}

	// 書き込みも読み取りもできない cache では、毎回解析しつつ結果は変わらない。
	closed, err := metacache.Open(cachePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		sessions, stats := scanWithCache(t, cfg, closed)
		if len(sessions) != 1 || stats.Parsed != 1 || stats.Reused != 0 {
			t.Fatalf("unusable cache sessions = %+v stats = %+v", sessions, stats)
		}
	}
}

func TestScanIgnoresEntriesFromAnotherCacheSchema(t *testing.T) {
	root, cfg := claudeRoot(t)
	writeClaude(t, root, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "Schema")
	path := cachePath(t)
	cache := openCache(t, path)
	scanWithCache(t, cfg, cache)
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE cache_meta SET value = value + 1 WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openCache(t, path)
	sessions, stats := scanWithCache(t, cfg, reopened)
	if len(sessions) != 1 || stats.Parsed != 1 || stats.Reused != 0 {
		t.Fatalf("schema change sessions = %+v stats = %+v, want a cold scan", sessions, stats)
	}
}

func TestScanSharesOneCacheBetweenConcurrentScans(t *testing.T) {
	root, cfg := claudeRoot(t)
	for _, id := range []string{"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"} {
		writeClaude(t, root, id, "Concurrent")
	}
	path := cachePath(t)
	first := openCache(t, path)
	second := openCache(t, path)
	done := make(chan []Session, 2)
	for _, cache := range []*metacache.Cache{first, second} {
		go func() {
			sessions, err := ScanWith(context.Background(), cfg, Options{Cache: cache}, "claude")
			if err != nil {
				done <- nil
				return
			}
			done <- sessions
		}()
	}
	for range 2 {
		if sessions := <-done; len(sessions) != 2 {
			t.Fatalf("concurrent scan sessions = %+v, want 2", sessions)
		}
	}
}

func TestFindNarrowsClaudeCandidatesByFileName(t *testing.T) {
	root, cfg := claudeRoot(t)
	want := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	writeClaude(t, root, want, "Wanted")
	writeClaude(t, root, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee", "Other")
	writeClaude(t, root, "ffffffff-ffff-4fff-8fff-ffffffffffff", "Another")

	stats := Stats{}
	session, found, err := Find(context.Background(), cfg, Options{Stats: &stats}, "claude", want)
	if err != nil || !found || session.Title != "Wanted" {
		t.Fatalf("Find = %+v found=%v err=%v", session, found, err)
	}
	if stats.Enumerated != 3 || stats.Parsed != 1 {
		t.Fatalf("Find stats = %+v, want 3 enumerated and 1 parsed", stats)
	}
	if _, found, err := Find(context.Background(), cfg, Options{}, "claude", "missing"); err != nil || found {
		t.Fatalf("missing ID found=%v err=%v", found, err)
	}
	if _, found, err := Find(context.Background(), cfg, Options{}, "claude", ""); err != nil || found {
		t.Fatalf("empty ID found=%v err=%v", found, err)
	}
	if _, _, err := Find(context.Background(), cfg, Options{}, "cursor", want); err == nil {
		t.Fatal("unsupported agent accepted")
	}
}

func TestFindPrefersLatestDuplicateAndUsesCachedCodexIDs(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	codex := filepath.Join(root, "codex")
	for _, dir := range []string{first, second, codex} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	id := "duplicate-native"
	older := writeClaude(t, first, id, "Old")
	newer := writeClaude(t, second, id, "New")
	if err := os.Chtimes(older, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, time.Unix(200, 0), time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{first, second}
	cfg.Paths.Codex.Sessions = []string{codex}
	session, found, err := Find(context.Background(), cfg, Options{}, "claude", id)
	if err != nil || !found || session.Title != "New" || session.RawPath != newer {
		t.Fatalf("duplicate Find = %+v found=%v err=%v", session, found, err)
	}

	// ファイル名に ID を含まない Codex 履歴は payload から ID を取るため、候補外と決められず解析される。
	payloadID := "019e8bd5-4230-7403-b1aa-b48f42e564dc"
	body := `{"type":"session_meta","payload":{"id":"` + payloadID + `","cwd":"/tmp/codex"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Codex title"}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(codex, "rollout-without-id.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := openCache(t, cachePath(t))
	stats := Stats{}
	session, found, err = Find(context.Background(), cfg, Options{Cache: cache, Stats: &stats}, "codex", payloadID)
	if err != nil || !found || session.Title != "Codex title" || stats.Parsed != 1 {
		t.Fatalf("payload-only Find = %+v found=%v err=%v stats=%+v", session, found, err, stats)
	}
	// 温まった cache は payload の ID を確定させるので、別 ID の探索では解析しない。
	stats = Stats{}
	if _, found, err := Find(context.Background(), cfg, Options{Cache: cache, Stats: &stats}, "codex", "unrelated"); err != nil || found {
		t.Fatalf("unrelated Find found=%v err=%v", found, err)
	}
	if stats.Parsed != 0 || stats.Reused != 1 {
		t.Fatalf("cached Codex narrowing stats = %+v, want 0 parsed", stats)
	}
}

// BenchmarkScanLargeHistory は大量履歴での列挙数・解析数と所要時間を cache の有無で比べる。
// -bench で走らせたときだけ計測し、列挙数と解析数は各 sub-benchmark の metric として報告する。
func BenchmarkScanLargeHistory(b *testing.B) {
	root := b.TempDir()
	cfg := config.Defaults()
	cfg.Paths.Claude.Sessions = []string{root}
	cfg.Paths.Codex.Sessions = []string{filepath.Join(root, "absent-codex")}
	body := []byte(`{"type":"user","cwd":"/workspace","message":{"content":"benchmark title"}}` + "\n")
	const files = 2000
	for i := range files {
		name := filepath.Join(root, "session-"+strconv.Itoa(i)+".jsonl")
		if err := os.WriteFile(name, body, 0o600); err != nil {
			b.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		cache bool
	}{{"direct", false}, {"cached", true}} {
		b.Run(tc.name, func(b *testing.B) {
			var cache *metacache.Cache
			if tc.cache {
				opened, err := metacache.Open(filepath.Join(b.TempDir(), "sessions.db"))
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = opened.Close() }()
				cache = opened
				if _, err := ScanWith(context.Background(), cfg, Options{Cache: cache}, "claude"); err != nil {
					b.Fatal(err)
				}
			}
			stats := Stats{}
			b.ResetTimer()
			for range b.N {
				stats = Stats{}
				if _, err := ScanWith(context.Background(), cfg, Options{Cache: cache, Stats: &stats}, "claude"); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(stats.Enumerated), "enumerated")
			b.ReportMetric(float64(stats.Parsed), "parsed")
		})
	}
}
