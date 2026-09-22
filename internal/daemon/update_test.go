package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/update"
)

// newUpdateManager は更新確認だけを扱う部分初期化の Manager を組む。
// probe は必ず注入し、実ネットワークへ出る既定の実装をテストから外す。
func newUpdateManager(t *testing.T, probe *updateProbe) (*Manager, *state.Store) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = t.TempDir()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := testManager(t, cfg, store)
	manager.updateProbe = probe
	return manager, store
}

// releaseProbe は配布用ビルドで version を名乗る probe を返す。
func releaseProbe(version string, latest func(context.Context) (update.Release, error)) *updateProbe {
	return &updateProbe{
		releaseBuild: func() bool { return true },
		current:      func() string { return version },
		latest:       latest,
	}
}

// TestUpdateCheckIsSkippedForDevelopmentBuildsAndWhenDisabled は、確認を行わない 2 つの条件を守る。
// どちらかで問い合わせてしまうと、開発ビルドと自動確認を切った利用者が GitHub へ接続することになる。
func TestUpdateCheckIsSkippedForDevelopmentBuildsAndWhenDisabled(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		release bool
		enabled bool
	}{
		{name: "development build", release: false, enabled: true},
		{name: "auto check disabled", release: true, enabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			asked := false
			probe := releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
				asked = true
				return update.Release{Tag: "v1.1.0"}, nil
			})
			probe.releaseBuild = func() bool { return test.release }
			manager, store := newUpdateManager(t, probe)
			enabled := test.enabled
			manager.cfg.System.Update.AutoCheck = &enabled
			manager.maybeCheckUpdate(context.Background())
			if asked {
				t.Fatal("the daemon asked GitHub although the check is disabled")
			}
			record, err := store.UpdateCheck(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !record.CheckedAt.IsZero() {
				t.Fatalf("a skipped check recorded %+v", record)
			}
			status, err := manager.UpdateState(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if status.Announce {
				t.Fatalf("status=%+v, want no announcement while the check is off", status)
			}
		})
	}
}

// TestUpdateCheckThinsOutRepeatedPasses は、保守の一巡ごとに問い合わせないことを守る。
// 間引かないと、既定の一巡ごとに GitHub を叩き、未認証のレート制限へ触れる。
func TestUpdateCheckThinsOutRepeatedPasses(t *testing.T) {
	t.Parallel()
	calls := 0
	manager, _ := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		calls++
		return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
	}))
	ctx := context.Background()
	manager.maybeCheckUpdate(ctx)
	manager.maybeCheckUpdate(ctx)
	manager.maybeCheckUpdate(ctx)
	if calls != 1 {
		t.Fatalf("checks=%d, want the later passes thinned out", calls)
	}
}

// TestFailedUpdateCheckStillAdvancesTheDeadline は、失敗も「確認した」とみなすことを守る。
// 進めないと、オフラインが続く間は一巡ごとに問い合わせ続ける。
func TestFailedUpdateCheckStillAdvancesTheDeadline(t *testing.T) {
	t.Parallel()
	calls := 0
	manager, store := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		calls++
		return update.Release{}, errors.New("dial tcp: no route to host")
	}))
	ctx := context.Background()
	manager.maybeCheckUpdate(ctx)
	manager.maybeCheckUpdate(ctx)
	if calls != 1 {
		t.Fatalf("checks=%d, want the failure to count as a check", calls)
	}
	record, err := store.UpdateCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if record.LastError == "" || record.CheckedAt.IsZero() {
		t.Fatalf("record=%+v, want the error kept and the deadline advanced", record)
	}
	status, err := manager.UpdateState(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if status.Available || status.LatestVersion != "" {
		t.Fatalf("status=%+v, want no update reported after a failed check", status)
	}
}

