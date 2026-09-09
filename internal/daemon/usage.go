package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// rootUsageSample は lifecycle が測った root 1 世代分のディスク使用量。
// Status は要求のたびに測り直さず、この値と measuredAt をそのまま返す。
type rootUsageSample struct {
	bytes      int64
	allocated  int64
	unmanaged  int64
	shared     int64
	measuredAt time.Time
	err        string
}

// slotUsageSample は lifecycle が測った slot 1 個分の使用量と CoW 共有量。
// 貸出後の書き換えで共有が解けた分もここに現れるよう、準備時の記録ではなく毎回の実測で求める。
type slotUsageSample struct {
	usage      workspace.SlotUsage
	measuredAt time.Time
}

const (
	// rootUsageMeasurement は allocated_bytes の算出方法を表す。
	rootUsageMeasurement = "st_blocks_x_512"
	// rootUsagePendingMeasurement は最初の測定が終わる前の root を表す。bytes と allocated_bytes は 0 で、実際の使用量ではない。
	rootUsagePendingMeasurement = "pending"
	// slotSharingMeasurement は shared_bytes の算出方法を表す。先頭と末尾の物理 offset だけを比べるため上限側の推定になる。
	slotSharingMeasurement = "log2phys_first_last"
	// slotSharingUnsupported は CoW 共有を判定できない platform を表す。shared_bytes は 0 で、共有が無いことの証明ではない。
	slotSharingUnsupported = "unsupported"
)

// MeasurementPending は SlotView.Measurement と worktree root の measurement が、まだ最初の測定を終えていないことを表す値である。
// 消費側が測定済みの 0 と未測定を取り違えないよう、判定用の値をここで公開する。
const MeasurementPending = rootUsagePendingMeasurement

// measureRootUsage は root ごとの使用量を測り直して cache へ載せ替える。
// 走査量は root 配下の総ファイル数に比例するため、要求経路では呼ばず lifecycle の周期処理だけが更新する。
// ctx が切れた回は cache を据え置き、部分的な測定値で既存の値を壊さない。
func (m *Manager) measureRootUsage(ctx context.Context) {
	roots := m.knownRoots(ctx)
	targetsAt := time.Now().UTC()
	targets, err := m.slotUsageTargets(ctx)
	if err != nil {
		m.log.Warn("list managed usage locations", "error", err)
		return
	}
	samples := make(map[string]rootUsageSample, len(roots))
	slots := map[string]slotUsageSample{}
	caches := make(map[string]workspace.SharedFileCache, len(roots))
	for root := range roots {
		usage, cache, err := m.rootDirectoryUsage(ctx, root, targets[root], m.sharedFileCache(root))
		if ctx.Err() != nil {
			return
		}
		measuredAt := time.Now().UTC()
		sample := rootUsageSample{bytes: usage.LogicalBytes, allocated: usage.AllocatedBytes, unmanaged: usage.UnmanagedBytes, shared: usage.SharedBytes, measuredAt: measuredAt}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			sample.err = err.Error()
		}
		samples[root] = sample
		caches[root] = cache
		if sample.err != "" {
			continue
		}
		for slotID, slot := range usage.Slots {
			slots[slotID] = slotUsageSample{usage: slot, measuredAt: measuredAt}
		}
	}
	m.mu.Lock()
	// 対象一覧を撮った後に準備が終わった slot はこの回の測定に入らない。
	// 上書きすると measureSlotUsage の結果が消えて次の周期まで pending へ戻るため、より新しい実測だけ残す。
	for slotID, sample := range m.slotUsage {
		if _, remeasured := slots[slotID]; !remeasured && sample.measuredAt.After(targetsAt) {
			slots[slotID] = sample
		}
	}
	m.rootUsage, m.slotUsage, m.sharedFiles = samples, slots, caches
	m.mu.Unlock()
}

