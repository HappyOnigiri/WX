package workspace

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
)

// SlotUsageTarget は 1 slot 分の測定対象である。
// RelPath は root 相対の slot path で、Repositories は slot 相対の repository directory 名から main worktree path への対応である。
// File は RelPath が directory ではなくファイル 1 個を指すことを表し、走査は directory 境界ではなくファイル名の一致で突き合わせる。
type SlotUsageTarget struct {
	SlotID       string
	RelPath      string
	Repositories map[string]string
	File         bool
}

// UsageNamespace は root 直下で走査を許す予約 namespace 1 個である。
// Path は root 相対の slash 区切り path、Files はその directory 直下のファイルが測定対象になり得るかである。
// 予約名の綴りは呼び出し側が持つ。測定側が wx の layout を知らずに済むよう、降下条件は引数として受け取る。
type UsageNamespace struct {
	Path  string
	Files bool
}

// SlotUsage は測定 1 回分の slot 使用量である。
// SharedBytes は main worktree と block を共有していると判定したファイルの allocated 合計、Compared はその判定を試みたファイル数である。
// Repositories は repository directory 名ごとの内訳で、repository の外にある slot 直下のファイルを含まないため合計は一致しない。
type SlotUsage struct {
	Files          int                        `json:"files"`
	LogicalBytes   int64                      `json:"logical_bytes"`
	AllocatedBytes int64                      `json:"allocated_bytes"`
	Compared       int                        `json:"compared_files"`
	SharedFiles    int                        `json:"shared_files"`
	SharedBytes    int64                      `json:"shared_bytes"`
	Repositories   map[string]RepositoryUsage `json:"repositories,omitempty"`
}

