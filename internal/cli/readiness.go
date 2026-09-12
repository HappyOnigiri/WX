package cli

import (
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
)

// leaseReadinessMode は貸出応答に載った readiness mode を返す。
// daemon が slot 内の repository 個別指定を合成した値で、client は repository を知らないため自分では解決できない。
// 応答が空なのは古い daemon か repository を持たない貸出なので、client の global 設定へ落ちる。
func leaseReadinessMode(cfg config.Config, lease daemon.Lease) string {
	if lease.ReadinessMode != "" {
		return lease.ReadinessMode
	}
	return cfg.Readiness.Mode
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
