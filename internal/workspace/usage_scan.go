package workspace

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

// usageMaxWorkers は測定の並列度の上限である。
// 費用の大半は openat・fstatat・fcntl の待ちなので、CPU 数まで重ねると実時間が縮む。
const usageMaxWorkers = 10

// usageScope は directory 1 個が root 配下のどの層にあるかを表し、降下先と集計対象の entry を決める。
// wx の予約 namespace の外へ降りないことと、列挙（`wx clear --unmanaged`）の対象と同じ実体だけを数えることをこの区別で担保する。
type usageScope uint8

const (
	// usageScopeTree は slot の内側で、すべての entry を数えて子へ降りる。
	usageScopeTree usageScope = iota
	// usageScopeRoot は root 直下で、予約 namespace と登録済み対象の先頭成分、short ID 形の directory へだけ降りる。
	usageScopeRoot
	// usageScopeGate は予約 namespace の途中成分で、その namespace へ続く子だけへ降りる。
	usageScopeGate
	// usageScopeNamespace は slot を並べる層で、登録の有無を問わず子 directory へ降りる。直下のファイルは数えない。
	usageScopeNamespace
	// usageScopeSnapshots は workspace snapshot の置き場で、直下のファイルだけを数え、子 directory へは降りない。
	usageScopeSnapshots
)

// usageDirectory は測定中の directory 1 個の位置と、対応する共有元 directory である。
// dir と main は task が閉じる descriptor で、main が nil の subtree では共有判定を行わない。
// repo は repository の内側かを表し、共有元を開けなかった subtree でも Compared の対象は変えない。repoName はその内訳の足し先である。
type usageDirectory struct {
	name     string
	slotID   string
	scope    usageScope
	repo     bool
	repoName string
	dir      *os.File
	main     *os.File
}

