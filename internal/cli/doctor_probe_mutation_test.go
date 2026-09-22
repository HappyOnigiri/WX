package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestProbeSubmoduleMutationBoundariesKeepAnOKFindingForAnEmptyIndex(t *testing.T) {
	worktree := probeWorktreeFixture(t)
	findings := probeSubmoduleFindings(context.Background(), probeTestGit(), "/root", worktree)
	if len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
		t.Fatalf("findings=%+v, want one successful check for zero gitlinks", findings)
	}
	if !strings.Contains(strings.Join(findings[0].Details, "\n"), "0 submodule(s) checked") {
		t.Fatalf("details=%v, want the zero-count check", findings[0].Details)
	}
	if strings.Contains(strings.Join(findings[0].Details, "\n"), "outside the preparation range") {
		t.Fatalf("details=%v, zero submodules must not be reported out of scope", findings[0].Details)
	}
}

func TestPrepareFailureMutationBoundariesReportOnlyRecordedFailures(t *testing.T) {
	for _, tt := range []struct {
		name       string
		code       string
		detailPath string
		want       bool
		wantPath   string
	}{
		{name: "no failure", want: false},
		{name: "failure without detail", code: "PREPARE_FAILED", want: true, wantPath: "unavailable"},
		{name: "failure with detail", code: "PREPARE_FAILED", detailPath: "/tmp/detail.log", want: true, wantPath: "/tmp/detail.log"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			findings := prepareFailureFindings("/workspace", daemon.SlotView{Path: "/slot", PrepareFailureCode: tt.code, PrepareFailureDetailPath: tt.detailPath})
			if (len(findings) > 0) != tt.want {
				t.Fatalf("findings=%+v, want present=%t", findings, tt.want)
			}
			if tt.want && !strings.Contains(findings[0].Cause, tt.wantPath) {
				t.Fatalf("cause=%q, want detail path %q", findings[0].Cause, tt.wantPath)
			}
		})
	}
}

func TestProbeSlotViewMutationBoundariesReturnMeasuredSlot(t *testing.T) {
	measured := daemon.SlotView{SlotSummary: state.SlotSummary{SlotID: "slot", SessionID: "session"}, Measurement: "log2phys"}
	client, _ := newBenchMutationClient(t, func(method string) (any, error) {
		if method != "Slots" {
			return nil, errors.New("unexpected method " + method)
		}
		return []daemon.SlotView{measured}, nil
	})
	got := client.probeSlotView(context.Background(), "session")
	if got.Measurement != measured.Measurement || got.SessionID != measured.SessionID {
		t.Fatalf("slot=%+v, want measured slot %+v", got, measured)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	got = client.probeSlotView(ctx, "slot")
	if got.SlotID != measured.SlotID || got.Measurement != measured.Measurement {
		t.Fatalf("slot lookup by slot ID=%+v, want measured slot %+v", got, measured)
	}
}

// Slots の応答が短時間で返る場合、probeUsageTimeout の単位を壊す変異は
// context deadline を先に発生させ、測定済み slot を返せなくする。
func TestProbeSlotViewMutationBoundariesKeepTheRPCTimeoutInSeconds(t *testing.T) {
	measured := daemon.SlotView{SlotSummary: state.SlotSummary{SlotID: "slot", SessionID: "session"}, Measurement: "log2phys"}
	client, _ := newBenchMutationClient(t, func(method string) (any, error) {
		if method != "Slots" {
			return nil, errors.New("unexpected method " + method)
		}
		time.Sleep(5 * time.Millisecond)
		return []daemon.SlotView{measured}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if got := client.probeSlotView(ctx, "session"); got.Measurement != measured.Measurement {
		t.Fatalf("slot=%+v, want the delayed measured slot %+v", got, measured)
	}
}
