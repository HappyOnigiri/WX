package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/testsupport"
)

func TestBenchMutationBoundariesPreserveOutputContracts(t *testing.T) {
	t.Run("runs are numbered from one and wait between measurements", func(t *testing.T) {
		client, handler, _, ctx := leaseFixture(t)
		minSize := 16
		stdout := captureLeaseStdout(t, func() {
			if got := client.RunBench(ctx, BenchOptions{Runs: 1, Configs: []config.PrepareOverride{
				{CopyMode: config.CopyModeCopy}, {COWMinSizeKiB: &minSize},
			}}); got != 0 {
				t.Fatalf("RunBench exit=%d", got)
			}
		})
		if !strings.Contains(stdout, "run 1/2") || !strings.Contains(stdout, "run 2/2") {
			t.Fatalf("stdout=%q, want one-based run numbers", stdout)
		}
		handler.mu.Lock()
		statusCalls := 0
		for _, method := range handler.methods {
			if method == "Status" {
				statusCalls++
			}
		}
		handler.mu.Unlock()
		if statusCalls != 1 {
			t.Fatalf("Status calls=%d, want exactly one wait between runs", statusCalls)
		}
	})

	t.Run("exact label width is not padded", func(t *testing.T) {
		for _, label := range []string{"1234567890123", ""} {
			got := padBenchLabel(label)
			if label == "1234567890123" && got != label {
				t.Fatalf("padBenchLabel(%q)=%q", label, got)
			}
		}
		if got := padBenchLabel("short"); len(got) != benchSummaryLabelWidth {
			t.Fatalf("padBenchLabel(short)=%q (width %d), want width %d", got, len(got), benchSummaryLabelWidth)
		}
	})

	t.Run("retirement is shown only for cold runs", func(t *testing.T) {
		stdout := captureLeaseStdout(t, func() {
			printBenchRunLanguage(1, 1, BenchRun{Source: "warm", RetiredStandby: 1}, i18n.English)
		})
		if strings.Contains(stdout, "retired") {
			t.Fatalf("stdout=%q, warm run must not report retired standby", stdout)
		}
	})

	t.Run("zero-count phase keeps its measured zero", func(t *testing.T) {
		stdout := captureLeaseStdout(t, func() {
			printBenchRunLanguage(1, 1, BenchRun{
				Source: "cold", Config: BenchConfig{},
				Measurement: &daemon.PrepareMeasurement{Phases: []daemon.PreparePhase{{Name: "checkout", Count: 1}}},
			}, i18n.English)
		})
		if !strings.Contains(stdout, "checkout") || !strings.Contains(stdout, "0.000s") || strings.Contains(stdout, "checkout                      -") {
			t.Fatalf("stdout=%q, want a zero-second single-count phase", stdout)
		}
	})
	t.Run("zero-time multi-count phase is shown as a count-only row", func(t *testing.T) {
		stdout := captureLeaseStdout(t, func() {
			printBenchRunLanguage(1, 1, BenchRun{Source: "cold", Measurement: &daemon.PrepareMeasurement{
				Phases: []daemon.PreparePhase{{Name: "checkout", Count: 2}},
			}}, i18n.English)
		})
		if !strings.Contains(stdout, "checkout") || !strings.Contains(stdout, "-") {
			t.Fatalf("stdout=%q, want count-only phase", stdout)
		}
	})

	t.Run("submodule details include only unavailable actions", func(t *testing.T) {
		stdout := captureLeaseStdout(t, func() {
			printBenchSubmodules(&daemon.PrepareSubmoduleReport{Details: []daemon.PrepareSubmoduleDetail{
				{Path: "vendor/skipped", Action: "skipped", Reason: "missing"},
				{Path: "vendor/unreachable", Action: "unreachable", Reason: "source"},
				{Path: "vendor/ready", Action: "materialized", Reason: ""},
			}}, i18n.New(string(i18n.English)))
		})
		for _, path := range []string{"vendor/skipped", "vendor/unreachable"} {
			if !strings.Contains(stdout, path) {
				t.Fatalf("stdout=%q, missing %s", stdout, path)
			}
		}
		if strings.Contains(stdout, "vendor/ready") {
			t.Fatalf("stdout=%q, materialized detail must stay hidden", stdout)
		}
	})
}

func TestBenchMutationBoundariesPreserveSummaryContracts(t *testing.T) {
	tests := []struct {
		name       string
		runs       []BenchRun
		wantOutput bool
		wantText   string
	}{
		{name: "one successful run", runs: []BenchRun{{EarlyReadyMS: 100, FullReadyMS: 200}}, wantOutput: true, wantText: "summary of 1"},
		{name: "only failed runs", runs: []BenchRun{{Error: "full ready: failed"}}, wantOutput: false},
		{name: "distribution has three labels", runs: []BenchRun{{EarlyReadyMS: 100, FullReadyMS: 200}, {EarlyReadyMS: 300, FullReadyMS: 400}}, wantOutput: true, wantText: "min"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout := captureLeaseStdout(t, func() { printBenchSummaryLanguage(tt.runs, i18n.English) })
			if (stdout != "") != tt.wantOutput {
				t.Fatalf("stdout=%q, want output=%t", stdout, tt.wantOutput)
			}
			if tt.wantText != "" && !strings.Contains(stdout, tt.wantText) {
				t.Fatalf("stdout=%q, missing %q", stdout, tt.wantText)
			}
		})
	}

	for _, tt := range []struct {
		name   string
		values []int64
		want   string
	}{
		{name: "single", values: []int64{1234}, want: "1.234s"},
		{name: "distribution", values: []int64{3000, 1000}, want: "min 1.000s  median 3.000s  max 3.000s"},
	} {
		t.Run("format "+tt.name, func(t *testing.T) {
			if got := formatBenchDistribution(tt.values); got != tt.want {
				t.Fatalf("formatBenchDistribution(%v)=%q, want %q", tt.values, got, tt.want)
			}
		})
	}
}