// forgetSlotUsage は削除の終わった slot の実測を cache から外し、root 合計からもその分を差し引く。
// `wx clear` や GC で消えた割当量を、次の周期測定まで Status の Disk へ残さないためである。
// 実体はもう無いので測定し直さずに引くだけとし、測定時刻は据え置いて root の他の部分の鮮度を偽らない。
func (m *Manager) forgetSlotUsage(slotID, root string) {
	root = filepath.Clean(root)
	m.mu.Lock()
	defer m.mu.Unlock()
	sample, measured := m.slotUsage[slotID]
	delete(m.slotUsage, slotID)
	rootSample, known := m.rootUsage[root]
	if !measured || !known || rootSample.err != "" {
		return
	}
	rootSample.bytes = subtractUsage(rootSample.bytes, sample.usage.LogicalBytes)
	rootSample.allocated = subtractUsage(rootSample.allocated, sample.usage.AllocatedBytes)
	rootSample.shared = subtractUsage(rootSample.shared, sample.usage.SharedBytes)
	m.rootUsage[root] = rootSample
}

// subtractUsage は差し引きの結果を 0 で止める。測定後に増えた slot を引くと負になり得るためである。
func subtractUsage(total, removed int64) int64 {
	if removed >= total {
		return 0
	}
	return total - removed
}

// scheduleSlotUsageMeasurement は準備完了の直後に、その slot だけの測定と root 合計の測り直しを background へ回す。
// 貸出の応答へ走査時間を持ち込まないため同期では測らず、停止中で受け付けられない場合は周期測定へ委ねる。
func (m *Manager) scheduleSlotUsageMeasurement(slotID string) {
	m.startBackground(func() {
		m.measureSlotUsage(m.ctx, slotID)
		m.remeasureRootUsage()
	})
}

// remeasureRootUsage は使用量が変わった直後の測り直しを1本へ畳んで background へ回す。
// 周期測定を待つと Disk が `Discovery.ReconcileInterval` の間だけ古い合計を出し続けるため、変化の直後に追随させる。
// 走っている間に届いた要求は落とさずに畳み、続けて追加の1巡を行う。
func (m *Manager) remeasureRootUsage() {
	if !m.claimRootUsageMeasurement() {
		return
	}
	started := m.startBackground(func() {
		for {
			m.measureRootUsage(m.ctx)
			if !m.nextRootUsageMeasurement() {
				return
			}
		}
	})
	if !started {
		// 停止中は測り直せないので実行権を手放し、次の起動後の周期測定へ委ねる。
		m.usageMu.Lock()
		m.usageRunning, m.usageDirty = false, false
		m.usageMu.Unlock()
	}
}

// claimRootUsageMeasurement は測り直しの実行権を取る。既に走っていれば dirty を立てて false を返す。
func (m *Manager) claimRootUsageMeasurement() bool {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	if m.usageRunning {
		m.usageDirty = true
		return false
	}
	m.usageRunning = true
	return true
}

// nextRootUsageMeasurement は畳まれた要求が残っていれば実行権を保ったまま true を返し、なければ手放す。
func (m *Manager) nextRootUsageMeasurement() bool {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	if m.usageDirty {
		m.usageDirty = false
		return true
	}
	m.usageRunning = false
	return false
}

// measureSlotUsage は準備の終わった slot 1 個だけを測って cache へ載せる。
// root 全体の周期測定を待たせずに方式と使用量を出すためだけの処理なので、失敗しても準備結果は変えず記録に留める。
func (m *Manager) measureSlotUsage(ctx context.Context, slotID string) {
	locations, err := m.store.SlotUsageLocationsForSlot(ctx, slotID)
	if err != nil {
		m.log.Warn("list slot usage location", "slot_id", slotID, "error", err)
		return
	}
	if len(locations) == 0 {
		return
	}
	target := workspace.SlotUsageTarget{SlotID: slotID, RelPath: locations[0].RelPath, Repositories: map[string]string{}}
	for _, location := range locations {
		target.Repositories[location.DirName] = location.MainPath
	}
	root := filepath.Clean(locations[0].RootPath)
	owner, release, err := m.usageRootDescriptor(root)
	if err != nil {
		m.log.Warn("open root for slot usage", "slot_id", slotID, "root", root, "error", err)
		return
	}
	defer release()
	usage, cache, err := workspace.MeasureSlotUsage(ctx, owner, target, m.sharedFileCache(root))
	if err != nil {
		if ctx.Err() == nil {
			m.log.Warn("measure slot usage", "slot_id", slotID, "error", err)
		}
		return
	}
	measuredAt := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slotUsage[slotID] = slotUsageSample{usage: usage, measuredAt: measuredAt}
	// 公開済みの cache は measureRootUsage が previous として読むため、書き換えずに差し替える。
	// 部分走査した slot の prefix だけは結果で置き換え、検証できなかった古い entry を残さない。
	m.sharedFiles[root] = mergeSlotSharedFileCache(m.sharedFiles[root], cache, target.RelPath)
}

