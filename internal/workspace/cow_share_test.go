package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestCOWRunsGroupConsecutiveDirectories(t *testing.T) {
	t.Parallel()
	runs := splitCOWRuns([]cowIndexEntry{
		{name: "a"},
		{name: "b"},
		{name: "dir/one"},
		{name: "dir/two"},
		{name: "dir/three"},
		{name: "other/one"},
	})
	if len(runs) != 3 {
		t.Fatalf("runs=%v", runs)
	}
	if runs[0].directory != "." || len(runs[0].leaves) != 2 {
		t.Fatalf("first run=%v", runs[0])
	}
	if runs[1].directory != "dir" || runs[1].leaves[2] != "three" {
		t.Fatalf("second run=%v", runs[1])
	}
	batches := batchCOWRuns(runs, 3)
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 {
		t.Fatalf("batches=%v", batches)
	}
	if batchCOWRuns(nil, 3) != nil {
		t.Fatal("empty runs produced a batch")
	}
}

func TestCOWShareSkipsFilesBelowMinimum(t *testing.T) {
	t.Parallel()
	a, b := cowRoots(t)
	small := strings.Repeat("s", cowMinShareSize-1)
	large := strings.Repeat("l", cowMinShareSize)
	for _, root := range []*os.Root{a, b} {
		cowWrite(t, root, "small", small)
		cowWrite(t, root, "large", large)
	}
	before, _ := b.Stat("small")
	stats := &cowStats{}
	sharer := &cowSharer{source: a, destination: b, proof: func() error { return nil }, minSize: cowMinShareSize, stats: stats}
	err := sharer.shareRun(context.Background(), newCOWScratch(), ".", []string{"small", "large"})
	if !cowAvailable() {
		// clone できない platform では下限を超えた large だけが失敗まで進む。
		if err == nil {
			t.Fatal("clone unexpectedly succeeded")
		}
	} else if err != nil {
		t.Fatal(err)
	}
	after, _ := b.Stat("small")
	if !os.SameFile(before, after) {
		t.Fatal("a file below the minimum was replaced")
	}
	if stats.skippedSize.Load() != 1 {
		t.Fatalf("skipped=%d", stats.skippedSize.Load())
	}
	if cowAvailable() && stats.shared.Load() != 1 {
		t.Fatalf("shared=%d", stats.shared.Load())
	}
}

