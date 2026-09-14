package workspace

import (
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

const (
	// submoduleMaxWorkers は submodule の検査と origin 復元、Git の clone 並列度の上限である。
	// clone・stat・config は待ち時間が支配的なので、CPU 数まで重ねると実時間が伸びる。
	submoduleMaxWorkers = 10
	// submoduleArgMaxBytes は Git の argv と -c 由来の設定値を安全に分割する上限である。
	// macOS の ARG_MAX と継承環境の余白を残し、大量の子でも E2BIG を準備失敗にしない。
	submoduleArgMaxBytes = 64 << 10
)

// submoduleStats は submodule 区間の件数と、並列 worker ごとの合計時間を集計する。
// 計測は準備結果を変えず、`wx bench` の固定費とファイル処理費の切り分けにだけ使う。
type submoduleStats struct {
	declared    atomic.Int64
	eligible    atomic.Int64
	skipped     atomic.Int64
	inspect     cowStage
	materialize cowStage
	origin      cowStage
}

// recordSubmodulePhases は submodule の段階別集計を準備の区間内訳へ移す。
// worker 間の合計なので親区間の実時間より大きくなり得る。
func (s *submoduleStats) recordSubmodulePhases(timings *PhaseTimings) {
	if s == nil || timings == nil {
		return
	}
	for _, counter := range []struct {
		name  string
		value int64
	}{
		{"submodule.declared", s.declared.Load()},
		{"submodule.eligible", s.eligible.Load()},
		{"submodule.skipped", s.skipped.Load()},
	} {
		timings.Add(counter.name, int(counter.value), 0)
	}
	for _, stage := range []struct {
		name  string
		stage *cowStage
	}{
		{"submodule.inspect", &s.inspect},
		{"submodule.materialize", &s.materialize},
		{"submodule.origin", &s.origin},
	} {
		timings.Add(stage.name, int(stage.stage.count.Load()), time.Duration(stage.stage.nanos.Load()))
	}
}

// submoduleWorkers は submodule 処理の並列度を返す。
// submoduleWorkerCount はテストが検査・origin 復元の順序を決定的にするための内部フックである。
func (p *Preparer) submoduleWorkers() int {
	if p.submoduleWorkerCount > 0 {
		return p.submoduleWorkerCount
	}
	return min(runtime.NumCPU(), submoduleMaxWorkers)
}

// argvSize は exec に渡す argv の概算バイト数を返す。各要素の区切り分を1 byte足す。
func argvSize(args []string) int {
	size := 0
	for _, arg := range args {
		size += len(arg) + 1
	}
	return size
}

// submoduleArgSize は argv と、Git が子プロセスへ渡す -c 設定値の概算バイト数を返す。
// 設定値は argv と環境変数の両方に現れるため二重に数え、継承環境の余白も残す。
func submoduleArgSize(args []string) int {
	size := argvSize(args)
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "-c" {
			size += len(args[index+1]) + 1
			index++
		}
	}
	return size
}

// batchSubmoduleArgs は argv と config 引数の合計が上限を超えない塊へ項目を分ける。
// 1件だけで上限を超える場合は、呼び出し側が Git の実際の制約を受けるため単独で残す。
func batchSubmoduleArgs[T any](items []T, base []string, itemArgs func(T) []string) [][]T {
	if len(items) == 0 {
		return nil
	}
	batches := make([][]T, 0, 1)
	current := make([]T, 0, min(len(items), 64))
	currentSize := submoduleArgSize(base)
	for _, item := range items {
		cost := submoduleArgSize(itemArgs(item))
		if len(current) > 0 && currentSize+cost > submoduleArgMaxBytes {
			batches = append(batches, current)
			current = make([]T, 0, min(len(items), 64))
			currentSize = submoduleArgSize(base)
		}
		current = append(current, item)
		currentSize += cost
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func batchSubmoduleMaterialization(items []materializedSubmodule, workers int) [][]materializedSubmodule {
	base := []string{"-c", "protocol.file.allow=always", "submodule", "update", "--init", "--jobs=" + strconv.Itoa(workers), "--"}
	return batchSubmoduleArgs(items, base, func(item materializedSubmodule) []string {
		return []string{"-c", "submodule." + item.module.name + ".url=" + item.source, item.module.path}
	})
}

func submoduleUpdateArgs(items []materializedSubmodule, workers int) []string {
	args := []string{"-c", "protocol.file.allow=always"}
	paths := make([]string, 0, len(items))
	for _, item := range items {
		// この config と path は同じループから組み立てる。対応がずれると Git が共有 config へ url/active を書く。
		args = append(args, "-c", "submodule."+item.module.name+".url="+item.source)
		paths = append(paths, item.module.path)
	}
	args = append(args, "submodule", "update", "--init", "--jobs="+strconv.Itoa(workers), "--")
	return append(args, paths...)
}
