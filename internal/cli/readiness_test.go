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
