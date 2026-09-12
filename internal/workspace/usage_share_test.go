package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

const usageShareName = "workspace/slot/repo/file"

// usageShareTask は共有判定だけを直接呼ぶための scan と、repository 直下の directory task を組む。
func usageShareTask(t *testing.T, root *os.Root, mainPath string, previous SharedFileCache) (*usageScan, usageDirectory) {
	t.Helper()
	targets := []SlotUsageTarget{{SlotID: "slot", RelPath: "workspace/slot", Repositories: map[string]string{"repo": mainPath}}}
	scan := newUsageScan(context.Background(), targets, previous)
	dir, err := openUsageDirectory(root, "workspace/slot/repo")
	if err != nil {
		t.Fatal(err)
	}
	main := openUsageRepository(mainPath)
	if main == nil {
		t.Fatal("main worktree could not be pinned")
	}
	task := usageDirectory{name: "workspace/slot/repo", slotID: "slot", repo: true, dir: dir, main: main}
	t.Cleanup(task.close)
	return scan, task
}

// usageShareStat は descriptor 相対に leaf を見る。共有判定へ渡す実体はこの観測である。
func usageShareStat(t *testing.T, dir *os.File, leaf string) *unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	return &stat
}

// size が違えば offset を比べるまでもなく共有していないので、判定だけを cache へ残す。
func TestSharedLeafRecordsASizeMismatchWithoutComparingOffsets(t *testing.T) {
	t.Parallel()
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "slot")
	usageWrite(t, mainPath, "file", "main content")
	scan, task := usageShareTask(t, root, mainPath, nil)

	state, decided := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file"))
	if !decided || state.Shared {
		t.Fatalf("state=%+v decided=%v", state, decided)
	}
}

func TestSharedLeafReusesTheVerdictWhileBothSidesAreUnchanged(t *testing.T) {
	t.Parallel()
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "same size ok")
	usageWrite(t, mainPath, "file", "same size ok")
	_, task := usageShareTask(t, root, mainPath, nil)
	cached := SharedFileState{
		Slot:   fileIdentityOf(usageShareStat(t, task.dir, "file")),
		Source: fileIdentityOf(usageShareStat(t, task.main, "file")),
		Shared: true,
	}

	// 両側の identity が同じ間は前回の判定をそのまま返す。実測なら別々に書いた実体は共有なしになる。
	scan, task := usageShareTask(t, root, mainPath, SharedFileCache{usageShareName: cached})
	state, decided := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file"))
	if !decided || state != cached {
		t.Fatalf("state=%+v decided=%v want=%+v", state, decided, cached)
	}
	for label, stale := range map[string]SharedFileState{"slot": usageShareStale(cached, true), "source": usageShareStale(cached, false)} {
		scan, task := usageShareTask(t, root, mainPath, SharedFileCache{usageShareName: stale})
		if state, _ := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file")); state.Shared {
			t.Fatalf("stale %s cache entry was trusted: %+v", label, state)
		}
	}
}

// usageShareStale は片側の ctime だけを進めた cache entry を返す。
func usageShareStale(state SharedFileState, slotSide bool) SharedFileState {
	if slotSide {
		state.Slot.CtimeNanos++
		return state
	}
	state.Source.CtimeNanos++
	return state
}

// 共有元を検証できない回は共有なしとして扱い、その回の判定を cache へ残さない。
func TestSharedLeafDoesNotCacheAnUnverifiableSource(t *testing.T) {
	t.Parallel()
	root, mainPath, _ := usageRoots(t)
	usageWrite(t, filepath.Join(root.Name(), "workspace", "slot", "repo"), "file", "shared content")
	usageWrite(t, mainPath, "target", "shared content")
	if err := os.Symlink("target", filepath.Join(mainPath, "file")); err != nil {
		t.Fatal(err)
	}
	scan, task := usageShareTask(t, root, mainPath, nil)
	if state, decided := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file")); decided || state.Shared {
		t.Fatalf("symlink source was cached: %+v decided=%v", state, decided)
	}

	if err := os.Remove(filepath.Join(mainPath, "file")); err != nil {
		t.Fatal(err)
	}
	if state, decided := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file")); decided || state.Shared {
		t.Fatalf("missing source was cached: %+v decided=%v", state, decided)
	}

	// 共有元 directory を開けなかった subtree も、共有なしのまま判定を残さない。
	task.main = nil
	if state, decided := scan.sharedLeaf(task, "file", usageShareName, usageShareStat(t, task.dir, "file")); decided || state.Shared {
		t.Fatalf("subtree without a source was cached: %+v decided=%v", state, decided)
	}
}

// usageShareOpen は識別の対象として leaf を読み取り専用で開く。
func usageShareOpen(t *testing.T, dir, name, data string) *os.File {
	t.Helper()
	usageWrite(t, dir, name, data)
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// 比較に使った descriptor が観測時の実体のままなら一致とみなし、片側でも食い違えば判定を捨てる。
// offset の比較が成立しない linux でも到達するよう、関数を直接呼ぶ。
func TestSameUsageIdentitiesRejectsAnyMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source, target := usageShareOpen(t, dir, "source", "content"), usageShareOpen(t, dir, "target", "content")
	sourceIdentity, err := usageFileIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	targetIdentity, err := usageFileIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	state := SharedFileState{Slot: targetIdentity, Source: sourceIdentity}
	if !sameUsageIdentities(source, target, state) {
		t.Fatalf("unchanged descriptors were rejected: %+v", state)
	}
	for label, stale := range map[string]SharedFileState{"slot": usageShareStale(state, true), "source": usageShareStale(state, false)} {
		if sameUsageIdentities(source, target, stale) {
			t.Fatalf("stale %s identity was accepted: %+v", label, stale)
		}
	}

	// fstat できない descriptor は実体を確かめられないので、共有と認めない。
	closed := usageShareOpen(t, dir, "closed", "content")
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if sameUsageIdentities(closed, target, state) || sameUsageIdentities(source, closed, state) {
		t.Fatal("a closed descriptor was accepted")
	}
}
