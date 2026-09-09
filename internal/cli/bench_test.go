package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// benchMeasurement は daemon が返す区間内訳の応答を組む。
func benchMeasurement() map[string]any {
	return map[string]any{"measurements": []daemon.PrepareMeasurement{{
		SlotID: "session", SessionID: "session", EarlyReadyMS: 1200, TotalMS: 4300,
		Phases: []daemon.PreparePhase{
			{Name: "checkout", Count: 1, MS: 900},
			{Name: "cow", Count: 1, MS: 2600},
			{Name: "cow.compare", Count: 1200, MS: 6100},
		},
	}}}
}

// wx bench は貸出から Early Ready・Full Ready を測り、daemon の区間内訳を添えて貸出を返す。
func TestRunBenchMeasuresBothReadinessGatesAndReleasesTheLease(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 1, Reuse: true, JSON: false}); exit != 0 {
			t.Fatalf("RunBench exit=%d", exit)
		}
	})
	for _, required := range []string{"EARLY READY", "FULL READY", "prepare job", "checkout", "cow.compare"} {
		if !strings.Contains(stdout, required) {
			t.Fatalf("stdout=%q missing %s", stdout, required)
		}
	}
	// 並列 worker の合計である下位区間は、上位区間より深く字下げして区別できるようにする。
	if !strings.Contains(stdout, "      cow.compare") {
		t.Fatalf("stdout=%q, want the sub-phase indented under its phase", stdout)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	for _, required := range []string{"WaitEarlyReady", "WaitReady", "PrepareTimings", "ReleaseLease"} {
		if !strings.Contains(methods, required) {
			t.Fatalf("methods=%s missing %s", methods, required)
		}
	}
	// standby を残す指定では退役要求を出さない。
	if strings.Contains(methods, "RetireStandby") {
		t.Fatalf("methods=%s, want no retire request with --reuse", methods)
	}
}