// TestUpdateStateHandsTheAnnouncementToOneCallerOnly は、案内権を要求した呼び出しだけが、
// その版について 1 回だけ受け取ることを守る。要求しない読み取りが権利を消費すると案内が消える。
func TestUpdateStateHandsTheAnnouncementToOneCallerOnly(t *testing.T) {
	t.Parallel()
	manager, _ := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
	}))
	ctx := context.Background()
	manager.maybeCheckUpdate(ctx)
	reader, err := manager.UpdateState(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Available || reader.Announce || reader.LatestVersion != "v1.1.0" || reader.ReleaseURL == "" {
		t.Fatalf("reader status=%+v, want an available update without consuming the announcement", reader)
	}
	first, err := manager.UpdateState(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Announce {
		t.Fatalf("first claim=%+v, want the announcement", first)
	}
	second, err := manager.UpdateState(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if second.Announce {
		t.Fatal("the same version was announced twice")
	}
}

// TestUpdateCheckIntervalStaysAboveTheRateLimitWindow は、間引きの刻みが 1 時間あたり数回を超えないことを守る。
// 未認証の GitHub API は IP あたり 1 時間 60 要求で、常駐が短い刻みで問い合わせると他の用途も巻き込んで枯れる。
func TestUpdateCheckIntervalStaysAboveTheRateLimitWindow(t *testing.T) {
	t.Parallel()
	if updateCheckInterval < time.Hour {
		t.Fatalf("update check interval=%s, want at least an hour", updateCheckInterval)
	}
}

// 直前までは新鮮だが、間隔ちょうどへ達した確認は次の問い合わせを許す。
func TestUpdateCheckFreshnessExpiresAtTheExactInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	if !updateCheckIsFresh(now.Add(-updateCheckInterval+time.Nanosecond), now) {
		t.Fatal("a check immediately before the deadline was treated as expired")
	}
	if updateCheckIsFresh(now.Add(-updateCheckInterval), now) {
		t.Fatal("a check at the deadline was treated as fresh")
	}
}

// TestUpdateStatusRPCPassesTheClaimFlagThrough は、RPC の decode と dispatch が案内権の要求を
// そのまま Manager へ渡すことを守る。false で呼んだ読み取りが権利を消費すると案内が消える。
func TestUpdateStatusRPCPassesTheClaimFlagThrough(t *testing.T) {
	t.Parallel()
	manager, _ := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
	}))
	ctx := context.Background()
	manager.maybeCheckUpdate(ctx)
	handler := Handler{Manager: manager}
	reply, err := handler.Handle(ctx, "UpdateStatus", json.RawMessage(`{"claim_announcement":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := reply.(UpdateStatus); !ok || status.Announce || !status.Available {
		t.Fatalf("reply=%+v, want an available update with the announcement left alone", reply)
	}
	reply, err = handler.Handle(ctx, "UpdateStatus", json.RawMessage(`{"claim_announcement":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if status, ok := reply.(UpdateStatus); !ok || !status.Announce {
		t.Fatalf("reply=%+v, want the announcement claimed", reply)
	}
}

// TestDisablingTheAutomaticApplyKeepsTheAnnouncement は、auto_apply を切っても新版のお知らせが
// 出続けることを守る。UpdateState が auto_apply を読み始めると、自動導入を断った利用者から
// 手で更新する手掛かりまで消える。
func TestDisablingTheAutomaticApplyKeepsTheAnnouncement(t *testing.T) {
	t.Parallel()
	manager, _ := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{Tag: "v1.1.0", URL: "https://example.test/v1.1.0"}, nil
	}))
	disabled := false
	manager.cfg.System.Update.AutoApply = &disabled
	ctx := context.Background()
	manager.maybeCheckUpdate(ctx)
	status, err := manager.UpdateState(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Available {
		t.Fatalf("status=%+v, want the dashboard to keep seeing the update", status)
	}
	claimed, err := manager.UpdateState(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.Announce {
		t.Fatalf("status=%+v, want the interactive launch to still announce it", claimed)
	}
}
