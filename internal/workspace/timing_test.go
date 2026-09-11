package workspace

import (
	"errors"
	"testing"
	"time"
)

// 区間は最初に現れた順で並び、同じ名前は回数と時間を足し合わせる。
func TestPhaseTimingsAggregatesInFirstAppearanceOrder(t *testing.T) {
	timings := &PhaseTimings{}
	timings.Add("checkout", 1, time.Second)
	timings.Add("cow", 1, 2*time.Second)
	timings.Add("checkout", 2, 3*time.Second)
	timings.Add("cow.compare", 100, 4*time.Second)
	phases := timings.Phases()
	if len(phases) != 3 {
		t.Fatalf("phases=%+v, want three distinct phases", phases)
	}
	if phases[0].Name != "checkout" || phases[0].Count != 3 || phases[0].Total != 4*time.Second {
		t.Fatalf("phases[0]=%+v, want the summed checkout phase first", phases[0])
	}
	if phases[1].Name != "cow" || phases[2].Name != "cow.compare" || phases[2].Count != 100 {
		t.Fatalf("phases=%+v, want cow before its sub-phase", phases)
	}
}

// 回数 0 の記録は区間を作らない。名前だけの区間が内訳へ現れると、測っていない区間と区別できなくなる。
func TestPhaseTimingsIgnoresEmptyObservations(t *testing.T) {
	timings := &PhaseTimings{}
	timings.Add("cow.shared", 0, time.Second)
	if phases := timings.Phases(); len(phases) != 0 {
		t.Fatalf("phases=%+v, want no phase for a zero count", phases)
	}
}

// 計測は準備結果を変えないため、器を持たない Preparer でも同じ経路が通る。
func TestPhaseTimingsIsOptional(t *testing.T) {
	var timings *PhaseTimings
	timings.Observe("checkout", time.Now())
	timings.Add("cow", 1, time.Second)
	if phases := timings.Phases(); phases != nil {
		t.Fatalf("phases=%+v, want nil without a recorder", phases)
	}
	preparer := &Preparer{}
	failure := errors.New("checkout failed")
	if err := preparer.timePhase("checkout", func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("timePhase err=%v, want the wrapped error", err)
	}
}

// 失敗した区間も記録する。どこで止まったかが遅さの調査に必要だからである。
func TestTimePhaseRecordsFailedRuns(t *testing.T) {
	preparer := &Preparer{Phases: &PhaseTimings{}}
	failure := errors.New("prepare command failed")
	if err := preparer.timePhase("prepare-command", func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("timePhase err=%v", err)
	}
	phases := preparer.Phases.Phases()
	if len(phases) != 1 || phases[0].Name != "prepare-command" || phases[0].Count != 1 {
		t.Fatalf("phases=%+v, want the failed phase recorded", phases)
	}
}

// 実行中の区間は入れ子の最も内側を返し、終わった区間は残らない。
func TestPhaseTimingsActiveReportsTheInnermostRunningPhase(t *testing.T) {
	preparer := &Preparer{Phases: &PhaseTimings{}}
	if _, _, ok := preparer.Phases.Active(); ok {
		t.Fatal("a phase was reported as running before any phase started")
	}
	err := preparer.timePhase("checkout", func() error {
		name, start, ok := preparer.Phases.Active()
		if !ok || name != "checkout" {
			t.Fatalf("active=%q ok=%t, want the outer phase", name, ok)
		}
		if start.IsZero() {
			t.Fatal("the running phase has no start time")
		}
		return preparer.timePhase("cow", func() error {
			if name, _, ok := preparer.Phases.Active(); !ok || name != "cow" {
				t.Fatalf("active=%q ok=%t, want the inner phase", name, ok)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatalf("timePhase err=%v", err)
	}
	if _, _, ok := preparer.Phases.Active(); ok {
		t.Fatal("a phase was still reported as running after every phase finished")
	}
}

// 完了区間を足す Add と混ざっても、実行中として返すのは timePhase が包んだ区間だけである。
func TestPhaseTimingsActiveIgnoresCompletedAggregates(t *testing.T) {
	preparer := &Preparer{Phases: &PhaseTimings{}}
	err := preparer.timePhase("cow", func() error {
		preparer.Phases.Add("cow.compare", 4, time.Second)
		name, _, ok := preparer.Phases.Active()
		if !ok || name != "cow" {
			t.Fatalf("active=%q ok=%t, want the running phase, not the aggregate", name, ok)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("timePhase err=%v", err)
	}
}

// 器を持たない Preparer でも Active は落ちず、実行中なしを返す。
func TestPhaseTimingsActiveIsOptional(t *testing.T) {
	var timings *PhaseTimings
	if _, _, ok := timings.Active(); ok {
		t.Fatal("a nil recorder reported a running phase")
	}
	preparer := &Preparer{}
	if err := preparer.timePhase("checkout", func() error { return nil }); err != nil {
		t.Fatalf("timePhase err=%v", err)
	}
}