// RepositoryUsage は slot 内の repository 1 個分の使用量である。
type RepositoryUsage struct {
	Files          int   `json:"files"`
	LogicalBytes   int64 `json:"logical_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
	SharedBytes    int64 `json:"shared_bytes"`
}

// RootUsage は root 1 世代分の合計と、その root 上にある slot ごとの内訳である。
// SharedBytes は slot ごとの SharedBytes の合計で、AllocatedBytes のうち main worktree と block を共有している分である。
// UnmanagedBytes は予約 namespace の配下で登録済み対象のどれにも属さない実体の割当量で、AllocatedBytes には含めず共有判定もしない。予約 namespace の外にある実体はどちらにも数えない。
type RootUsage struct {
	UnmanagedBytes int64
	LogicalBytes   int64
	AllocatedBytes int64
	SharedBytes    int64
	Slots          map[string]SlotUsage
}

// fileIdentity は開いた regular file の実体と変更時刻を識別する。
// slot と共有元を同じ cache entry に保存するため、判定側と共有元側を区別して保持する。
type fileIdentity struct {
	Dev        uint64
	Ino        uint64
	CtimeNanos int64
}

// SharedFileState は 1 ファイルの共有判定と、その判定が有効な slot・共有元の identity である。
type SharedFileState struct {
	Slot   fileIdentity
	Source fileIdentity
	Shared bool
}

// SharedFileCache は root 相対 path をキーに前回の共有判定を保持する。
// slot と共有元のどちらかが変わっていれば再判定し、両方が変わっていない場合だけ判定を省ける。
type SharedFileCache map[string]SharedFileState

// SharingSupported は CoW の共有判定がこの platform で行えるかを返す。
func SharingSupported() bool { return cowAvailable() }

type usageRepository struct {
	slotID   string
	dirName  string
	mainPath string
}

// MeasureRootUsage は pin 済み root を 1 度だけ walk し、root 合計と slot ごとの使用量を返す。
// 共有判定は main worktree の同じ path を開いて物理 offset を比べるだけで、どちらのファイルも内容・metadata を変更しない。
// previous に前回の cache を渡すと slot と共有元の identity が変わっていないファイルの判定を再利用する。返す cache は今回 walk したファイルだけを含む。
// root 直下は namespaces と登録済み対象の先頭成分、および slot を並べる short ID 形の directory へしか降りない。
// 無関係な実体を walk しないことで、報告する使用量を `wx clear --unmanaged` が消せる範囲と一致させる。
// commentlint:allow-long -- 共有判定の非破壊性と、降下範囲を絞る理由を呼び出し側へ 1 か所で示す
func MeasureRootUsage(ctx context.Context, root *os.Root, targets []SlotUsageTarget, namespaces []UsageNamespace, previous SharedFileCache) (RootUsage, SharedFileCache, error) {
	scan := newUsageScan(ctx, targets, namespaces, previous)
	base, err := root.Open(".")
	if err != nil {
		return RootUsage{Slots: map[string]SlotUsage{}}, nil, err
	}
	scan.measure(usageDirectory{name: ".", dir: base, scope: usageScopeRoot})
	return scan.finish()
}

// MeasureSlotUsage は root 配下の slot 1 個だけを walk し、その slot の使用量と共有量を返す。
// root 合計は求めないため、準備の終わった slot を root 全体の測定を待たずに反映する用途に限る。
func MeasureSlotUsage(ctx context.Context, root *os.Root, target SlotUsageTarget, previous SharedFileCache) (SlotUsage, SharedFileCache, error) {
	relative := path.Clean(target.RelPath)
	scan := newUsageScan(ctx, []SlotUsageTarget{target}, nil, previous)
	slotID, measurable := scan.slots[relative]
	if !measurable {
		// ファイル 1 個の登録は directory として開けないため、部分測定の対象にしない。
		return SlotUsage{}, nil, fmt.Errorf("slot %s has no measurable path under the root", target.SlotID)
	}
	// slot が消えていれば error にして、呼び出し側がこの回の測定を諦められるようにする。
	dir, err := openUsageDirectory(root, relative)
	if err != nil {
		return SlotUsage{}, nil, err
	}
	scan.measure(usageDirectory{name: relative, dir: dir, slotID: slotID, scope: usageScopeTree})
	usage, cache, err := scan.finish()
	return usage.Slots[slotID], cache, err
}

// usagePrefixes は measure 用の prefix 表を組み、slot ごとの空 sample を用意する。
// 表のキーは root 相対 path なので、走査は降りた先の path を引くだけで slot と repository の境界を判別できる。
// directory の登録とファイルの登録は別の表に分ける。前者は降下時に、後者は entry 1 件ごとに引くためである。
func usagePrefixes(targets []SlotUsageTarget, samples map[string]SlotUsage) (slots, files map[string]string, repositories map[string]usageRepository) {
	slots, files, repositories = map[string]string{}, map[string]string{}, map[string]usageRepository{}
	for _, target := range targets {
		relative := path.Clean(target.RelPath)
		if target.SlotID == "" || relative == "." || relative == "/" || strings.HasPrefix(relative, "..") {
			continue
		}
		samples[target.SlotID] = SlotUsage{}
		if target.File {
			files[relative] = target.SlotID
			continue
		}
		slots[relative] = target.SlotID
		for directory, mainPath := range target.Repositories {
			if directory == "" || mainPath == "" {
				continue
			}
			repositories[path.Join(relative, directory)] = usageRepository{slotID: target.SlotID, dirName: directory, mainPath: mainPath}
		}
	}
	return slots, files, repositories
}

// usageEntryScopes は root 直下から降りてよい path と、降りた先で適用する scope を組む。
// 予約 namespace の途中成分は gate として通過だけを許し、登録済み対象の先頭成分は slot を並べる namespace として扱う。
func usageEntryScopes(targets []SlotUsageTarget, namespaces []UsageNamespace) map[string]usageScope {
	entries := map[string]usageScope{}
	for _, namespace := range namespaces {
		relative := path.Clean(namespace.Path)
		if relative == "." || relative == "/" || strings.HasPrefix(relative, "..") {
			continue
		}
		components := strings.Split(relative, "/")
		for index := 1; index < len(components); index++ {
			prefix := path.Join(components[:index]...)
			if _, known := entries[prefix]; !known {
				entries[prefix] = usageScopeGate
			}
		}
		scope := usageScopeNamespace
		if namespace.Files {
			scope = usageScopeSnapshots
		}
		entries[relative] = scope
	}
	for _, target := range targets {
		relative := path.Clean(target.RelPath)
		if target.SlotID == "" || relative == "." || relative == "/" || strings.HasPrefix(relative, "..") {
			continue
		}
		// 登録済み対象は必ず測れるようにする。先頭成分が予約名でも short ID でもない layout でも取りこぼさない。
		head := strings.Split(relative, "/")[0]
		if _, known := entries[head]; !known {
			entries[head] = usageScopeNamespace
		}
	}
	return entries
}
