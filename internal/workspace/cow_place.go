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
		// clone は file flags も複製するため、uchg の付いた実体を置くと slot が書換えも削除もできなくなる。
		// 置換方式では checkout 済みの実体との flags 不一致で落ちる分を、配置方式では donor 側だけで落とす。
		if cowSourceFlags(&info) != 0 {
			c.stats.skippedFlags.Add(1)
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

// cowPlacement は先行配置の結果である。
// placed は clone で置けた path で、checkout の対象から外す。
type cowPlacement struct {
	placed map[string]bool
	// pending は配置方式では置けなかったが、置換方式ならまだ共有できる候補の件数である。
	pending int
}

// complete は配置方式だけで共有をやり切ったかを返す。
// false の回に貸出前の置換方式を省くと、配置から外れた候補がどの方式でも共有されないまま残る。
func (c cowPlacement) complete() bool { return len(c.placed) > 0 && c.pending == 0 }

// placeSharedFiles は残りの checkout より先に、main と同内容になり得る tracked file を clone で配置する。
// checkout してから同内容へ差し替えるのに比べ、同じ bytes の書き出しと読み比べが1往復ぶん要らなくなる。
func (p *Preparer) placeSharedFiles(ctx context.Context, repo discovery.Repository, item *stagedRepository, slotID string) (cowPlacement, error) {
	// clone できない platform と copy 指定では1件も置かず、方式の判断は従来どおり compactWorktree に委ねる。
	mode := p.Config.Storage.CopyMode
	if mode == config.CopyModeCopy || !cowAvailable() {
		return cowPlacement{}, nil
	}
	// 先行配置した未追跡の .gitattributes は、要求 OID から読ませた checkout の属性と、配置後の tracked 検査が使う属性を食い違わせる。
	// この回は1件も置かず、bytes の一致を自分で確かめる置換方式へ共有を任せる。
	if item.plan.earlyAttributes() {
		p.logSkip("CoW placement yields to the replacement method for an early untracked .gitattributes", "repository", string(repo.MainPath))
		return cowPlacement{}, nil
	}
	placement, err := p.placeOwnedSharedFiles(ctx, repo, item, slotID)
	return placement, p.cowFallback(ctx, mode, item.Target, err)
}

func (p *Preparer) placeOwnedSharedFiles(ctx context.Context, repo discovery.Repository, item *stagedRepository, slotID string) (cowPlacement, error) {
	owner, relative, _, err := p.openOwnedRoot(p.RootPath, item.Target)
	if err != nil {
		return cowPlacement{}, err
	}
	validate := func() error {
		return p.verifyPreparedTargetIdentity(owner, relative, item.locked.identity)
	}
	destination, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return cowPlacement{}, fmt.Errorf("%w: open CoW target: %w", state.ErrOwnership, err)
	}
	defer func() { _ = destination.Close() }()
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return cowPlacement{}, err
	}
	defer func() { _ = source.Close() }()
	candidates, excluded, err := p.shareableCOWPlacements(ctx, item, planCOWPlacement(&item.plan, p.cowSourceIndexOIDs(ctx, source)))
	if err != nil {
		return cowPlacement{}, err
	}
	stats := &cowStats{}
	stats.entries.Store(int64(len(item.plan.tracked)))
	stats.candidates.Store(int64(len(candidates)))
	stats.pending.Store(int64(excluded))
	placer := &cowPlacer{
		source:      source,
		destination: destination,
		proof:       validate,
		minSize:     p.Config.Storage.COWMinShareSize(),
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
	if placeErr != nil {
		// 途中で止めた回は着手していない候補が残るため、置換方式へ回す件数を候補の残りで数える。
		stats.pending.Store(int64(excluded + len(candidates) - len(placer.placed)))
	}
	p.logCOWStats(item.Target, stats)
	stats.recordCOWPhases(p.Phases, "cow-place")
	placement := cowPlacement{placed: placer.placed, pending: int(stats.pending.Load())}
	if placeErr != nil {
		return placement, placeErr
	}
	return placement, validate()
}

// shareableCOWPlacements は変換の入り得る候補を落とし、落とした件数を返す。
// 配置後の tracked 検査は clean filter 越しの一致しか見ないため、変換が入る path では
// main の未コミット内容が blob へ戻る限り検査を通り、通常 checkout と違う bytes が残る。
func (p *Preparer) shareableCOWPlacements(ctx context.Context, item *stagedRepository, candidates []cowIndexEntry) ([]cowIndexEntry, int, error) {
	if len(candidates) == 0 {
		return nil, 0, nil
	}
	// core.autocrlf は属性を持たない path にも効くため、有効な回は path 単位に選り分けず全件を置換方式へ回す。
	// core.eol と core.checkRoundtripEncoding は対応する属性が付いた path にしか効かないので、属性側の判定で足りる。
	result, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, nil, "config", "--default", "false", "--get", "core.autocrlf")
	if err != nil {
		return nil, 0, err
	}
	if value := strings.TrimSpace(result.Stdout); value != "false" {
		p.logSkip("CoW placement yields to the replacement method while core.autocrlf converts content", "repository", string(item.Repository.MainPath), "core.autocrlf", value)
		return nil, len(candidates), nil
	}
	convertible, err := p.convertibleCOWPaths(ctx, item, candidates)
	if err != nil {
		return nil, 0, err
	}
	if len(convertible) == 0 {
		return candidates, 0, nil
	}
	kept := make([]cowIndexEntry, 0, len(candidates))
	for _, entry := range candidates {
		if convertible[entry.name] {
			continue
		}
		kept = append(kept, entry)
	}
	return kept, len(candidates) - len(kept), nil
}

// convertibleCOWPaths は候補のうち、属性によって checkout の bytes が blob と変わり得る path を返す。
// --cached は index の .gitattributes を読む指定で、tracked file を未配置の worktree で checkout が参照する側と同じになる。
func (p *Preparer) convertibleCOWPaths(ctx context.Context, item *stagedRepository, candidates []cowIndexEntry) (map[string]bool, error) {
	var input strings.Builder
	for _, entry := range candidates {
		input.WriteString(entry.name)
		input.WriteByte(0)
	}
	result, err := p.RunGitInWorktree(ctx, item.Target, item.locked.identity, nil, []byte(input.String()), "check-attr", "--cached", "--all", "--stdin", "-z")
	if err != nil {
		return nil, err
	}
	return parseCOWConvertiblePaths(result.Stdout), nil
}

// cowConversionAttributes は、checkout が書く bytes を index の blob と変え得る属性である。
var cowConversionAttributes = map[string]bool{
	"text": true, "eol": true, "crlf": true, "ident": true, "filter": true, "working-tree-encoding": true,
}

// parseCOWConvertiblePaths は `check-attr --all -z` の path・属性・値の3つ組から、変換の入り得る path を集める。
// --all は設定のある属性だけを出すので、変換に関わらない属性と、変換を外す unset は読み飛ばす。
func parseCOWConvertiblePaths(stdout string) map[string]bool {
	fields := strings.Split(stdout, "\x00")
	convertible := map[string]bool{}
	for index := 0; index+2 < len(fields); index += 3 {
		if !cowConversionAttributes[fields[index+1]] || fields[index+2] == "unset" {
			continue
		}
		convertible[fields[index]] = true
	}
	return convertible
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
