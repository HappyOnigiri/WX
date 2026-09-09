package workspace

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

const (
	// cowMinShareSize は共有対象の下限である。APFS のブロック共有は数KBのファイルでほぼ容量を節約せず、
	// clone・比較・metadata 検査・swap・unlink の定数費用だけが残るため、下限未満は通常 checkout のまま残す。
	cowMinShareSize = 16 << 10
	// cowBatchSize は1つの worker が受け持つ entry のおおよその件数である。
	// run 単位で並列にすると短い run では同期費用が勝つため、数百件へまとめてから配る。
	cowBatchSize = 192
	// cowMaxWorkers は並列度の上限である。CoW は利用者の対話操作と同じマシンで走るので全 CPU は使わない。
	cowMaxWorkers = 4
)

// cowStage は1種類の操作の呼び出し回数と所要時間を集計する。
type cowStage struct {
	count atomic.Int64
	nanos atomic.Int64
}

func (s *cowStage) observe(start time.Time) {
	s.count.Add(1)
	s.nanos.Add(int64(time.Since(start)))
}

func (s *cowStage) logArgs(name string) []any {
	return []any{name + "_count", s.count.Load(), name + "_ms", s.nanos.Load() / int64(time.Millisecond)}
}

// cowStats は共有処理の内訳を集計する。準備結果は変えず、遅い区間の特定にだけ使う。
type cowStats struct {
	entries     atomic.Int64
	candidates  atomic.Int64
	shared      atomic.Int64
	skippedSize atomic.Int64
	open        cowStage
	compare     cowStage
	clone       cowStage
	metadata    cowStage
	swap        cowStage
	verify      cowStage
	unlink      cowStage
	proof       cowStage
}

func (s *cowStats) logArgs() []any {
	args := []any{
		"entries", s.entries.Load(), "candidates", s.candidates.Load(),
		"shared", s.shared.Load(), "skipped_size", s.skippedSize.Load(),
	}
	for _, stage := range []struct {
		name  string
		stage *cowStage
	}{
		{"open", &s.open},
		{"compare", &s.compare},
		{"clone", &s.clone},
		{"metadata", &s.metadata},
		{"swap", &s.swap},
		{"verify", &s.verify},
		{"unlink", &s.unlink},
		{"proof", &s.proof},
	} {
		args = append(args, stage.stage.logArgs(stage.name)...)
	}
	return args
}

// cowSharer は1回の共有処理で変わらない対象と、per-file の所有権証明・計測をまとめる。
// proof は clone に成功した対象だけに対し、swap 前と cleanup 前の最大2回呼ばれる。
type cowSharer struct {
	source      *os.Root
	destination *os.Root
	proof       func() error
	minSize     int64
	stats       *cowStats
}

// verifyProof は所有権証明を1回行い、その所要時間を計測する。
func (s *cowSharer) verifyProof() error {
	start := time.Now()
	defer func() { s.stats.proof.observe(start) }()
	return s.proof()
}

// cowRun は同一 directory に属する連続した entry である。`ls-files` の出力は path 順なので連続で現れる。
type cowRun struct {
	directory string
	leaves    []string
}

// splitCOWRuns は entry を directory ごとの run へまとめる。
// run 単位で directory descriptor を1度だけ開くことで、成分 lstat を entry 数から directory 数へ落とす。
func splitCOWRuns(entries []cowIndexEntry) []cowRun {
	var runs []cowRun
	for _, entry := range entries {
		directory := filepath.Dir(entry.name)
		if len(runs) == 0 || runs[len(runs)-1].directory != directory {
			runs = append(runs, cowRun{directory: directory})
		}
		last := &runs[len(runs)-1]
		last.leaves = append(last.leaves, filepath.Base(entry.name))
	}
	return runs
}

