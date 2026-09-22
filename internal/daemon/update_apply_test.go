package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/update"
)

// updateApplySpy は注入した spawn が受け取った argv・env・log path を順に記録する。
type updateApplySpy struct {
	argv    [][]string
	env     [][]string
	logs    []string
	failure error
}

func (s *updateApplySpy) spawn(argv, env []string, logPath string) error {
	s.argv = append(s.argv, argv)
	s.env = append(s.env, env)
	s.logs = append(s.logs, logPath)
	return s.failure
}

// updateApplyFixture は自動適用だけを通す Manager を組み、新版が見えている状態にする。
// spawn と launchd 判定は必ず注入する。production の実装を通すと本当に自分を置き換えてしまう。
func updateApplyFixture(t *testing.T) (*Manager, *state.Store, *updateApplySpy) {
	t.Helper()
	manager, store := newUpdateManager(t, releaseProbe("v1.0.0", func(context.Context) (update.Release, error) {
		return update.Release{}, errors.New("the automatic apply must not ask GitHub")
	}))
	enabled := true
	manager.cfg.System.Update.AutoCheck = &enabled
	manager.cfg.System.Update.AutoApply = &enabled
	manager.launchdManaged = func() bool { return true }
	manager.executablePath = filepath.Join(t.TempDir(), "wx")
	spy := &updateApplySpy{}
	manager.updateApply = &updateApplyHooks{spawn: spy.spawn, logDir: t.TempDir()}
	if err := store.RecordUpdateCheck(context.Background(), "v1.1.0", "https://example.invalid/v1.1.0", ""); err != nil {
		t.Fatal(err)
	}
	return manager, store, spy
}

// leaseOneSlot は貸出中の slot を 1 件作り、state.Status().Leased を 1 にする。
func leaseOneSlot(t *testing.T, manager *Manager, store *state.Store) {
	t.Helper()
	ctx := context.Background()
	root := manager.cfg.Storage.WorktreeRoot
	workspace := registerTestWorkspace(t, store, discovery.Workspace{Root: domain.CanonicalPath(root), Kind: "repository"})
	slot := storeSlotAt(t, store, root, string(workspace.ID), "update-apply-slot", filepath.Join(root, "update-apply-slot"), 1, "PREPARING")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.FinishPreparationWithRelease(ctx, slot.ID); err != nil {
		t.Fatal(err)
	}
	session := state.Session{ID: "update-apply-session", WorkspaceID: string(workspace.ID), SlotID: slot.ID, State: "STARTING", AgentKind: "claude", TokenHash: state.HashToken("update-apply-session")}
	if err := store.LeaseReady(ctx, slot.ID, session); err != nil {
		t.Fatal(err)
	}
	// 準備と補充が積んだ job を捌き、貸出だけが自動適用を止めていることを確かめられる状態にする。
	jobs, err := store.RecoverJobs(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range jobs {
		if _, err := store.ClaimJob(ctx, job.ID, "update-apply-test"); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, job.ID, "update-apply-test", nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestAutomaticUpdateStaysOutOfTheWay は、自動適用を始めてはならない条件を固定する。
// ここを緩めると、利用者が作業している最中や、置換しても入れ替わらない daemon で適用が走る。
func TestAutomaticUpdateStaysOutOfTheWay(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, manager *Manager, store *state.Store)
	}{
		{name: "a slot is leased", setup: func(t *testing.T, manager *Manager, store *state.Store) {
			leaseOneSlot(t, manager, store)
		}},
		{name: "a job is queued", setup: func(t *testing.T, manager *Manager, store *state.Store) {
			if _, err := store.CreateJob(context.Background(), "ENSURE_STANDBY", "", "", ""); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a request is in flight", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.beginRequest(false)
		}},
		{name: "a restart is pending", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.restartPending = true
		}},
		{name: "a stop is pending", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.stopPending = true
		}},
		{name: "launchd does not manage this daemon", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.launchdManaged = func() bool { return false }
		}},
		{name: "the daemon executable is unknown", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.executablePath = ""
		}},
		{name: "this is a development build", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.updateProbe.releaseBuild = func() bool { return false }
		}},
		{name: "automatic apply is disabled", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			disabled := false
			manager.cfg.System.Update.AutoApply = &disabled
		}},
		{name: "automatic check is disabled", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			disabled := false
			manager.cfg.System.Update.AutoCheck = &disabled
		}},
		{name: "the installed version is already the latest", setup: func(_ *testing.T, manager *Manager, _ *state.Store) {
			manager.updateProbe.current = func() string { return "v1.1.0" }
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manager, store, spy := updateApplyFixture(t)
			test.setup(t, manager, store)
			manager.maybeApplyUpdate(context.Background())
			if len(spy.argv) != 0 {
				t.Fatalf("the daemon started %v although %s", spy.argv, test.name)
			}
		})
	}
}

