package cli

import (
	"time"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

// readiness の値は待機経路そのものを表す。`ready` は準備待ちをせず、
// `early` / `full` はそれぞれ Early Ready / Full Ready まで待つ。
const (
	readinessReady = "ready"
	readinessEarly = "early"
	readinessFull  = "full"
)

// readiness の full 待機が設定・起動条件のどれで選ばれたかを表す。
// hook 未整備だけが設定どおりでない後退なので、client の案内対象になる。
const (
	readinessReasonResume           = "resume"
	readinessReasonLease            = "lease"
	readinessReasonInitialSetup     = "initial-setup"
	readinessReasonConfiguredFull   = "configured-full"
	readinessReasonHooksUnavailable = "hooks-unavailable"
)

// readinessDecision は貸出後に client が実際に使う準備完了ゲートである。
// 同じ値を待機 RPC、進捗表示、hook 未整備の案内へ渡し、判定の二重定義を避ける。
type readinessDecision struct {
	Mode       string
	Reason     string
	WaitMethod string
}

// readinessForLease は貸出応答と起動条件から実効 readiness を決める純関数である。
// 優先順位は再開、agent 以外の貸出、full 設定、hook 未整備、early の順に固定する。
// lease.Ready のときは条件にかかわらず待機なし（ready）として扱う。
func readinessForLease(cfg config.Config, lease daemon.Lease, resuming bool, leaseKind string, hooksReady, initialSetup bool) readinessDecision {
	if lease.Ready {
		return readinessDecision{Mode: readinessReady}
	}
	if resuming {
		return readinessDecision{Mode: readinessFull, Reason: readinessReasonResume, WaitMethod: "WaitReady"}
	}
	if leaseKind != "" {
		return readinessDecision{Mode: readinessFull, Reason: readinessReasonLease, WaitMethod: "WaitReady"}
	}
	if initialSetup {
		return readinessDecision{Mode: readinessFull, Reason: readinessReasonInitialSetup, WaitMethod: "WaitReady"}
	}
	if leaseReadinessMode(cfg, lease) == readinessFull {
		return readinessDecision{Mode: readinessFull, Reason: readinessReasonConfiguredFull, WaitMethod: "WaitReady"}
	}
	if !hooksReady {
		return readinessDecision{Mode: readinessFull, Reason: readinessReasonHooksUnavailable, WaitMethod: "WaitReady"}
	}
	return readinessDecision{Mode: readinessEarly, WaitMethod: "WaitEarlyReady"}
}

// leaseReadinessMode は貸出応答に載った readiness mode を返す。
// daemon が slot 内の repository 個別指定を合成した値で、client は repository を知らないため自分では解決できない。
// 応答が空なのは古い daemon か repository を持たない貸出なので、client の global 設定へ落ちる。
func leaseReadinessMode(cfg config.Config, lease daemon.Lease) string {
	if lease.ReadinessMode != "" {
		return lease.ReadinessMode
	}
	return cfg.RepositoryDefaults.Readiness.Mode
}

// leaseReadinessTimeout は貸出応答に載った readiness timeout を返す。
// 空のときの global fallback は leaseReadinessMode と合わせてこの2関数だけに閉じ込め、
// client と daemon の設定がずれ得る窓口を散らさない。
func leaseReadinessTimeout(cfg config.Config, lease daemon.Lease) time.Duration {
	if lease.ReadinessTimeoutMS > 0 {
		return time.Duration(lease.ReadinessTimeoutMS) * time.Millisecond
	}
	return cfg.MaxReadinessTimeout()
}