// batchCOWRuns は run を entry 数が size 程度になるまでまとめ、worker へ配る単位を作る。
func batchCOWRuns(runs []cowRun, size int) [][]cowRun {
	var batches [][]cowRun
	var current []cowRun
	count := 0
	for _, run := range runs {
		current = append(current, run)
		count += len(run.leaves)
		if count >= size {
			batches = append(batches, current)
			current, count = nil, 0
		}
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

// shareRun は run を、その directory の descriptor 相対で処理する。
// 成分の symlink・非 directory の検査は run 先頭の OpenRootAt が担い、leaf 側は1成分だけの検査になる。
func (s *cowSharer) shareRun(ctx context.Context, directory string, leaves []string) error {
	sourceDirectory, err := domain.OpenRootAt(s.source, directory)
	if cowSourceIneligible(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = sourceDirectory.Close() }()
	destinationDirectory, err := domain.OpenRootAt(s.destination, directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: open CoW destination directory: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destinationDirectory.Close() }()
	// clone・swap・unlink 用の parent は開いた Root 自身から取り、path 名を歩き直さない。
	parent, err := destinationDirectory.Open(".")
	if err != nil {
		return fmt.Errorf("%w: open CoW parent: %w", state.ErrOwnership, err)
	}
	defer func() { _ = parent.Close() }()
	for _, leaf := range leaves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.shareFile(ctx, sourceDirectory, destinationDirectory, parent, leaf); err != nil {
			return err
		}
	}
	return nil
}

func (s *cowSharer) shareFile(ctx context.Context, source, destination *os.Root, parent *os.File, leaf string) error {
	start := time.Now()
	in, sourceInfo, err := cowOpenFile(source, leaf)
	if cowSourceIneligible(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if in == nil {
		return nil
	}
	defer func() { _ = in.Close() }()
	original, info, err := cowOpenFile(destination, leaf)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: inspect CoW destination: %w", state.ErrOwnership, err)
	}
	if original == nil {
		return nil
	}
	defer func() { _ = original.Close() }()
	s.stats.open.observe(start)
	if info.Size() == 0 || sourceInfo.Size() != info.Size() || os.SameFile(sourceInfo, info) {
		return nil
	}
	if info.Size() < s.minSize {
		s.stats.skippedSize.Add(1)
		return nil
	}
	var before unix.Stat_t
	if err := unix.Fstat(int(original.Fd()), &before); err != nil {
		return err
	}
	if before.Nlink != 1 {
		return nil
	}
	return s.replaceWithClone(ctx, in, original, parent, leaf, before)
}

func (s *cowSharer) replaceWithClone(ctx context.Context, in, original, parent *os.File, leaf string, before unix.Stat_t) (result error) {
	temporary := cowTemporaryPrefix + rand.Text()
	start := time.Now()
	if err := cloneCOW(in, parent, temporary); err != nil {
		return err
	}
	s.stats.clone.observe(start)
	candidate, err := openCOWLeaf(parent, temporary)
	if err != nil {
		return fmt.Errorf("%w: open CoW clone: %w", state.ErrOwnership, err)
	}
	defer func() { _ = candidate.Close() }()
	candidateInfo, err := candidate.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat CoW clone: %w", state.ErrOwnership, err)
	}
	cleanupInfo := candidateInfo
	defer func() {
		// swap 後は元ファイルが temporary にある。証明できない物は消さず隔離へ渡す。
		if errors.Is(result, state.ErrOwnership) {
			return
		}
		if err := s.verifyProof(); err != nil {
			result = fmt.Errorf("%w: CoW cleanup ownership: %w", state.ErrOwnership, err)
			return
		}
		verifyStart := time.Now()
		if err := verifyCOWLeaf(parent, temporary, cleanupInfo); err != nil {
			result = err
			return
		}
		s.stats.verify.observe(verifyStart)
		unlinkStart := time.Now()
		if err := unix.Unlinkat(int(parent.Fd()), temporary, 0); err != nil {
			result = fmt.Errorf("%w: remove CoW temporary: %w", state.ErrOwnership, err)
			return
		}
		s.stats.unlink.observe(unlinkStart)
	}()
	start = time.Now()
	equal, err := sameCOWBytes(ctx, original, candidate)
	s.stats.compare.observe(start)
	if err != nil || !equal {
		return err
	}
	start = time.Now()
	compatible, err := cowMetadata(original, candidate, before)
	s.stats.metadata.observe(start)
	if err != nil || !compatible {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.verifyProof(); err != nil {
		return fmt.Errorf("%w: CoW replacement ownership: %w", state.ErrOwnership, err)
	}
	originalInfo, err := original.Stat()
	if err != nil {
		return err
	}
	start = time.Now()
	if err := swapCOW(parent, temporary, leaf); err != nil {
		return err
	}
	s.stats.swap.observe(start)
	// swap は atomic なので入れ替わりは確認し直さない。cleanup が消す inode の同一性だけ後で検査する。
	cleanupInfo = originalInfo
	s.stats.shared.Add(1)
	return nil
}

// cowErrors は worker の失敗を、隔離すべき所有権失敗を落とさない優先度で1つに畳む。
type cowErrors struct {
	mu        sync.Mutex
	ownership error
	other     error
}

func (e *cowErrors) add(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if errors.Is(err, state.ErrOwnership) {
		if e.ownership == nil {
			e.ownership = err
		}
		return
	}
	if e.other == nil {
		e.other = err
	}
}

// result は所有権失敗・ctx・その他の順で返す。
// 「最初に着いた失敗」を返すと、良性の失敗が先に着いた回だけ隔離されず貸し出される欠落になる。
func (e *cowErrors) result(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ownership != nil {
		return e.ownership
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.other
}

// runCOWBatches は着手済みの batch を完走させ、失敗後は新規 batch の投入だけを止める。
// derived context の cancel は使わない。内部 cancel が集約結果に混ざると auto でも hard fail するためである。
func runCOWBatches(ctx context.Context, workers int, batches [][]cowRun, share func(context.Context, []cowRun) error) error {
	if workers < 1 {
		workers = 1
	}
	errs := &cowErrors{}
	var stop atomic.Bool
	var wait sync.WaitGroup
	gate := make(chan struct{}, workers)
	for _, batch := range batches {
		// 実行枠を取ってから判定する。先に判定すると、枠待ちの間に起きた失敗を見落として次の batch を投入する。
		gate <- struct{}{}
		if stop.Load() || ctx.Err() != nil {
			<-gate
			break
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-gate }()
			if err := share(ctx, batch); err != nil {
				errs.add(err)
				stop.Store(true)
			}
		}()
	}
	wait.Wait()
	return errs.result(ctx)
}

// cowWorkers は共有処理の並列度を返す。
// cowWorkerCount はテストが共有順序を決定的にするための内部フックで、production では 0 のままにする。
func (p *Preparer) cowWorkers() int {
	if p.cowWorkerCount > 0 {
		return p.cowWorkerCount
	}
	return min(runtime.NumCPU(), cowMaxWorkers)
}

// logCOWStats は共有処理の内訳を1行で残す。Log が nil でも準備結果は変わらず、記録だけが落ちる。
func (p *Preparer) logCOWStats(target string, stats *cowStats) {
	if p.Log == nil {
		return
	}
	p.Log.Info("worktree CoW compaction", append([]any{"target", target}, stats.logArgs()...)...)
}
