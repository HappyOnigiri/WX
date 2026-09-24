package daemon

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// 配置した差分が上限ちょうどなら、短縮表示へ切り替えず全件を返す。
func TestPlacementDriftIncludesExactlyThePathLimit(t *testing.T) {
	t.Parallel()

	planned := make([]state.Placement, 0, mismatchPathLimit)
	for i := range mismatchPathLimit {
		planned = append(planned, state.Placement{RelativePath: string(rune('a' + i)), Kind: "copy"})
	}
	got := placementDrift(nil, planned)
	if strings.Contains(got, "more") {
		t.Fatalf("drift=%q, want all paths without truncation", got)
	}
	if want := "+a +b +c +d +e"; got != want {
		t.Fatalf("drift=%q, want %q", got, want)
	}
}

// OID が短縮長と同じなら、その値をそのままログへ載せる。
func TestShortOIDPreservesExactlyTheShortLength(t *testing.T) {
	t.Parallel()

	want := "0123456789ab"
	if got := shortOID(want); got != want {
		t.Fatalf("shortOID=%q, want %q", got, want)
	}
}

// 再利用候補が無いときは、warm lease の転落ログを出さない。
func TestReusableStandbyDoesNotLogFallbackWithoutCandidates(t *testing.T) {
	f := newReuseStandbyFixtureWithWarmCount(t, 0, initGitRepo)
	logs := newDiagnosticLog(managerFixtureLogLimit)
	f.manager.log = slog.New(slog.NewTextHandler(logs, nil))

	if _, err := f.manager.ResolveAndLease(context.Background(), f.repository, nil, "codex", 0, leaseAttrs{}); err != nil {
		t.Fatal(err)
	}
	if got := logs.tail(); strings.Contains(got, "warm lease fell back to a cold start") {
		t.Fatalf("fallback log=%q, want no fallback for zero candidates", got)
	}
}

// 再利用を無効にした経路でも、候補0件を「試行済み」として記録しない。
func TestLeaseDoesNotLogFallbackWithoutReadyCandidates(t *testing.T) {
	f := newReuseStandbyFixtureWithWarmCount(t, 0, initGitRepo)
	f.manager.cfg.Worktree.ReuseStandby = false
	logs := newDiagnosticLog(managerFixtureLogLimit)
	f.manager.log = slog.New(slog.NewTextHandler(logs, nil))

	if _, err := f.manager.ResolveAndLease(context.Background(), f.repository, nil, "codex", 0, leaseAttrs{}); err != nil {
		t.Fatal(err)
	}
	if got := logs.tail(); strings.Contains(got, "warm lease fell back to a cold start") {
		t.Fatalf("fallback log=%q, want no fallback for zero READY candidates", got)
	}
}

// 検証に失敗する READY 候補を試した場合は、cold start への転落と試行数をログへ残す。
func TestLeaseFallbackLogCountsAnInvalidReadyCandidate(t *testing.T) {
	f := newReuseStandbyFixture(t)
	standby := f.readyStandby(t)
	if err := os.RemoveAll(standby.Path); err != nil {
		t.Fatal(err)
	}
	f.manager.cfg.Worktree.ReuseStandby = false
	// 準備直後の使用量測定は background で走るため、logger の差し替え前に完了を待つ。
	f.manager.backgroundWG.Wait()
	logs := newDiagnosticLog(managerFixtureLogLimit)
	f.manager.log = slog.New(slog.NewTextHandler(logs, nil))

	if _, err := f.manager.ResolveAndLease(context.Background(), f.repository, nil, "codex", 0, leaseAttrs{}); err != nil {
		t.Fatal(err)
	}
	got := logs.tail()
	if !strings.Contains(got, "warm lease fell back to a cold start") || !strings.Contains(got, "ready_candidates=1") || !strings.Contains(got, "attempts=1") {
		t.Fatalf("fallback log=%q, want one attempted READY candidate", got)
	}
}