// --json は run ごとの測定と区間内訳をそのまま機械可読で出す。
func TestRunBenchJSONCarriesThePhaseBreakdown(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 1, Reuse: true, JSON: true}); exit != 0 {
			t.Fatalf("RunBench --json exit=%d", exit)
		}
	})
	var reply struct {
		Workspace string     `json:"workspace"`
		Runs      []BenchRun `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if len(reply.Runs) != 1 || reply.Runs[0].Source != "cold" {
		t.Fatalf("runs=%+v, want one cold run", reply.Runs)
	}
	measurement := reply.Runs[0].Measurement
	if measurement == nil || len(measurement.Phases) != 3 || measurement.TotalMS != 4300 {
		t.Fatalf("measurement=%+v, want the daemon breakdown", measurement)
	}
}

// 準備済み slot をそのまま受け取った回は warm として報告し、準備の待機を測らない。
func TestRunBenchReportsAWarmLeaseWithoutWaiting(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 1, Reuse: true, JSON: false}); exit != 0 {
			t.Fatalf("RunBench exit=%d", exit)
		}
	})
	if !strings.Contains(stdout, "warm") {
		t.Fatalf("stdout=%q, want the warm source reported", stdout)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if strings.Contains(methods, "WaitEarlyReady") || strings.Contains(methods, "WaitReady") {
		t.Fatalf("methods=%s, want no readiness wait for a prepared slot", methods)
	}
}

// 準備が失敗した回も失敗として終え、測った区間と貸出の返却は残す。
func TestRunBenchReportsAFailedPreparationAndStillReleases(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.waitReadyErr = errors.New("preparation failed")
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 1, Reuse: true, JSON: false}); exit != 1 {
			t.Fatalf("RunBench exit=%d, want 1 for a failed preparation", exit)
		}
	})
	if !strings.Contains(stdout, "preparation failed") {
		t.Fatalf("stdout=%q, want the failure reported", stdout)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if !strings.Contains(methods, "ReleaseLease") {
		t.Fatalf("methods=%s, want the lease returned after a failure", methods)
	}
}

// 既定の cold start 測定は、貸出の前に対象 workspace の standby を退役させる。
func TestRunBenchRetiresStandbyBeforeMeasuringAColdStart(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 1, Reuse: false, JSON: false}); exit != 0 {
			t.Fatalf("RunBench exit=%d", exit)
		}
	})
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if !strings.Contains(methods, "RetireStandby,ResolveAndLease") {
		t.Fatalf("methods=%s, want the standby retired before the lease", methods)
	}
}

// 回数の指定誤りは貸出を取る前に引数エラーで終える。
func TestRunBenchRejectsAnEmptyRunCount(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	stderr := captureStderrForLease(t, func() {
		if exit := client.RunBench(ctx, BenchOptions{Runs: 0, Reuse: true, JSON: false}); exit != 2 {
			t.Fatalf("RunBench --runs 0 exit=%d, want 2", exit)
		}
	})
	if !strings.Contains(stderr, "--runs") {
		t.Fatalf("stderr=%q, want the --runs guidance", stderr)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if methods != "" {
		t.Fatalf("methods=%s, want no request for an argument error", methods)
	}
}

// benchSlotUsageReply は測り終えた slot の使用量として daemon が返す行を組む。
// 測定時刻は run の途中に置く。bench は貸出要求より前の測定を前の準備の値として採らないためである。
func benchSlotUsageReply() []daemon.SlotView {
	return []daemon.SlotView{{
		SlotSummary:    state.SlotSummary{SlotID: "session", SessionID: "session", State: "LEASED"},
		Measurement:    "log2phys_first_last",
		CopyMode:       config.CopyModeCOW,
		AllocatedBytes: 1291 << 20, SharedBytes: 1116 << 20, ExclusiveBytes: 175 << 20,
		MeasuredAt: state.FormatTime(time.Now().Add(time.Minute)),
	}}
}

// --config を並べた測定は、設定ごとに貸出要求へ上書きを載せ、設定ごとの比較行を出す。
// 上書きは要求に付随するだけなので、設定の再読込を daemon へ求めない。
func TestRunBenchMeasuresEachConfigurationAndCarriesTheOverrideInTheRequest(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	handler.slots = benchSlotUsageReply()
	minSize := 16
	options := BenchOptions{Runs: 1, Configs: []config.PrepareOverride{
		{COWMinSizeKiB: &minSize}, {CopyMode: config.CopyModeCopy},
	}}
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, options); exit != 0 {
			t.Fatalf("RunBench exit=%d", exit)
		}
	})
	for _, required := range []string{"cow_min_size_kib=16", "copy_mode=copy", "175.00 MiB", "comparison by configuration"} {
		if !strings.Contains(stdout, required) {
			t.Fatalf("stdout=%q missing %s", stdout, required)
		}
	}
	requests := leaseRequests(t, handler)
	if len(requests) != 2 {
		t.Fatalf("lease requests=%d, want one per configuration", len(requests))
	}
	if requests[0].PrepareCOWMinSizeKiB == nil || *requests[0].PrepareCOWMinSizeKiB != minSize || requests[0].PrepareCopyMode != "" {
		t.Fatalf("first request=%+v, want only the lower bound overridden", requests[0])
	}
	if requests[1].PrepareCopyMode != config.CopyModeCopy || requests[1].PrepareCOWMinSizeKiB != nil {
		t.Fatalf("second request=%+v, want only the copy mode overridden", requests[1])
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if strings.Contains(methods, "ReloadConfig") {
		t.Fatalf("methods=%s, want no configuration reload for a measurement", methods)
	}
}

// --json は各 run の適用設定と使用量、設定ごとの集計を持つ。
func TestRunBenchJSONCarriesTheConfigurationOfEachRun(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	handler.slots = benchSlotUsageReply()
	options := BenchOptions{Runs: 1, JSON: true, Configs: []config.PrepareOverride{{CopyMode: config.CopyModeCopy}}}
	stdout := captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, options); exit != 0 {
			t.Fatalf("RunBench --json exit=%d", exit)
		}
	})
	var reply struct {
		Runs    []BenchRun           `json:"runs"`
		Configs []BenchConfigSummary `json:"configs"`
	}
	if err := json.Unmarshal([]byte(stdout), &reply); err != nil {
		t.Fatalf("stdout=%q err=%v", stdout, err)
	}
	if len(reply.Runs) != 1 || reply.Runs[0].Config.CopyMode != config.CopyModeCopy || reply.Runs[0].Config.Label != "copy_mode=copy" {
		t.Fatalf("runs=%+v, want the applied configuration on the run", reply.Runs)
	}
	if reply.Runs[0].Usage == nil || reply.Runs[0].Usage.ExclusiveBytes != 175<<20 {
		t.Fatalf("usage=%+v, want the slot usage measured before the release", reply.Runs[0].Usage)
	}
	if len(reply.Configs) != 1 || reply.Configs[0].ExclusiveBytes == nil || *reply.Configs[0].ExclusiveBytes != 175<<20 {
		t.Fatalf("configs=%+v, want the per-configuration aggregate", reply.Configs)
	}
}

// 設定を振る測定は cold start を前提とするため、--reuse との併用は測定を始める前に断る。
func TestRunBenchRejectsConfigurationsTogetherWithReuse(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	stderr := captureStderrForLease(t, func() {
		options := BenchOptions{Runs: 1, Reuse: true, Configs: []config.PrepareOverride{{CopyMode: config.CopyModeCopy}}}
		if exit := client.RunBench(ctx, options); exit != 2 {
			t.Fatalf("RunBench --config --reuse exit=%d, want 2", exit)
		}
	})
	if !strings.Contains(stderr, "--reuse") {
		t.Fatalf("stderr=%q, want the conflict reported", stderr)
	}
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if methods != "" {
		t.Fatalf("methods=%s, want no request for an argument error", methods)
	}
}

// 測定用の設定は要求に付随するだけなので、完走しても中断しても設定ファイルは変わらない。
func TestRunBenchLeavesTheConfigurationFileUntouched(t *testing.T) {
	client, handler, base, ctx := leaseFixture(t)
	handler.lease.Ready = false
	handler.prepareTimings = benchMeasurement()
	handler.slots = benchSlotUsageReply()
	configPath := filepath.Join(base, "config.yaml")
	original := []byte("version: 1\nstorage:\n  cow_min_size_kib: 16\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	options := BenchOptions{Runs: 1, Configs: []config.PrepareOverride{{CopyMode: config.CopyModeCopy}}}
	captureLeaseStdout(t, func() {
		if exit := client.RunBench(ctx, options); exit != 0 {
			t.Fatalf("RunBench exit=%d", exit)
		}
	})
	assertConfigFileUnchanged(t, configPath, original)
	// 中断した測定でも設定は残らない。貸出の後で context を切って途中終了を再現する。
	interrupted, cancel := context.WithCancel(ctx)
	go func() {
		waitUntilLeaseRequested(handler)
		cancel()
	}()
	captureLeaseStdout(t, func() { client.RunBench(interrupted, options) })
	assertConfigFileUnchanged(t, configPath, original)
	handler.mu.Lock()
	methods := strings.Join(handler.methods, ",")
	handler.mu.Unlock()
	if strings.Contains(methods, "ReloadConfig") {
		t.Fatalf("methods=%s, want no configuration reload for a measurement", methods)
	}
}

func assertConfigFileUnchanged(t *testing.T, path string, original []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("configuration file changed to %q, want %q", after, original)
	}
}

// waitUntilLeaseRequested は最初の貸出要求が届くのを待つ。中断を準備の途中に落とすためである。
func waitUntilLeaseRequested(handler *launcherHandler) {
	for range 200 {
		handler.mu.Lock()
		requested := len(handler.leaseParamsAll) > 0
		handler.mu.Unlock()
		if requested {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// leaseRequests は記録された ResolveAndLease の Params を要求順に返す。
func leaseRequests(t *testing.T, handler *launcherHandler) []rpc.ResolveAndLeaseParams {
	t.Helper()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	out := make([]rpc.ResolveAndLeaseParams, 0, len(handler.leaseParamsAll))
	for _, raw := range handler.leaseParamsAll {
		var params rpc.ResolveAndLeaseParams
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Fatalf("lease params=%s err=%v", raw, err)
		}
		out = append(out, params)
	}
	return out
}

// 前の準備で測った使用量は、時刻で見分けて今回の結果として採らない。
func TestBenchUsageMeasuredAfterRejectsTheMeasurementOfAnEarlierPreparation(t *testing.T) {
	leaseAt := time.Now()
	if benchUsageMeasuredAfter(state.FormatTime(leaseAt.Add(-time.Second)), leaseAt) {
		t.Fatal("accepted a measurement taken before the lease request")
	}
	if !benchUsageMeasuredAfter(state.FormatTime(leaseAt.Add(time.Second)), leaseAt) {
		t.Fatal("rejected a measurement taken during the run")
	}
	// 測定がまだ無い行と読めない時刻は、待ち続けて次の測定を待つ。
	if benchUsageMeasuredAfter("", leaseAt) || benchUsageMeasuredAfter("not a timestamp", leaseAt) {
		t.Fatal("accepted a row without a usable measurement time")
	}
}