// storage.cow_min_size_kib を0にすると下限がなくなり、数バイトのファイルまで共有対象になる。
func TestCOWShareWithoutMinimumSharesEverySize(t *testing.T) {
	t.Parallel()
	a, b := cowRoots(t)
	for _, root := range []*os.Root{a, b} {
		cowWrite(t, root, "tiny", "t")
	}
	before, _ := b.Stat("tiny")
	stats := &cowStats{}
	sharer := &cowSharer{source: a, destination: b, proof: func() error { return nil }, minSize: 0, stats: stats}
	err := sharer.shareRun(context.Background(), newCOWScratch(), ".", []string{"tiny"})
	if !cowAvailable() {
		// clone できない platform では下限を外した結果、tiny が失敗まで進むことだけを確かめる。
		if err == nil {
			t.Fatal("clone unexpectedly succeeded")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	after, _ := b.Stat("tiny")
	if os.SameFile(before, after) {
		t.Fatal("a tiny file was left unshared without a minimum")
	}
	if stats.skippedSize.Load() != 0 || stats.shared.Load() != 1 {
		t.Fatalf("skipped=%d shared=%d", stats.skippedSize.Load(), stats.shared.Load())
	}
}

// main の tree 形状違いは共有対象外というだけなので、run 全体を skip して準備は続ける。
func TestCOWShareSkipsSourceShapeMismatch(t *testing.T) {
	t.Parallel()
	a, b := cowRoots(t)
	cowWrite(t, a, "dir", "a file where the destination has a directory")
	if err := b.Mkdir("dir", 0o700); err != nil {
		t.Fatal(err)
	}
	cowWrite(t, b, "dir/file", "same")
	before, _ := b.Stat("dir/file")
	sharer := &cowSharer{source: a, destination: b, proof: func() error { return nil }, stats: &cowStats{}}
	if err := sharer.shareRun(context.Background(), newCOWScratch(), "dir", []string{"file"}); err != nil {
		t.Fatalf("source shape mismatch aborted preparation: %v", err)
	}
	after, _ := b.Stat("dir/file")
	if !os.SameFile(before, after) {
		t.Fatal("destination changed")
	}
	// 宛先に無い directory は、宛先ファイル欠落と同じく skip する。
	if err := sharer.shareRun(context.Background(), newCOWScratch(), "missing", []string{"file"}); err != nil {
		t.Fatalf("missing destination directory aborted preparation: %v", err)
	}
}

func TestCOWErrorsPreferOwnershipOverBenignFailures(t *testing.T) {
	t.Parallel()
	benign := errors.New("clone failed")
	errs := &cowErrors{}
	errs.add(nil)
	errs.add(benign)
	if got := errs.result(context.Background()); !errors.Is(got, benign) {
		t.Fatalf("benign result=%v", got)
	}
	errs.add(state.ErrOwnership)
	errs.add(errors.New("later"))
	if got := errs.result(context.Background()); !errors.Is(got, state.ErrOwnership) {
		t.Fatalf("ownership result=%v", got)
	}
	// ctx は良性の失敗より優先し、所有権失敗より後ろに置く。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := (&cowErrors{}).result(ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("cancelled result=%v", got)
	}
	if got := errs.result(ctx); !errors.Is(got, state.ErrOwnership) {
		t.Fatalf("cancelled ownership result=%v", got)
	}
	benignOnly := &cowErrors{}
	benignOnly.add(benign)
	if got := benignOnly.result(ctx); !errors.Is(got, context.Canceled) {
		t.Fatalf("cancelled benign result=%v", got)
	}
}

func TestCOWBatchesStopSubmissionAfterFailure(t *testing.T) {
	t.Parallel()
	batches := [][]cowRun{{{directory: "0"}}, {{directory: "1"}}, {{directory: "2"}}, {{directory: "3"}}}
	failure := errors.New("batch failed")
	var started atomic.Int64
	// worker 1 で順序を固定し、失敗後に新規 batch を投入しないことを見る。
	err := runCOWBatches(context.Background(), 1, batches, func(_ context.Context, batch []cowRun) error {
		started.Add(1)
		if batch[0].directory == "0" {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) || started.Load() != 1 {
		t.Fatalf("err=%v started=%d", err, started.Load())
	}
	started.Store(0)
	if err := runCOWBatches(context.Background(), 0, batches, func(context.Context, []cowRun) error {
		started.Add(1)
		return nil
	}); err != nil || started.Load() != int64(len(batches)) {
		t.Fatalf("err=%v started=%d", err, started.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runCOWBatches(ctx, 2, batches, func(context.Context, []cowRun) error {
		t.Error("a batch ran after the context was cancelled")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err=%v", err)
	}
}

func TestCOWWorkerCountStaysWithinBounds(t *testing.T) {
	t.Parallel()
	p := &Preparer{}
	workers := p.cowWorkers()
	if workers < 1 || workers > cowMaxWorkers {
		t.Fatalf("workers=%d", workers)
	}
	p.cowWorkerCount = 1
	if got := p.cowWorkers(); got != 1 {
		t.Fatalf("pinned workers=%d", got)
	}
}

// 証明は batch の前後で呼ばれるため、clone できない platform では直接検査する。
// 失敗した証明も隔離判断の入力になるので、成否に関わらず結果を書き換えず計測へ残すことを確かめる。
func TestCOWProofIsPassedThroughAndMeasured(t *testing.T) {
	t.Parallel()
	failure := errors.New("proof failed")
	stats := &cowStats{}
	sharer := &cowSharer{proof: func() error { return failure }, stats: stats}
	if err := sharer.verifyProof(); !errors.Is(err, failure) {
		t.Fatalf("failed proof err=%v", err)
	}
	sharer.proof = func() error { return nil }
	if err := sharer.verifyProof(); err != nil {
		t.Fatalf("successful proof err=%v", err)
	}
	if got := stats.proof.count.Load(); got != 2 {
		t.Fatalf("proof count=%d", got)
	}
}

func TestCOWStatsAreLoggedWhenALoggerExists(t *testing.T) {
	t.Parallel()
	stats := &cowStats{}
	stats.entries.Store(7)
	stats.shared.Add(1)
	stats.clone.observe(time.Now())
	var logged bytes.Buffer
	p := &Preparer{Log: slog.New(slog.NewTextHandler(&logged, nil))}
	p.logCOWStats("target", stats)
	for _, want := range []string{"entries=7", "shared=1", "clone_count=1", "unlink_ms=0", "proof_count=0"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatalf("log %q does not contain %q", logged.String(), want)
		}
	}
	// 計測は準備結果を変えないため、logger を持たない経路でも呼べる。
	(&Preparer{}).logCOWStats("target", stats)
}

// 並列と batch 分割を通した共有を、実際の準備結果に対して確かめる。
// testlint:allow-serial -- プロセス全体の環境（HOME）を変更するため
func TestCOWSharesFilesAcrossParallelBatches(t *testing.T) {
	if !cowAvailable() {
		t.Skip("CoW platform required")
	}
	p, repo, _, target := cowFixture(t)
	source := string(repo.MainPath)
	var names []string
	for _, directory := range []string{"a", "b", "c"} {
		if err := os.MkdirAll(filepath.Join(source, directory), 0o700); err != nil {
			t.Fatal(err)
		}
		for index := range 100 {
			name := filepath.Join(directory, fmt.Sprintf("f%03d", index))
			if err := os.WriteFile(filepath.Join(source, name), []byte(cowBody), 0o644); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
	}
	cowGit(t, source, "add", ".")
	cowGit(t, source, "commit", "-m", "many")
	oid := cowGit(t, source, "rev-parse", "HEAD")
	p.Config.Storage.CopyMode = config.CopyModeCopy
	if err := p.Prepare(context.Background(), repo, target, oid, testSlotID); err != nil {
		t.Fatal(err)
	}
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	before := make([]uint64, len(names))
	for index, name := range names {
		var info unix.Stat_t
		if err := unix.Stat(filepath.Join(target, name), &info); err != nil {
			t.Fatal(err)
		}
		before[index] = info.Ino
	}
	if err := p.compactOwnedWorktree(context.Background(), repo, target, oid, testSlotID, preparePhaseCreate, identity, nil); err != nil {
		t.Fatal(err)
	}
	for index, name := range names {
		var info unix.Stat_t
		if err := unix.Stat(filepath.Join(target, name), &info); err != nil {
			t.Fatal(err)
		}
		if info.Ino == before[index] {
			t.Fatalf("%s was not shared", name)
		}
		if data, err := os.ReadFile(filepath.Join(target, name)); err != nil || string(data) != cowBody {
			t.Fatalf("%s changed: %d bytes %v", name, len(data), err)
		}
	}
	if got := cowGit(t, target, "status", "--porcelain", "--untracked-files=no"); got != "" {
		t.Fatalf("dirty checkout: %s", got)
	}
}
