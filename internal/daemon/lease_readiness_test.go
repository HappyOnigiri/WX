package daemon

import (
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// slot 内の readiness は mode を full へ、timeout を最長へ寄せて合成する。
// early 側へ寄せると full を求めた repository が準備前に起動し、最短へ寄せると遅い repository が必ず timeout する。
func TestLeaseReadinessCombinesRepositoryOverrides(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.Readiness.Mode = "early"
	long := config.Duration{Duration: 20 * time.Minute}
	short := config.Duration{Duration: 5 * time.Minute}
	cfg.Repositories["/full"] = config.Repository{Readiness: config.RepositoryReadiness{Mode: "full", Timeout: &short}}
	cfg.Repositories["/slow"] = config.Repository{Readiness: config.RepositoryReadiness{Timeout: &long}}
	repositories := func(paths ...string) []discovery.Repository {
		out := make([]discovery.Repository, 0, len(paths))
		for _, path := range paths {
			out = append(out, discovery.Repository{MainPath: domain.CanonicalPath(path)})
		}
		return out
	}
	mode, timeout := leaseReadiness(cfg, repositories("/slow", "/full"))
	if mode != "full" || timeout != int(long.Milliseconds()) {
		t.Fatalf("mode=%q timeout=%dms, want full and the longest timeout", mode, timeout)
	}
	// 順序を変えても full が勝つ。
	if mode, _ := leaseReadiness(cfg, repositories("/full", "/slow")); mode != "full" {
		t.Fatalf("mode=%q, want full regardless of order", mode)
	}
	// 個別指定の無い repository だけなら global の値になる。
	mode, timeout = leaseReadiness(cfg, repositories("/plain"))
	if mode != "early" || timeout != int(cfg.Readiness.Timeout.Milliseconds()) {
		t.Fatalf("mode=%q timeout=%dms, want the global values", mode, timeout)
	}
	// repository を持たない貸出は空を返し、client 側の global fallback に任せる。
	if mode, timeout := leaseReadiness(cfg, nil); mode != "" || timeout != 0 {
		t.Fatalf("mode=%q timeout=%d, want an empty response", mode, timeout)
	}
}
