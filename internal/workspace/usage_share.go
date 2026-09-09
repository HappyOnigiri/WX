package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// sharedLeaf は slot 側の 1 件と共有元の同じ相対 path を比べ、共有判定とその判定を cache へ残せるかを返す。
// 判定できない事情（共有元の欠落・symlink 化・open 失敗・offset の取得失敗）はすべて共有なしとして扱い、cache へは残さない。
// どちらのファイルも読むだけで、内容も metadata も変更しない。
func (s *usageScan) sharedLeaf(task usageDirectory, leaf, name string, target *unix.Stat_t) (SharedFileState, bool) {
	if task.main == nil {
		return SharedFileState{}, false
	}
	var source unix.Stat_t
	if err := unix.Fstatat(int(task.main.Fd()), leaf, &source, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return SharedFileState{}, false
	}
	if source.Mode&unix.S_IFMT != unix.S_IFREG {
		return SharedFileState{}, false
	}
	state := SharedFileState{Slot: fileIdentityOf(target), Source: fileIdentityOf(&source)}
	// 共有を壊す書き込みは必ず ctime を更新するため、両側が変わっていない回は前回の判定をそのまま使い、ファイルを開かない。
	if before, found := s.previous[name]; found && before.Slot == state.Slot && before.Source == state.Source {
		return before, true
	}
	if source.Size != target.Size {
		// size が違えば offset を比べるまでもなく共有していない。
		return state, true
	}
	return compareUsageOffsets(task, leaf, state, target.Size)
}

// compareUsageOffsets は両側を読み取り専用で開き、物理 offset の一致を block 共有の証拠として使う。
// 比較の前後で identity が変わった回は判定を cache へ残さず、次回の測定で現在の実体を見直す。
func compareUsageOffsets(task usageDirectory, leaf string, state SharedFileState, size int64) (SharedFileState, bool) {
	source, err := openCOWLeaf(task.main, leaf)
	if err != nil {
		return SharedFileState{}, false
	}
	defer func() { _ = source.Close() }()
	target, err := openCOWLeaf(task.dir, leaf)
	if err != nil {
		return SharedFileState{}, false
	}
	defer func() { _ = target.Close() }()
	shared, comparable := compareCOWOffsets(source, target, size)
	if !comparable || !sameUsageIdentities(source, target, state) {
		return SharedFileState{}, false
	}
	state.Shared = shared
	return state, true
}

// sameUsageIdentities は比較に使った descriptor が、fstatat で見た実体のまま変わっていないかを確かめる。
// 開いた inode の検査もこの 1 回に兼ねる。ctime は巻き戻らないので、開く前後で 2 回 fstat する必要はない。
func sameUsageIdentities(source, target *os.File, state SharedFileState) bool {
	sourceIdentity, err := usageFileIdentity(source)
	if err != nil {
		return false
	}
	targetIdentity, err := usageFileIdentity(target)
	if err != nil {
		return false
	}
	return sourceIdentity == state.Source && targetIdentity == state.Slot
}

// fileIdentityOf は fstat・fstatat の結果から cache 判定用の identity を組む。
// Dev の符号は platform で違うが、比較にしか使わないため uint64 へ寄せる。
func fileIdentityOf(stat *unix.Stat_t) fileIdentity {
	return fileIdentity{Dev: uint64(stat.Dev), Ino: stat.Ino, CtimeNanos: stat.Ctim.Nano()}
}

func usageFileIdentity(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentityOf(&stat), nil
}

// compareCOWOffsets は offset を比較し、共有判定を得られたかどうかも返す。
// offset の取得に失敗した場合は、非共有という判定を cache に固定しない。
func compareCOWOffsets(source, target *os.File, size int64) (shared, comparable bool) {
	for _, offset := range []int64{0, size - 1} {
		left, err := physicalOffset(source, offset)
		if err != nil {
			return false, false
		}
		right, err := physicalOffset(target, offset)
		if err != nil {
			return false, false
		}
		if left != right {
			return false, true
		}
	}
	return true, true
}
