package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// cowDirectoryStack は path 順の走査で directory descriptor を共通接頭辞ごと持ち越す。
// entry ごとに root から全成分をたどると、深さに比例した openat が directory 数だけ繰り返される。
// 成分は必ず1つずつ O_NOFOLLOW で開くので、走査中に symlink を差し込まれても pin した外へは出ない。
type cowDirectoryStack struct {
	create  bool
	opened  []*os.File
	current []string
}

func newCOWDirectoryStack(root *os.Root, create bool) (*cowDirectoryStack, error) {
	base, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	return &cowDirectoryStack{create: create, opened: []*os.File{base}}, nil
}

func (s *cowDirectoryStack) close() {
	for _, file := range s.opened {
		_ = file.Close()
	}
	s.opened, s.current = nil, nil
}

func (s *cowDirectoryStack) pop(depth int) {
	for len(s.current) > depth {
		_ = s.opened[len(s.opened)-1].Close()
		s.opened = s.opened[:len(s.opened)-1]
		s.current = s.current[:len(s.current)-1]
	}
}

// at は directory の descriptor を返す。返した descriptor は次の at 呼び出しまで有効である。
// create が false のとき、途中に存在しない成分があれば os.ErrNotExist を返す。
func (s *cowDirectoryStack) at(directory string) (*os.File, error) {
	if directory == "." {
		s.pop(0)
		return s.opened[0], nil
	}
	parts := strings.Split(directory, string(os.PathSeparator))
	shared := 0
	for shared < len(s.current) && shared < len(parts) && s.current[shared] == parts[shared] {
		shared++
	}
	s.pop(shared)
	for _, part := range parts[shared:] {
		parent := int(s.opened[len(s.opened)-1].Fd())
		if s.create {
			if err := unix.Mkdirat(parent, part, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
				return nil, err
			}
		}
		fd, err := unix.Openat(parent, part, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, err
		}
		s.opened = append(s.opened, os.NewFile(uintptr(fd), part))
		s.current = append(s.current, part)
	}
	return s.opened[len(s.opened)-1], nil
}

// cowPlacer は clone 一巡で変わらない対象と、置けた path の集計をまとめる。
type cowPlacer struct {
	source      *os.Root
	destination *os.Root
	proof       func() error
	minSize     int64
	stats       *cowStats
	mu          sync.Mutex
	placed      map[string]bool
}

func (c *cowPlacer) verifyProof() error {
	start := time.Now()
	defer func() { c.stats.proof.observe(start) }()
	return c.proof()
}

func (c *cowPlacer) record(names []string) {
	if len(names) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, name := range names {
		c.placed[name] = true
	}
}

// placeChunk は path 順に連続した run の塊を1つの descriptor スタックで処理する。
// gate は所有権証明で、chunk の前後と一定件数ごとに呼ぶ。
func (c *cowPlacer) placeChunk(ctx context.Context, chunk []cowRun, gate func() error) error {
	if err := gate(); err != nil {
		return err
	}
	sourceStack, err := newCOWDirectoryStack(c.source, false)
	if err != nil {
		return err
	}
	defer sourceStack.close()
	destinationStack, err := newCOWDirectoryStack(c.destination, true)
	if err != nil {
		return err
	}
	defer destinationStack.close()
	placed := make([]string, 0, cowBatchSize)
	since := 0
	defer func() { c.record(placed) }()
	for _, run := range chunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		if since >= cowBatchSize {
			if err := gate(); err != nil {
				return err
			}
			since = 0
		}
		since += len(run.leaves)
		start := time.Now()
		sourceDirectory, err := sourceStack.at(run.directory)
		if err != nil {
			// donor 側の形状違いは共有できないというだけなので、run ごと skip して準備は続ける。
			if cowSourceIneligible(err) || errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		c.stats.directory.observe(start)
		// 下限を超える leaf が1つも無い directory では宛先を作らない。
		// 宛先の作成と open は共有の有無に関わらず directory 数だけ積み上がり、後段の checkout がどのみち作る。
		shareable := c.shareableLeaves(sourceDirectory, run.leaves)
		if len(shareable) == 0 {
			continue
		}
		start = time.Now()
		destinationDirectory, err := destinationStack.at(run.directory)
		if err != nil {
			return fmt.Errorf("%w: open CoW destination directory: %w", state.ErrOwnership, err)
		}
		c.stats.directory.observe(start)
		for _, leaf := range shareable {
			done, err := c.placeFile(sourceDirectory, destinationDirectory, run.directory, leaf)
			if err != nil {
				return err
			}
			if done {
				placed = append(placed, joinCOWPath(run.directory, leaf))
			}
		}
	}
	return gate()
}