// TestAutomaticUpdateStartsTheApplyCommandOncePerVersion は、保守の一巡ごとに判定しても
// 同じ版の適用が 1 回に留まること、子へ渡す argv が `wx update --apply` であることを固定する。
func TestAutomaticUpdateStartsTheApplyCommandOncePerVersion(t *testing.T) {
	t.Parallel()
	manager, _, spy := updateApplyFixture(t)
	ctx := context.Background()
	manager.maybeApplyUpdate(ctx)
	manager.maybeApplyUpdate(ctx)
	if len(spy.argv) != 1 {
		t.Fatalf("the daemon started the apply %d time(s), want 1", len(spy.argv))
	}
	want := []string{manager.executablePath, "update", "--apply"}
	if !slices.Equal(spy.argv[0], want) {
		t.Fatalf("argv=%v, want %v", spy.argv[0], want)
	}
	if got := spy.logs[0]; got != filepath.Join(manager.updateApply.logDir, updateApplyLogName) {
		t.Fatalf("log path=%q", got)
	}
}

// TestAutomaticUpdateDoesNotRetryAfterAFailedStart は、子の起動に失敗した回も試行として残すことを固定する。
// 残さないと、起動が失敗し続ける環境で保守の一巡ごとに起動を試み続ける。
func TestAutomaticUpdateDoesNotRetryAfterAFailedStart(t *testing.T) {
	t.Parallel()
	manager, _, spy := updateApplyFixture(t)
	spy.failure = errors.New("start failed")
	ctx := context.Background()
	manager.maybeApplyUpdate(ctx)
	manager.maybeApplyUpdate(ctx)
	if len(spy.argv) != 1 {
		t.Fatalf("the daemon started the apply %d time(s), want 1", len(spy.argv))
	}
}

// TestUpdateChildEnvDetachesFromLaunchdAndFindsTheInstaller は子へ渡す環境を固定する。
// XPC_SERVICE_NAME が残ると子側の underLaunchd が自分を daemon と誤判定し、
// PATH が足りないと install.sh が curl などの前提条件で落ちる。
func TestUpdateChildEnvDetachesFromLaunchdAndFindsTheInstaller(t *testing.T) {
	t.Parallel()
	env := updateChildEnv([]string{"XPC_SERVICE_NAME=com.user.wx", "PATH=/opt/tools", "HOME=/Users/example", "BARE"}, "/Users/example/.local/bin")
	if slices.ContainsFunc(env, func(entry string) bool { return entry == "XPC_SERVICE_NAME=com.user.wx" }) {
		t.Fatalf("env=%v still binds the child to the launchd job", env)
	}
	if !slices.Contains(env, "HOME=/Users/example") || !slices.Contains(env, "BARE") {
		t.Fatalf("env=%v dropped an unrelated entry", env)
	}
	want := "PATH=/Users/example/.local/bin:/opt/tools:" + updateApplySystemPath
	if !slices.Contains(env, want) {
		t.Fatalf("env=%v, want %q", env, want)
	}
}
