package cli

import (
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

// 貸出応答に readiness が載っていればそれを使い、空なら client の global 設定へ落ちる。
// fallback をこの2関数へ閉じ込めているので、待機側の各所で client と daemon の設定がずれない。
func TestLeaseReadinessPrefersTheLeaseResponse(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Readiness.Mode = "early"
	full := daemon.Lease{ReadinessMode: "full", ReadinessTimeoutMS: int((20 * time.Minute).Milliseconds())}
	if got := leaseReadinessMode(cfg, full); got != "full" {
		t.Fatalf("mode=%q, want the leased value", got)
	}
	if got := leaseReadinessTimeout(cfg, full); got != 20*time.Minute {
		t.Fatalf("timeout=%s, want the leased value", got)
	}
	var empty daemon.Lease
	if got := leaseReadinessMode(cfg, empty); got != "early" {
		t.Fatalf("mode=%q, want the global fallback", got)
	}
	if got := leaseReadinessTimeout(cfg, empty); got != cfg.Readiness.Timeout.Duration {
		t.Fatalf("timeout=%s, want the global fallback", got)
	}
}

// 起動条件の優先順位を固定する。特に early 設定でも hook が無ければ full へ後退し、
// 再開や貸出コマンドでは hook の有無にかかわらず意図した full 待機になる。
func TestReadinessForLeaseSelectsTheEffectiveGate(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	tests := []struct {
		name                     string
		lease                    daemon.Lease
		resuming                 bool
		leaseKind                string
		hooksReady               bool
		initialSetup             bool
		mode, reason, waitMethod string
	}{
		{name: "ready standby", lease: daemon.Lease{Ready: true}, hooksReady: false, mode: readinessReady},
		{name: "resume wins", resuming: true, leaseKind: "wx-shell", hooksReady: true, mode: readinessFull, reason: readinessReasonResume, waitMethod: "WaitReady"},
		{name: "lease wins", leaseKind: "wx-run", hooksReady: false, mode: readinessFull, reason: readinessReasonLease, waitMethod: "WaitReady"},
		{name: "initial setup wins over configured early", hooksReady: true, initialSetup: true, mode: readinessFull, reason: readinessReasonInitialSetup, waitMethod: "WaitReady"},
		{name: "resume wins over initial setup", resuming: true, hooksReady: true, initialSetup: true, mode: readinessFull, reason: readinessReasonResume, waitMethod: "WaitReady"},
		{name: "lease wins over initial setup", leaseKind: "wx-run", hooksReady: true, initialSetup: true, mode: readinessFull, reason: readinessReasonLease, waitMethod: "WaitReady"},
		{name: "configured full", lease: daemon.Lease{ReadinessMode: readinessFull}, hooksReady: true, mode: readinessFull, reason: readinessReasonConfiguredFull, waitMethod: "WaitReady"},
		{name: "missing hooks", lease: daemon.Lease{ReadinessMode: readinessEarly}, hooksReady: false, mode: readinessFull, reason: readinessReasonHooksUnavailable, waitMethod: "WaitReady"},
		{name: "early hooks", lease: daemon.Lease{ReadinessMode: readinessEarly}, hooksReady: true, mode: readinessEarly, waitMethod: "WaitEarlyReady"},
		{name: "global fallback", hooksReady: true, mode: readinessEarly, waitMethod: "WaitEarlyReady"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := readinessForLease(cfg, test.lease, test.resuming, test.leaseKind, test.hooksReady, test.initialSetup)
			if decision.Mode != test.mode || decision.Reason != test.reason || decision.WaitMethod != test.waitMethod {
				t.Fatalf("decision=%+v, want mode=%q reason=%q method=%q", decision, test.mode, test.reason, test.waitMethod)
			}
		})
	}
}