// shareableLeaves は donor 側の fstatat だけで、下限を超える通常ファイルの leaf を選ぶ。
// 候補の過半は下限未満で落ちるため、先に宛先を用意すると使わない mkdir と open がその分だけ積み上がる。
func (c *cowPlacer) shareableLeaves(source *os.File, leaves []string) []string {
	start := time.Now()
	defer func() { c.stats.stat.observe(start) }()
	shareable := leaves[:0:0]
	for _, leaf := range leaves {
		var info unix.Stat_t
		if err := unix.Fstatat(int(source.Fd()), leaf, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		if info.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		if info.Size < c.minSize {
			c.stats.skippedSize.Add(1)
			continue
		}
		shareable = append(shareable, leaf)
	}
	return shareable
}

// placeFile は1件を clone し、置けたかを返す。
// 置けなかった leaf は通常 checkout に回るだけなので、共有できない理由では準備を止めない。
func (c *cowPlacer) placeFile(source, destination *os.File, directory, leaf string) (bool, error) {
	start := time.Now()
	fd, err := unix.Openat(int(source.Fd()), leaf, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		// main 側の実体が走査中に消えた・symlink へ変わった回は共有対象外にするだけでよい。
		return false, nil
	}
	in := os.NewFile(uintptr(fd), leaf)
	defer func() { _ = in.Close() }()
	c.stats.open.observe(start)
	start = time.Now()
	cloneErr := cloneCOW(in, destination, leaf)
	c.stats.clone.observe(start)
	if cloneErr != nil {
		// 宛先に既に実体がある回は、通常 checkout の結果を CoW で上書きしないために諦める。
		if errors.Is(cloneErr, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("clone %s: %w", joinCOWPath(directory, leaf), cloneErr)
	}
	c.stats.shared.Add(1)
	return true, nil
}

func joinCOWPath(directory, leaf string) string {
	if directory == "." {
		return leaf
	}
	return directory + "/" + leaf
}

// chunkCOWRuns は run を path 順に連続したまま、entry 数がおおよそ size になる塊へ分ける。
// 塊の中では directory descriptor を共有接頭辞ごと持ち越せるので、塊を細かくしすぎると走査が重複する。
func chunkCOWRuns(runs []cowRun, workers int) [][]cowRun {
	entries := 0
	for _, run := range runs {
		entries += len(run.leaves)
	}
	size := max(entries/(workers*4), cowBatchSize)
	return batchCOWRuns(runs, size)
}

// planCOWPlacement は、要求 OID と main 側 index の blob OID が一致する tracked path を選ぶ。
// OID の一致は clone してよい根拠ではなく、内容まで一致し得る path に絞る事前 skip でしかない。
// 実際に一致したかは配置後の tracked 検査が Git に判定させ、違えばその path だけ通常 checkout でやり直す。
func planCOWPlacement(plan *earlyPlan, sourceOIDs map[string]string) []cowIndexEntry {
	if sourceOIDs == nil {
		return nil
	}
	candidates := make([]cowIndexEntry, 0, len(plan.oids))
	for _, path := range plan.tracked {
		oid, shareable := plan.oids[path]
		if !shareable || plan.early[path] || sourceOIDs[path] != oid {
			continue
		}
		candidates = append(candidates, cowIndexEntry{name: path, oid: oid})
	}
	return candidates
}

// placeSharedFiles は残りの checkout より先に、main と同内容になり得る tracked file を clone で配置する。
// checkout してから同内容へ差し替えるのに比べ、同じ bytes の書き出しと読み比べが1往復ぶん要らなくなる。
// 戻り値の path は checkout の対象から外す。
func (p *Preparer) placeSharedFiles(ctx context.Context, repo discovery.Repository, item *stagedRepository, slotID string) (map[string]bool, error) {
	// clone できない platform と copy 指定では1件も置かず、方式の判断は従来どおり compactWorktree に委ねる。
	mode := p.Config.Storage.CopyMode
	if mode == config.CopyModeCopy || !cowAvailable() {
		return nil, nil
	}
	placed, err := p.placeOwnedSharedFiles(ctx, repo, item, slotID)
	return placed, p.cowFallback(ctx, mode, item.Target, err)
}

func (p *Preparer) placeOwnedSharedFiles(ctx context.Context, repo discovery.Repository, item *stagedRepository, slotID string) (map[string]bool, error) {
	owner, relative, _, err := p.openOwnedRoot(p.RootPath, item.Target)
	if err != nil {
		return nil, err
	}
	validate := func() error {
		return p.verifyPreparedTargetIdentity(owner, relative, item.locked.identity)
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return nil, fmt.Errorf("%w: open CoW target: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	candidates := planCOWPlacement(&item.plan, p.cowSourceIndexOIDs(ctx, source))
	stats := &cowStats{}
	stats.entries.Store(int64(len(item.plan.tracked)))
	stats.candidates.Store(int64(len(candidates)))
	placer := &cowPlacer{
		source:      source,
		destination: destination,
		proof:       validate,
		minSize:     cowMinShareSize,
		stats:       stats,
		placed:      make(map[string]bool, len(candidates)),
	}
	slotStates, repoStates := preparationOwnershipStates(preparePhaseCreate)
	gate := func(ctx context.Context) func() error {
		return func() error {
			// WithoutCancel は cancel 後も証明を成立させるための扱いで、落とすと中断時に隔離判断ができなくなる。
			if err := p.validateStateOwnership(context.WithoutCancel(ctx), repo, item.Target, slotID, slotStates, repoStates); err != nil {
				return err
			}
			if err := placer.verifyProof(); err != nil {
				return fmt.Errorf("%w: CoW placement ownership: %w", state.ErrOwnership, err)
			}
			return nil
		}
	}
	workers := p.cowWorkers()
	chunks := chunkCOWRuns(splitCOWRuns(candidates), workers)
	placeErr := runCOWBatches(ctx, workers, chunks, func(ctx context.Context, chunk []cowRun) error {
		return placer.placeChunk(ctx, chunk, gate(ctx))
	})
	p.logCOWStats(item.Target, stats)
	stats.recordCOWPhases(p.Phases, "cow-place")
	if placeErr != nil {
		return placer.placed, placeErr
	}
	return placer.placed, validate()
}

// settleCOWPlacement は clone した内容が要求 OID と一致することを Git に判定させ、
// 一致しない path だけを通常 checkout でやり直す。
// main が dirty だった path や、clone 後に main が変わった path はここで通常 checkout へ落ちる。
func (p *Preparer) settleCOWPlacement(ctx context.Context, item *stagedRepository, placed map[string]bool) error {
	if len(placed) == 0 {
		return nil
	}
	changed, err := p.trackedChangedPaths(ctx, item)
	if err != nil {
		return err
	}
	repair := make([]string, 0, len(changed))
	for _, path := range changed {
		if placed[path] {
			repair = append(repair, path)
		}
	}
	if len(repair) == 0 {
		return nil
	}
	p.logSkip("CoW placement did not match the requested OID", "repository", string(item.Repository.MainPath), "paths", len(repair))
	// --force は clone した実体を捨てて要求 OID の内容に戻すために要る。
	args := []string{"checkout-index", "--index", "--force", "-z", "--stdin"}
	if _, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, []byte(strings.Join(repair, "\x00")+"\x00"), args...); err != nil {
		return err
	}
	changed, err = p.trackedChangedPaths(ctx, item)
	if err != nil {
		return err
	}
	for _, path := range changed {
		if placed[path] {
			return fmt.Errorf("CoW placement remains different from the requested OID at %s", path)
		}
	}
	return nil
}

// trackedChangedPaths は tracked file のうち index と内容・mode が違う path を返す。
// clone した file は index に stat 情報を持たないため、この検査が内容を実際に読んで確かめる。
func (p *Preparer) trackedChangedPaths(ctx context.Context, item *stagedRepository) ([]string, error) {
	result, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if len(entry) < 4 {
			continue
		}
		paths = append(paths, entry[3:])
	}
	return paths, nil
}