// mergeSlotSharedFileCache は他 slot の cache を保ったまま、測定対象 slot の entry を今回の結果へ差し替える。
// source の消失や symlink 化で今回の cache に現れない path は、次回に古い判定を再利用しないよう除外する。
func mergeSlotSharedFileCache(previous, measured workspace.SharedFileCache, relPath string) workspace.SharedFileCache {
	prefix := path.Clean(filepath.ToSlash(relPath))
	merged := make(workspace.SharedFileCache, len(previous)+len(measured))
	for name, shared := range previous {
		clean := path.Clean(filepath.ToSlash(name))
		if clean == prefix || strings.HasPrefix(clean, prefix+"/") {
			continue
		}
		merged[name] = shared
	}
	for name, shared := range measured {
		merged[name] = shared
	}
	return merged
}

// slotUsageTargets は測定対象の slot を root ごとにまとめる。
// DB を読めない回は前回値を維持し、管理対象を登録外の容量へ誤分類しない。
func (m *Manager) slotUsageTargets(ctx context.Context) (map[string][]workspace.SlotUsageTarget, error) {
	locations, err := m.store.SlotUsageLocations(ctx)
	if err != nil {
		return nil, err
	}
	targets := map[string][]workspace.SlotUsageTarget{}
	indexes := map[string]int{}
	for _, location := range locations {
		root := filepath.Clean(location.RootPath)
		key := root + "\x00" + location.SlotID
		index, known := indexes[key]
		if !known {
			index = len(targets[root])
			indexes[key] = index
			targets[root] = append(targets[root], workspace.SlotUsageTarget{SlotID: location.SlotID, RelPath: location.RelPath, Repositories: map[string]string{}})
		}
		targets[root][index].Repositories[location.DirName] = location.MainPath
	}
	return targets, nil
}

func (m *Manager) sharedFileCache(root string) workspace.SharedFileCache {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sharedFiles[filepath.Clean(root)]
}

func (m *Manager) rootDirectoryUsage(ctx context.Context, root string, targets []workspace.SlotUsageTarget, previous workspace.SharedFileCache) (workspace.RootUsage, workspace.SharedFileCache, error) {
	// root の旧 identity に依存せず、削除と同じ登録 path の現在の実体を測る。
	owner, err := os.OpenRoot(root)
	if err != nil {
		return workspace.RootUsage{}, nil, err
	}
	defer func() { _ = owner.Close() }()
	return workspace.MeasureRootUsage(ctx, owner, targets, previous)
}

// usageRootDescriptor は測定用に root を pin し、path 名ではなく descriptor で走査できるようにする。
// path walk では reload 後の置換 directory へ渡り得るため、使用量も pin 済み descriptor 経由で測る。
func (m *Manager) usageRootDescriptor(root string) (*os.Root, func(), error) {
	root = filepath.Clean(root)
	m.mu.RLock()
	_, known := m.roots[root]
	m.mu.RUnlock()
	if !known {
		return nil, nil, fmt.Errorf("%w: root is not registered", state.ErrOwnership)
	}
	_, release, err := m.existingRootDescriptor(root)
	if err != nil {
		return nil, nil, err
	}
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		release()
		return nil, nil, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		release()
		return nil, nil, err
	}
	return owner, release, nil
}
