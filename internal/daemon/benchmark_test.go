package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func BenchmarkHotLease(b *testing.B) {
	ctx := context.Background()
	root := b.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	workspace := discovery.Workspace{ID: "workspace", Root: domain.CanonicalPath(root), Kind: "repository"}
	if err := store.UpsertWorkspace(ctx, workspace); err != nil {
		b.Fatal(err)
	}

	b.StopTimer()
	for i := 0; i < b.N; i++ {
		slotID := domain.StableID("benchmark-slot", fmt.Sprint(i))
		if _, err := store.CreateStandby(ctx, state.Slot{ID: slotID, WorkspaceID: string(workspace.ID), Generation: 1, Path: filepath.Join(root, slotID), State: "PREPARING"}, nil); err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.FinishPreparationWithRelease(ctx, slotID); err != nil {
			b.Fatal(err)
		}
	}
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		slotID := domain.StableID("benchmark-slot", fmt.Sprint(i))
		sessionID := domain.StableID("benchmark-session", fmt.Sprint(i))
		if err := store.LeaseReady(ctx, slotID, state.Session{ID: sessionID, WorkspaceID: string(workspace.ID), SlotID: slotID, State: "STARTING", AgentKind: "benchmark", TokenHash: state.HashToken(sessionID)}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkManagerStatus(b *testing.B) {
	root := b.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := New(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := manager.Status(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
