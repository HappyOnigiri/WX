package workspace

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// usageMaxWorkers は測定の並列度の上限である。
// 費用の大半は openat・fstatat・fcntl の待ちなので、CPU 数まで重ねると実時間が縮む。
const usageMaxWorkers = 10

// usageDirectory は測定中の directory 1 個の位置と、対応する共有元 directory である。
// dir と main は task が閉じる descriptor で、main が nil の subtree では共有判定を行わない。
// repo は repository の内側かを表し、共有元を開けなかった subtree でも Compared の対象は変えない。
type usageDirectory struct {
	name   string
	slotID string
	repo   bool
	dir    *os.File
	main   *os.File
}

func (d usageDirectory) close() {
	_ = d.dir.Close()
	if d.main != nil {
		_ = d.main.Close()
	}
}

// usageCacheEntry は共有判定を root 相対 path 付きで持ち運ぶ。
type usageCacheEntry struct {
	name  string
	state SharedFileState
}

// usageDirectoryTotals は directory 1 個分の集計である。
// directory 内の entry は必ず同じ slot に属するため、slot 別に分けずに 1 つの sample へ足す。
type usageDirectoryTotals struct {
	unmanaged int64
	logical   int64
	allocated int64
	shared    int64
	sample    SlotUsage
	cache     []usageCacheEntry
}

// usageScan は 1 回の測定で共有する対象・集計先・cache である。
// 走査は directory 単位で並列に走るので、集計と cache は mutex の下でだけ触る。
type usageScan struct {
	ctx      context.Context
	previous SharedFileCache
	slots    map[string]string
	repos    map[string]usageRepository
	gate     chan struct{}
	wait     sync.WaitGroup

	mu      sync.Mutex
	usage   RootUsage
	cache   SharedFileCache
	failure error
}

func newUsageScan(ctx context.Context, targets []SlotUsageTarget, previous SharedFileCache) *usageScan {
	usage := RootUsage{Slots: map[string]SlotUsage{}}
	slots, repos := usagePrefixes(targets, usage.Slots)
	return &usageScan{
		ctx: ctx, previous: previous, slots: slots, repos: repos,
		gate:  make(chan struct{}, min(runtime.NumCPU(), usageMaxWorkers)),
		usage: usage, cache: SharedFileCache{},
	}
}

// finish は spawn した worker を待ち合わせ、集計と cache を返す。
func (s *usageScan) finish() (RootUsage, SharedFileCache, error) {
	s.wait.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usage, s.cache, s.failure
}

// measure は directory 1 個の entry を数え、子 directory へ降りる。
// entry は descriptor 相対の 1 成分だけで引くので、path 名を root から辿り直す費用が深さに比例して積み上がらない。
func (s *usageScan) measure(task usageDirectory) {
	defer task.close()
	if err := s.ctx.Err(); err != nil {
		s.fail(err)
		return
	}
	if s.stopped() {
		return
	}
	leaves, err := task.dir.Readdirnames(-1)
	if err != nil {
		s.fail(err)
		return
	}
	totals := usageDirectoryTotals{}
	for _, leaf := range leaves {
		if err := s.ctx.Err(); err != nil {
			s.fail(err)
			break
		}
		if !s.visit(task, &totals, leaf) {
			break
		}
	}
	s.record(task.slotID, &totals)
}

// visit は entry 1 件を集計へ足すか子 directory へ降り、走査を続けてよいかを返す。
func (s *usageScan) visit(task usageDirectory, totals *usageDirectoryTotals, leaf string) bool {
	var stat unix.Stat_t
	if err := unix.Fstatat(int(task.dir.Fd()), leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return s.tolerate(err)
	}
	name := usageJoin(task.name, leaf)
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		s.count(task, totals, name, leaf, &stat)
		return true
	}
	child, err := s.childOf(task, name, leaf)
	if err != nil {
		return s.tolerate(err)
	}
	s.spawn(child)
	return true
}

// tolerate は走査中に消えた entry を飛ばし、それ以外の失敗では測定を打ち切る。
// 消えた 1 件で root 全体の集計を捨てると、GC や clear と並走した回の Disk が測れないままになる。
func (s *usageScan) tolerate(err error) bool {
	if usageEntryVanished(err) {
		return true
	}
	s.fail(err)
	return false
}

// usageEntryVanished は走査中に entry が消えた・置き換わったことしか意味しない失敗を判定する。
func usageEntryVanished(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR)
}

