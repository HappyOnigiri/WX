package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSessionLifecycleSoak(t *testing.T) {
	raw := os.Getenv("WX_SOAK_SESSIONS")
	if raw == "" {
		t.Skip("set WX_SOAK_SESSIONS to run the lifecycle soak test")
	}
	sessions, err := strconv.Atoi(raw)
	if err != nil || sessions < 1 || sessions > 1000 {
		t.Fatalf("invalid WX_SOAK_SESSIONS=%q", raw)
	}
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	workspace := discovery.Workspace{ID: "soak-workspace", Root: domain.CanonicalPath(root), Kind: "repository"}
	if _, err := store.UpsertWorkspaceGeneration(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Retention.EndedWorktree.Duration = -time.Second
	manager := testManager(t, cfg, store)
	defer manager.Close()

	for i := 0; i < sessions; i++ {
		id := domain.StableID("soak", fmt.Sprint(i))
		path := filepath.Join(cfg.Storage.WorktreeRoot, "workspaces", string(workspace.ID), "slots", id, "root")
		_, err := store.CreateSlotSession(ctx,
			state.Slot{ID: id, WorkspaceID: string(workspace.ID), Generation: 1, Path: path, State: "PREPARING"}, nil,
			state.Session{ID: id, WorkspaceID: string(workspace.ID), SlotID: id, State: "STARTING", AgentKind: "soak", TokenHash: state.HashToken(id)}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, id); err != nil {
			t.Fatal(err)
		}
		job, changed, err := store.Release(ctx, id, string(workspace.ID), id)
		if err != nil || !changed {
			t.Fatalf("release %d changed=%v err=%v", i, changed, err)
		}
		claimed, err := store.ClaimJob(ctx, job.ID, "soak")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.BeginSnapshot(ctx, id, id); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkArchived(ctx, id, id, state.FormatTime(time.Now().Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
		if err := store.FinishJob(ctx, claimed.ID, "soak", nil); err != nil {
			t.Fatal(err)
		}
	}

	count, err := manager.GC(ctx, false)
	if err != nil || count != sessions {
		t.Fatalf("GC count=%d, want %d, err=%v", count, sessions, err)
	}
	for {
		jobs, err := store.RecoverJobs(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		removed := 0
		for _, job := range jobs {
			if job.Kind != "REMOVE" {
				continue
			}
			claimed, err := store.ClaimJob(ctx, job.ID, "soak-gc")
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.runRecoveredJob(ctx, claimed); err != nil {
				t.Fatal(err)
			}
			if err := store.FinishJob(ctx, claimed.ID, "soak-gc", nil); err != nil {
				t.Fatal(err)
			}
			removed++
		}
		if removed == 0 {
			break
		}
	}
	status, err := store.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Leased != 0 || status.Active != 0 || status.Jobs != 0 {
		t.Fatalf("lifecycle did not converge: %+v", status)
	}
	if entries, err := os.ReadDir(cfg.Storage.WorktreeRoot); err != nil && !errors.Is(err, os.ErrNotExist) || len(entries) != 0 {
		t.Fatalf("worktree root entries=%v err=%v", entries, err)
	}
}
