package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/state"
)

func TestSessionEndWaitsForForegroundClientExit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.CreateSlotSession(ctx, storeSlotAt(t, store, root, "", "live", filepath.Join(root, "live"), 0, "LEASED"), nil, state.Session{ID: "live", SlotID: "live", State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: state.HashToken("token")}, ""); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{store: store, jobQueue: newJobQueue(1), log: slog.New(slog.NewTextHandler(newDiagnosticLog(managerFixtureLogLimit), nil)), ctx: context.Background()}
	if err := manager.Release(ctx, "live", "token", "session-end-hook"); err != nil {
		t.Fatal(err)
	}
	if session, err := store.SessionByID(ctx, "live"); err != nil || session.State != "ACTIVE" {
		t.Fatalf("live SessionEnd changed session: state=%s err=%v", session.State, err)
	}
	if err := manager.Release(ctx, "live", "token", "client-exit"); err != nil {
		t.Fatal(err)
	}
	if session, err := store.SessionByID(ctx, "live"); err != nil || session.State != "RELEASING" {
		t.Fatalf("client exit did not release session: state=%s err=%v", session.State, err)
	}
}
