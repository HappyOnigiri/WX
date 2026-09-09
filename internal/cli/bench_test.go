package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/daemon"
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
		if exit := client.RunBench(ctx, 1, nil, true, false); exit != 0 {
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
		if exit := client.RunBench(ctx, 1, nil, true, true); exit != 0 {
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
		if exit := client.RunBench(ctx, 1, nil, true, false); exit != 0 {
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
		if exit := client.RunBench(ctx, 1, nil, true, false); exit != 1 {
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
		if exit := client.RunBench(ctx, 1, nil, false, false); exit != 0 {
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
		if exit := client.RunBench(ctx, 0, nil, true, false); exit != 2 {
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
