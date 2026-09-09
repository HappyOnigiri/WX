package cli

import (
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

// benchSweepRun は集計の入力になる1回分の結果を組む。
func benchSweepRun(label string, early, full int64, exclusive, shared int64, measured bool) BenchRun {
	run := BenchRun{Source: "cold", Config: BenchConfig{Label: label}, EarlyReadyMS: early, FullReadyMS: full}
	if measured {
		run.Usage = &BenchUsage{Measurement: "log2phys_first_last", ExclusiveBytes: exclusive, SharedBytes: shared}
	}
	return run
}

// 設定ごとの集計は、最初に測った設定の順で並び、時間の分布と使用量の中央値を持つ。
// 失敗した回は分布に入れず件数だけを残す。
func TestSummarizeBenchConfigsGroupsRunsByConfiguration(t *testing.T) {
	failed := benchSweepRun("copy_mode=copy", 0, 0, 0, 0, false)
	failed.Error = "full ready: preparation failed"
	summaries := summarizeBenchConfigs([]BenchRun{
		benchSweepRun("cow_min_size_kib=16", 3300, 40500, 175<<20, 1116<<20, true),
		benchSweepRun("copy_mode=copy", 3400, 15200, 1291<<20, 0, true),
		benchSweepRun("cow_min_size_kib=16", 3100, 37700, 165<<20, 1126<<20, true),
		failed,
		benchSweepRun("cow_min_size_kib=16", 3500, 41000, 185<<20, 1106<<20, true),
	})
	if len(summaries) != 2 || summaries[0].Config.Label != "cow_min_size_kib=16" || summaries[1].Config.Label != "copy_mode=copy" {
		t.Fatalf("summaries=%+v, want the configurations in the order they were measured", summaries)
	}
	first := summaries[0]
	if first.Runs != 3 || first.Failed != 0 {
		t.Fatalf("first=%+v, want three successful runs", first)
	}
	if first.FullReady.MinMS != 37700 || first.FullReady.MedianMS != 40500 || first.FullReady.MaxMS != 41000 {
		t.Fatalf("full ready=%+v, want min/median/max over the three runs", first.FullReady)
	}
	if first.EarlyReady.MinMS != 3100 || first.EarlyReady.MedianMS != 3300 || first.EarlyReady.MaxMS != 3500 {
		t.Fatalf("early ready=%+v, want min/median/max over the three runs", first.EarlyReady)
	}
	if first.ExclusiveBytes == nil || *first.ExclusiveBytes != 175<<20 || first.SharedBytes == nil || *first.SharedBytes != 1116<<20 {
		t.Fatalf("usage exclusive=%v shared=%v, want the median of the measured runs", first.ExclusiveBytes, first.SharedBytes)
	}
	second := summaries[1]
	if second.Runs != 1 || second.Failed != 1 {
		t.Fatalf("second=%+v, want one successful and one failed run", second)
	}
}

// --runs が 1 のときも最小・中央値・最大を出し、3つが同値になる。
func TestSummarizeBenchConfigsRepeatsTheSingleRunAcrossMinMedianMax(t *testing.T) {
	summaries := summarizeBenchConfigs([]BenchRun{benchSweepRun("cow_min_size_kib=64", 3300, 28800, 951<<20, 340<<20, true)})
	if len(summaries) != 1 {
		t.Fatalf("summaries=%+v, want one configuration", summaries)
	}
	full := summaries[0].FullReady
	if full.MinMS != 28800 || full.MedianMS != 28800 || full.MaxMS != 28800 {
		t.Fatalf("full ready=%+v, want the single run repeated", full)
	}
	early := summaries[0].EarlyReady
	if early.MinMS != 3300 || early.MedianMS != 3300 || early.MaxMS != 3300 {
		t.Fatalf("early ready=%+v, want the single run repeated", early)
	}
}

// 使用量を1回も測れなかった設定は、時間だけの行として出す。
func TestPrintBenchConfigsReportsUnmeasuredUsageWithoutDroppingTheTimings(t *testing.T) {
	summaries := summarizeBenchConfigs([]BenchRun{
		benchSweepRun("cow_min_size_kib=16", 3300, 40500, 175<<20, 1116<<20, true),
		benchSweepRun("copy_mode=copy", 3400, 15200, 0, 0, false),
	})
	stdout := captureLeaseStdout(t, func() { printBenchConfigs(summaries) })
	for _, required := range []string{"cow_min_size_kib=16", "copy_mode=copy", "175 MiB", "40.500s", "15.200s"} {
		if !strings.Contains(stdout, required) {
			t.Fatalf("stdout=%q missing %s", stdout, required)
		}
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	unmeasured := lines[len(lines)-1]
	if !strings.HasSuffix(strings.TrimSpace(unmeasured), "-") {
		t.Fatalf("row=%q, want the unmeasured usage reported as -", unmeasured)
	}
}

// 上書きのない測定は設定名を名乗らず、比較行も出さない。従来の出力を変えないためである。
func TestPrintBenchConfigsStaysSilentForTheCurrentConfiguration(t *testing.T) {
	summaries := summarizeBenchConfigs([]BenchRun{benchSweepRun(benchCurrentConfigLabel, 3300, 40500, 0, 0, false)})
	stdout := captureLeaseStdout(t, func() { printBenchConfigs(summaries) })
	if stdout != "" {
		t.Fatalf("stdout=%q, want no comparison table without --config", stdout)
	}
}

// 上書きの表記は指定した key だけを並べ、run の行と比較行で同じ見出しになる。
func TestBenchConfigOfLabelsTheOverride(t *testing.T) {
	minSize := 0
	labeled := benchConfigOf(config.PrepareOverride{CopyMode: config.CopyModeCOW, COWMinSizeKiB: &minSize})
	if labeled.Label != "copy_mode=cow,cow_min_size_kib=0" {
		t.Fatalf("label=%q, want both keys including the zero lower bound", labeled.Label)
	}
	if benchConfigOf(config.PrepareOverride{}).Label != benchCurrentConfigLabel {
		t.Fatalf("label=%q, want the current configuration named", benchConfigOf(config.PrepareOverride{}).Label)
	}
}