// countsFiles はこの層の非 directory entry を集計へ入れてよいかを返す。
// 登録済みのファイルは層を問わず数えるため、判定は登録に当たらなかった entry にだけ効く。
func (d usageDirectory) countsFiles() bool {
	return d.scope == usageScopeTree || d.scope == usageScopeSnapshots
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

// usageFileSample は directory 内で見つけた「ファイル 1 個の登録」1 件分の集計である。
// directory の slot とは別の slot に属し得るため、sample へは混ぜずに足し先を持ったまま運ぶ。
type usageFileSample struct {
	slotID string
	usage  SlotUsage
}

// usageDirectoryTotals は directory 1 個分の集計である。
// directory 内の entry は必ず同じ slot に属するため、slot 別に分けずに 1 つの sample へ足す。
// ファイル単位で登録された対象だけがこの前提から外れるので、files に足し先ごと分けて持つ。
type usageDirectoryTotals struct {
	unmanaged int64
	logical   int64
	allocated int64
	shared    int64
	sample    SlotUsage
	files     []usageFileSample
	cache     []usageCacheEntry
}

// usageScan は 1 回の測定で共有する対象・集計先・cache である。
// 走査は directory 単位で並列に走るので、集計と cache は mutex の下でだけ触る。
type usageScan struct {
	ctx      context.Context
	previous SharedFileCache
	slots    map[string]string
	files    map[string]string
	entries  map[string]usageScope
	repos    map[string]usageRepository
	gate     chan struct{}
	wait     sync.WaitGroup

	mu      sync.Mutex
	usage   RootUsage
	cache   SharedFileCache
	failure error
}

func newUsageScan(ctx context.Context, targets []SlotUsageTarget, namespaces []UsageNamespace, previous SharedFileCache) *usageScan {
	usage := RootUsage{Slots: map[string]SlotUsage{}}
	slots, files, repos := usagePrefixes(targets, usage.Slots)
	return &usageScan{
		ctx: ctx, previous: previous, slots: slots, files: files, entries: usageEntryScopes(targets, namespaces), repos: repos,
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
	s.record(task, &totals)
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
	child, descend, err := s.childOf(task, name, leaf)
	if err != nil {
		return s.tolerate(err)
	}
	if !descend {
		return true
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
// ファイル 1 個で登録された対象は directory の境界に現れないので、登録外かを判定する前にファイルの表を引く。
func (s *usageScan) count(task usageDirectory, totals *usageDirectoryTotals, name, leaf string, stat *unix.Stat_t) {
	allocated := stat.Blocks * 512
	if slotID, registered := s.files[name]; registered {
		s.countRegisteredFile(totals, slotID, allocated, stat)
		return
	}
	if !task.countsFiles() {
		// 予約 namespace の外と、slot を並べる層に置かれたファイルは wx の管理対象ではない。
		// 列挙が拾わない実体を測ると、表示した未管理量を `wx clear --unmanaged` で解消できなくなる。
		return
	}
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
	if !usageFileCanCompare(task, regular, stat.Size, cowAvailable()) {
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

func usageFileCanCompare(task usageDirectory, regular bool, size int64, available bool) bool {
	return task.repo && regular && size != 0 && available
}

// countRegisteredFile はファイル 1 個で登録された対象を、その登録の slot と root 合計へ足す。
// 共有元を持たない archive なので CoW の比較は行わず、Compared にも数えない。
func (s *usageScan) countRegisteredFile(totals *usageDirectoryTotals, slotID string, allocated int64, stat *unix.Stat_t) {
	sample := SlotUsage{Files: 1, AllocatedBytes: allocated}
	totals.allocated += allocated
	if stat.Mode&unix.S_IFMT == unix.S_IFREG {
		sample.LogicalBytes = stat.Size
		totals.logical += stat.Size
	}
	totals.files = append(totals.files, usageFileSample{slotID: slotID, usage: sample})
}

// childScope は子 directory へ降りてよいかと、降りた先の層を返す。
// root と予約 namespace の途中成分では、登録済み対象と予約名の成分、および slot を並べる short ID 形の名前だけを通す。
func (s *usageScan) childScope(task usageDirectory, name, leaf string) (usageScope, bool) {
	if _, boundary := s.slots[name]; boundary {
		return usageScopeTree, true
	}
	switch task.scope {
	case usageScopeTree, usageScopeNamespace:
		return usageScopeTree, true
	case usageScopeSnapshots:
		// 登録は directory を指さないので、この層の directory は列挙も削除もされない。測るだけでは合わなくなる。
		return 0, false
	case usageScopeRoot, usageScopeGate:
		if scope, allowed := s.entries[name]; allowed {
			return scope, true
		}
		if task.scope == usageScopeRoot && domain.ValidShortID(leaf) {
			return usageScopeNamespace, true
		}
		return 0, false
	}
	return 0, false
}

// childOf は子 directory の task を組む。slot と repository の境界はここで切り替え、
// repository の内側では共有元も同じ 1 成分だけ降りる。共有元を開けない subtree は共有なしとして数える。
// 降りてはいけない子には descend=false を返し、descriptor を開かない。
func (s *usageScan) childOf(task usageDirectory, name, leaf string) (usageDirectory, bool, error) {
	scope, descend := s.childScope(task, name, leaf)
	if !descend {
		return usageDirectory{}, false, nil
	}
	child := usageDirectory{name: name, slotID: task.slotID, scope: scope, repo: task.repo, repoName: task.repoName}
	if scope != usageScopeTree {
		// slot より上の層は slot に属さない。登録外 directory と同じ扱いにし、内訳の足し先を持たせない。
		child.slotID, child.repo, child.repoName = "", false, ""
	}
	if slotID, boundary := s.slots[name]; boundary {
		child.slotID, child.repo, child.repoName = slotID, false, ""
	}
	dir, err := openUsageChild(task.dir, leaf)
	if err != nil {
		return usageDirectory{}, false, err
	}
	child.dir = dir
	switch repository, boundary := s.repos[name]; {
	case boundary:
		// 登録と違う slot の下に現れた repository path は、共有元を持たない普通の directory として数える。
		child.repo = repository.slotID == child.slotID
		child.repoName = ""
		if child.repo {
			child.repoName = repository.dirName
			child.main = openUsageRepository(repository.mainPath)
		}
	case child.repo && task.main != nil:
		child.main = openUsageChildOrNil(task.main, leaf)
	}
	return child, true, nil
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
// directory は 1 つの slot と 1 つの repository にしか属さないため、内訳の足し先は task が決める。
func (s *usageScan) record(task usageDirectory, totals *usageDirectoryTotals) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage.UnmanagedBytes += totals.unmanaged
	s.usage.LogicalBytes += totals.logical
	s.usage.AllocatedBytes += totals.allocated
	s.usage.SharedBytes += totals.shared
	if task.slotID != "" {
		sample := s.usage.Slots[task.slotID]
		sample.Files += totals.sample.Files
		sample.LogicalBytes += totals.sample.LogicalBytes
		sample.AllocatedBytes += totals.sample.AllocatedBytes
		sample.Compared += totals.sample.Compared
		sample.SharedFiles += totals.sample.SharedFiles
		sample.SharedBytes += totals.sample.SharedBytes
		if task.repoName != "" {
			if sample.Repositories == nil {
				sample.Repositories = map[string]RepositoryUsage{}
			}
			repository := sample.Repositories[task.repoName]
			repository.Files += totals.sample.Files
			repository.LogicalBytes += totals.sample.LogicalBytes
			repository.AllocatedBytes += totals.sample.AllocatedBytes
			repository.SharedBytes += totals.sample.SharedBytes
			sample.Repositories[task.repoName] = repository
		}
		s.usage.Slots[task.slotID] = sample
	}
	for _, file := range totals.files {
		sample := s.usage.Slots[file.slotID]
		sample.Files += file.usage.Files
		sample.LogicalBytes += file.usage.LogicalBytes
		sample.AllocatedBytes += file.usage.AllocatedBytes
		s.usage.Slots[file.slotID] = sample
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
