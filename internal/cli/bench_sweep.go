package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
)

// benchCurrentConfigLabel は上書きを指定しなかった測定の表記である。
// 値を並べても daemon の実効設定は測定時点でしか分からないため、行の見出しでは設定名を名乗らない。
const benchCurrentConfigLabel = "current"

// BenchConfig は run に適用した準備設定の上書きである。
// Label は表と `--json` で行を指す表記で、指定した key だけを `key=value` で並べる。
type BenchConfig struct {
	Label         string `json:"label"`
	CopyMode      string `json:"copy_mode,omitempty"`
	COWMinSizeKiB *int   `json:"cow_min_size_kib,omitempty"`
}

// BenchUsage は返却の直前に引いた slot の使用量である。
// ExclusiveBytes が CoW 共有を割り引いた専有量で、Measurement は算出方法を表す。
type BenchUsage struct {
	Measurement    string `json:"measurement"`
	CopyMode       string `json:"copy_mode,omitempty"`
	Files          int    `json:"files,omitempty"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	SharedBytes    int64  `json:"shared_bytes"`
	ExclusiveBytes int64  `json:"exclusive_bytes"`
	MeasuredAt     string `json:"measured_at,omitempty"`
}

// BenchStat は同じ設定で繰り返した測定の分布である。--runs が 1 のときは3つが同値になる。
type BenchStat struct {
	MinMS    int64 `json:"min_ms"`
	MedianMS int64 `json:"median_ms"`
	MaxMS    int64 `json:"max_ms"`
}

// BenchConfigSummary は設定1つ分の集計である。Runs は成功した回数、Failed は失敗した回数を指す。
// ExclusiveBytes と SharedBytes は使用量を測れた回の中央値で、1回も測れなかった設定では nil になる。
type BenchConfigSummary struct {
	Config         BenchConfig `json:"config"`
	Runs           int         `json:"runs"`
	Failed         int         `json:"failed"`
	EarlyReady     BenchStat   `json:"early_ready"`
	FullReady      BenchStat   `json:"full_ready"`
	ExclusiveBytes *int64      `json:"exclusive_bytes"`
	SharedBytes    *int64      `json:"shared_bytes"`
}

// benchConfigOf は上書きを出力用の設定表記へ直す。
func benchConfigOf(override config.PrepareOverride) BenchConfig {
	label := override.String()
	if label == "" {
		label = benchCurrentConfigLabel
	}
	return BenchConfig{Label: label, CopyMode: override.CopyMode, COWMinSizeKiB: override.COWMinSizeKiB}
}

// summarizeBenchConfigs は run を設定ごとにまとめる。行の順は最初に測った順で、指定した順を保つ。
// 失敗した回は時間の分布に入れず、件数だけを Failed に残す。
func summarizeBenchConfigs(runs []BenchRun) []BenchConfigSummary {
	order := []string{}
	early, full := map[string][]int64{}, map[string][]int64{}
	exclusive, shared := map[string][]int64{}, map[string][]int64{}
	configs, failed := map[string]BenchConfig{}, map[string]int{}
	for _, run := range runs {
		label := run.Config.Label
		if _, seen := configs[label]; !seen {
			order = append(order, label)
			configs[label] = run.Config
		}
		if run.Error != "" {
			failed[label]++
			continue
		}
		early[label] = append(early[label], run.EarlyReadyMS)
		full[label] = append(full[label], run.FullReadyMS)
		if run.Usage != nil {
			exclusive[label] = append(exclusive[label], run.Usage.ExclusiveBytes)
			shared[label] = append(shared[label], run.Usage.SharedBytes)
		}
	}
	out := make([]BenchConfigSummary, 0, len(order))
	for _, label := range order {
		summary := BenchConfigSummary{Config: configs[label], Runs: len(full[label]), Failed: failed[label]}
		if summary.Runs > 0 {
			summary.EarlyReady = benchStatOf(early[label])
			summary.FullReady = benchStatOf(full[label])
		}
		summary.ExclusiveBytes = benchMedianBytes(exclusive[label])
		summary.SharedBytes = benchMedianBytes(shared[label])
		out = append(out, summary)
	}
	return out
}

func benchStatOf(values []int64) BenchStat {
	return BenchStat{MinMS: minOf(values), MedianMS: medianOf(values), MaxMS: maxOf(values)}
}

// benchMedianBytes は測れた回の中央値を返す。1回も測れていない場合は nil を返し、表では `-` になる。
func benchMedianBytes(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	median := medianOf(values)
	return &median
}

// printBenchConfigs は設定ごとの比較行を出す。
// 上書きを指定しない1通りの測定では run ごとの出力と summary に同じ値が出ているため出さない。
func printBenchConfigs(summaries []BenchConfigSummary) {
	if len(summaries) == 0 || (len(summaries) == 1 && summaries[0].Config.Label == benchCurrentConfigLabel) {
		return
	}
	fmt.Println("comparison by configuration (min/median/max, usage is the median of the measured runs)")
	fmt.Printf("  %-28s %5s %5s  %-24s %-24s %12s %12s\n", "config", "runs", "fail", "EARLY READY", "FULL READY", "exclusive", "shared")
	for _, summary := range summaries {
		fmt.Printf("  %-28s %5d %5d  %-24s %-24s %12s %12s\n",
			summary.Config.Label, summary.Runs, summary.Failed,
			formatBenchStat(summary.Runs, summary.EarlyReady), formatBenchStat(summary.Runs, summary.FullReady),
			formatBenchOptionalBytes(summary.ExclusiveBytes), formatBenchOptionalBytes(summary.SharedBytes))
	}
}

// formatBenchStat は分布を1列へ畳む。成功した回がない設定は時間を名乗らず `-` を出す。
func formatBenchStat(runs int, stat BenchStat) string {
	if runs == 0 {
		return "-"
	}
	return strings.Join([]string{
		formatBenchDuration(stat.MinMS), formatBenchDuration(stat.MedianMS), formatBenchDuration(stat.MaxMS),
	}, "/")
}

// formatBenchOptionalBytes は上限内に測れなかった使用量を `-` で出す。0 と測れなかったことを混ぜないためである。
func formatBenchOptionalBytes(value *int64) string {
	if value == nil {
		return "-"
	}
	return formatBenchBytes(*value)
}

// formatBenchBytes は使用量を 2 進接頭辞で出す。`wx status` の Disk 行と読み比べられるよう同じ単位を使う。
func formatBenchBytes(value int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	negative := value < 0
	n := float64(value)
	if negative {
		n = -n
	}
	unit := 0
	for n >= 1024 && unit < len(units)-1 {
		n /= 1024
		unit++
	}
	var number string
	if unit == 0 || n == math.Trunc(n) {
		number = strconv.FormatFloat(n, 'f', 0, 64)
	} else {
		number = strings.TrimRight(strings.TrimRight(strconv.FormatFloat(n, 'f', 2, 64), "0"), ".")
	}
	if negative {
		number = "-" + number
	}
	return number + " " + units[unit]
}
