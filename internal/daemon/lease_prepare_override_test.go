package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// prepareOverrideCOWMinSizeKiB は既定（16）と異なる下限を上書きに使う。
const prepareOverrideCOWMinSizeKiB = 64

// 貸出要求の準備設定の上書きは、準備 job が使う設定と保存する fingerprint の両方へ届く。
// 片方だけでは、既定設定の fingerprint を持つ slot が別設定で準備され、後の貸出で再利用される。
func TestPrepareOverrideReachesPreparerConfigAndFingerprint(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	m := f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	minSize := prepareOverrideCOWMinSizeKiB
	override := config.PrepareOverride{CopyMode: config.CopyModeCopy, COWMinSizeKiB: &minSize}
	lease, err := m.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), leaseAttrs{Prepare: override})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	slot, err := f.Store.Slot(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := config.DecodePrepareOverride(slot.PrepareOverride)
	if err != nil || stored.CopyMode != config.CopyModeCopy || stored.COWMinSizeKiB == nil || *stored.COWMinSizeKiB != minSize {
		t.Fatalf("slot override=%q decoded=%+v err=%v", slot.PrepareOverride, stored, err)
	}
	// 準備 job は貸出要求とは別のタイミングで走るため、Preparer の設定は slot の記録から組み立たなければならない。
	prepareConfig, err := m.slotPrepareConfig(slot)
	if err != nil || prepareConfig.Storage.CopyMode != config.CopyModeCopy || prepareConfig.Storage.COWMinSizeKiB != minSize {
		t.Fatalf("prepare config=%+v err=%v", prepareConfig.Storage, err)
	}
	// 上書きは daemon の実効設定を変えない。変えると並走する他 workspace の準備まで巻き込む。
	if effective := m.Config().Storage; effective.CopyMode != config.CopyModeAuto || effective.COWMinSizeKiB != config.DefaultCOWMinSizeKiB {
		t.Fatalf("effective storage config=%+v, want the defaults left untouched", effective)
	}
	assertPrepareOverrideFingerprint(ctx, t, f, slot, repo, override)
}

// assertPrepareOverrideFingerprint は保存済み fingerprint が上書き後の設定で計算されたことを確かめる。
func assertPrepareOverrideFingerprint(ctx context.Context, t *testing.T, f *managerFixture, slot state.Slot, repo string, override config.PrepareOverride) {
	t.Helper()
	m := f.Manager
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := f.Store.SlotRepositories(ctx, slot.ID)
	if err != nil || len(repositories) != 1 {
		t.Fatalf("slot repositories=%+v err=%v", repositories, err)
	}
	overridden, err := workspace.Fingerprint(slot.Generation, repositories[0].BaseOID, w.Repositories[0], override.Apply(m.Config()))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := workspace.Fingerprint(slot.Generation, repositories[0].BaseOID, w.Repositories[0], m.Config())
	if err != nil {
		t.Fatal(err)
	}
	if overridden == plain {
		t.Fatal("the override must change the fingerprint; otherwise the slot is reused by a lease without it")
	}
	if repositories[0].Fingerprint != overridden {
		t.Fatalf("stored fingerprint=%s, want the one computed with the override %s", repositories[0].Fingerprint, overridden)
	}
}

// 上書きで準備した slot は、上書きのない後続の貸出で再利用候補にならない。
// 対照として、同じ検査が上書きなしで準備した slot は候補として通すことも確かめる。
func TestSlotPreparedWithPrepareOverrideIsNotAReuseCandidate(t *testing.T) {
	t.Parallel()
	requireDaemonIntegration(t)
	f := runningManagerFixture(t, func(s *managerFixtureSetup) {
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	})
	m := f.Manager
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	plainID := leaseAndWaitSlot(ctx, t, f, repo, leaseAttrs{})
	minSize := prepareOverrideCOWMinSizeKiB
	override := config.PrepareOverride{CopyMode: config.CopyModeCopy, COWMinSizeKiB: &minSize}
	measuredID := leaseAndWaitSlot(ctx, t, f, repo, leaseAttrs{Prepare: override})
	plainSlot := reuseCandidateSlot(ctx, t, f, plainID)
	measuredSlot := reuseCandidateSlot(ctx, t, f, measuredID)
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := m.readyMatches(ctx, plainSlot, resolved); err != nil || !matched {
		t.Fatalf("plain slot matched=%v err=%v, want it accepted as a reuse candidate", matched, err)
	}
	if matched, err := m.readyMatches(ctx, measuredSlot, resolved); err != nil || matched {
		t.Fatalf("measured slot matched=%v err=%v, want it rejected as a reuse candidate", matched, err)
	}
}

// leaseAndWaitSlot は1回貸し出して FULL READY まで待ち、その slot の ID を返す。
func leaseAndWaitSlot(ctx context.Context, t *testing.T, f *managerFixture, repo string, attrs leaseAttrs) string {
	t.Helper()
	lease, err := f.Manager.ResolveAndLease(ctx, repo, nil, "codex", os.Getpid(), attrs)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, f.Manager, 30*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	return lease.SessionID
}

// reuseCandidateSlot は測り終えた slot を READY へ戻し、再利用判定にかけられる行を返す。
// 貸出中の slot は状態だけで判定が止まり、fingerprint による受け入れ・拒否を確かめられない。
func reuseCandidateSlot(ctx context.Context, t *testing.T, f *managerFixture, id string) state.Slot {
	t.Helper()
	if err := f.Store.SetSlotState(ctx, id, []string{"LEASED"}, "READY", ""); err != nil {
		t.Fatal(err)
	}
	slot, err := f.Store.Slot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return slot
}