func TestBenchMutationBoundariesPreserveUsageAndRetirement(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.slots = benchSlotUsageReply()
	usage, err := client.benchSlotUsageOnce(ctx, "session", time.Now().Add(-time.Minute))
	if err != nil || usage == nil || usage.ExclusiveBytes != 175<<20 {
		t.Fatalf("usage=%+v err=%v, want the measured slot", usage, err)
	}
	missing, err := client.benchSlotUsageOnce(ctx, "missing", time.Now().Add(-time.Minute))
	if err != nil || missing != nil {
		t.Fatalf("missing usage=%+v err=%v, want nil without an RPC error", missing, err)
	}

	standbyClient, standby := newBenchMutationClient(t, func(method string) (any, error) {
		if method == "RetireStandby" {
			return map[string]any{"retired": []string{"slot-a", "slot-b"}}, nil
		}
		return nil, errors.New("unexpected method " + method)
	})
	retired, err := standbyClient.retireStandby(context.Background(), "/workspace")
	if err != nil || retired != 2 {
		t.Fatalf("retired=%d err=%v, want two retired standby slots", retired, err)
	}
	_ = standby
}

func TestBenchSlotUsageMutationBoundariesKeepAUsageResultAfterTheDeadline(t *testing.T) {
	client, handler, _, ctx := leaseFixture(t)
	handler.slots = benchSlotUsageReply()
	oldTimeout := benchUsageTimeout
	benchUsageTimeout = 0
	t.Cleanup(func() { benchUsageTimeout = oldTimeout })
	if got := client.benchSlotUsage(ctx, "session", time.Now().Add(-time.Minute)); got == nil {
		t.Fatal("benchSlotUsage returned nil for a measured slot even after the deadline")
	}
}

func TestBenchRunsMutationBoundariesWaitOnlyBetweenMeasuredRuns(t *testing.T) {
	client, handler, root, ctx := leaseFixture(t)
	_, _ = client.benchRuns(ctx, root, root, BenchOptions{Runs: 1, Configs: []config.PrepareOverride{{}}})
	handler.mu.Lock()
	methods := append([]string(nil), handler.methods...)
	handler.mu.Unlock()
	retire, status := -1, -1
	for i, method := range methods {
		if method == "RetireStandby" && retire < 0 {
			retire = i
		}
		if method == "Status" && status < 0 {
			status = i
		}
	}
	if status >= 0 && (retire < 0 || status < retire) {
		t.Fatalf("methods=%v, Status must not precede the first measured run", methods)
	}
}

// Slots の失敗は、次の測定を待たずにその回を使用量なしとして終える。
func TestBenchSlotUsageMutationBoundariesStopAfterRPCFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	client, _ := newBenchMutationClient(t, func(method string) (any, error) {
		if method != "Slots" {
			return nil, errors.New("unexpected method " + method)
		}
		calls++
		if calls == 2 {
			cancel()
		}
		return nil, errors.New("slots unavailable")
	})
	if got := client.benchSlotUsage(ctx, "session", time.Now()); got != nil {
		t.Fatalf("benchSlotUsage=%+v, want nil after an RPC failure", got)
	}
	if calls != 1 {
		t.Fatalf("Slots calls=%d, want one call before returning the RPC failure", calls)
	}
}

func TestWaitBenchIdleWaitsForBusyJobs(t *testing.T) {
	var calls int
	client, handler := newBenchMutationClient(t, func(method string) (any, error) {
		if method != "Status" {
			return nil, errors.New("unexpected method " + method)
		}
		calls++
		if calls == 1 {
			return map[string]any{"job_details": map[string]int{"pending": 1, "running": 0}}, nil
		}
		return map[string]any{"job_details": map[string]int{"pending": 0, "running": 0}}, nil
	})
	client.waitBenchIdle(context.Background())
	if calls != 2 {
		t.Fatalf("Status calls=%d, want a second check after the busy job clears", calls)
	}
	_ = handler
}

func TestBenchSweepConfigsKeepsThePublishedOrder(t *testing.T) {
	configs := BenchSweepConfigs()
	if len(configs) != len(benchSweepCOWMinSizeKiB)+1 {
		t.Fatalf("configs=%d, want copy baseline plus all sweep values", len(configs))
	}
	if configs[0].CopyMode != config.CopyModeCopy || configs[0].COWMinSizeKiB != nil {
		t.Fatalf("baseline=%+v", configs[0])
	}
	for index, want := range benchSweepCOWMinSizeKiB {
		if configs[index+1].COWMinSizeKiB == nil || *configs[index+1].COWMinSizeKiB != want {
			t.Fatalf("config[%d]=%+v, want COW lower bound %d", index+1, configs[index+1], want)
		}
	}
}

// benchMutationHandler は境界テストに必要な応答だけを返す小さな RPC fixture である。
type benchMutationHandler struct {
	reply func(string) (any, error)
}

func (h benchMutationHandler) Handle(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return h.reply(method)
}

func newBenchMutationClient(t *testing.T, reply func(string) (any, error)) (Client, *benchMutationHandler) {
	t.Helper()
	handler := &benchMutationHandler{reply: reply}
	socket := testsupport.SocketPath(t, "wxd.sock")
	ctx, cancel := context.WithCancel(context.Background())
	server := &rpc.Server{Socket: socket, Handler: handler}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	waitForSocket(t, socket, done)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("bench mutation RPC server: %v", err)
		}
	})
	return Client{RPC: rpc.Client{Socket: socket, Timeout: time.Second}, Config: config.Defaults()}, handler
}