// count は entry 1 件を集計へ足し、repository の内側にある通常ファイルだけ共有判定へ回す。
func (s *usageScan) count(task usageDirectory, totals *usageDirectoryTotals, name, leaf string, stat *unix.Stat_t) {
	allocated := stat.Blocks * 512
	if task.slotID == "" {
		totals.unmanaged += allocated
		return
	}
	regular := stat.Mode&unix.S_IFMT == unix.S_IFREG
	totals.allocated += allocated
	totals.sample.Files++
	totals.sample.AllocatedBytes += allocated
	if regular {
		totals.logical += stat.Size
		totals.sample.LogicalBytes += stat.Size
	}
	if !task.repo || !regular || stat.Size == 0 || !cowAvailable() {
		return
	}
	totals.sample.Compared++
	state, decided := s.sharedLeaf(task, leaf, name, stat)
	if decided {
		totals.cache = append(totals.cache, usageCacheEntry{name: name, state: state})
	}
	if state.Shared {
		totals.sample.SharedFiles++
		totals.sample.SharedBytes += allocated
		totals.shared += allocated
	}
}

// childOf は子 directory の task を組む。slot と repository の境界はここで切り替え、
// repository の内側では共有元も同じ 1 成分だけ降りる。共有元を開けない subtree は共有なしとして数える。
func (s *usageScan) childOf(task usageDirectory, name, leaf string) (usageDirectory, error) {
	child := usageDirectory{name: name, slotID: task.slotID, repo: task.repo}
	if slotID, boundary := s.slots[name]; boundary {
		child.slotID, child.repo = slotID, false
	}
	dir, err := openUsageChild(task.dir, leaf)
	if err != nil {
		return usageDirectory{}, err
	}
	child.dir = dir
	switch repository, boundary := s.repos[name]; {
	case boundary:
		// 登録と違う slot の下に現れた repository path は、共有元を持たない普通の directory として数える。
		child.repo = repository.slotID == child.slotID
		if child.repo {
			child.main = openUsageRepository(repository.mainPath)
		}
	case child.repo && task.main != nil:
		child.main = openUsageChildOrNil(task.main, leaf)
	}
	return child, nil
}

// spawn は子 directory を worker へ渡す。枠が空いていなければ呼び出し元の goroutine で降り、待ち合わせで詰まらせない。
func (s *usageScan) spawn(child usageDirectory) {
	select {
	case s.gate <- struct{}{}:
		s.wait.Add(1)
		go func() {
			defer s.wait.Done()
			defer func() { <-s.gate }()
			s.measure(child)
		}()
	default:
		s.measure(child)
	}
}

// record は directory 1 個分の集計を共有の合計へ移す。
func (s *usageScan) record(slotID string, totals *usageDirectoryTotals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.UnmanagedBytes += totals.unmanaged
	s.usage.LogicalBytes += totals.logical
	s.usage.AllocatedBytes += totals.allocated
	s.usage.SharedBytes += totals.shared
	if slotID != "" {
		sample := s.usage.Slots[slotID]
		sample.Files += totals.sample.Files
		sample.LogicalBytes += totals.sample.LogicalBytes
		sample.AllocatedBytes += totals.sample.AllocatedBytes
		sample.Compared += totals.sample.Compared
		sample.SharedFiles += totals.sample.SharedFiles
		sample.SharedBytes += totals.sample.SharedBytes
		s.usage.Slots[slotID] = sample
	}
	for _, entry := range totals.cache {
		s.cache[entry.name] = entry.state
	}
}

func (s *usageScan) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == nil {
		s.failure = err
	}
}

// stopped は打ち切り済みかを返す。返す集計は捨てられるため、残りの subtree へは降りない。
func (s *usageScan) stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure != nil
}

func usageJoin(directory, leaf string) string {
	if directory == "." || directory == "" {
		return leaf
	}
	return directory + "/" + leaf
}

// openUsageDirectory は root 相対 path の directory を、成分の symlink を拒否して descriptor へ開く。
// 深さに比例した検査を要するのは slot ごとに 1 度だけで、その下は openUsageChild が 1 成分ずつ降りる。
func openUsageDirectory(root *os.Root, relative string) (*os.File, error) {
	if relative == "." {
		return root.Open(".")
	}
	child, err := domain.OpenRootAt(root, relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = child.Close() }()
	return child.Open(".")
}

// openUsageChild は directory の子を 1 成分だけ openat で開き、symlink を辿らない。
func openUsageChild(parent *os.File, leaf string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), leaf, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), leaf), nil
}

// openUsageChildOrNil は共有元側の子 directory を開く。開けない回は共有判定なしで測定を続ける。
func openUsageChildOrNil(parent *os.File, leaf string) *os.File {
	dir, err := openUsageChild(parent, leaf)
	if err != nil {
		return nil
	}
	return dir
}

// openUsageRepository は共有元 main worktree を pin して directory descriptor を返す。
// 開けない回は共有判定を諦めるだけで、測定の失敗にはしない。
func openUsageRepository(mainPath string) *os.File {
	root, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return nil
	}
	defer func() { _ = root.Close() }()
	dir, err := root.Open(".")
	if err != nil {
		return nil
	}
	return dir
}
