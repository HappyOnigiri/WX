package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestManagerResumeAndArchiveFailureStates(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	root, cfg, store, m := f.Root, f.Config, f.Store, f.Manager
	ctx := context.Background()
	w := discovery.Workspace{ID: "workspace", Root: discoveryPath(root), Kind: "multi_repository", Repositories: []discovery.Repository{
		{ID: "repository-1", MainPath: discoveryPath(filepath.Join(root, "repository-1")), CommonDir: discoveryPath(filepath.Join(root, "repository-1", ".git")), RelativePath: "repository-1", DefaultBranch: "main"},
		{ID: "repository-2", MainPath: discoveryPath(filepath.Join(root, "repository-2")), CommonDir: discoveryPath(filepath.Join(root, "repository-2", ".git")), RelativePath: "repository-2", DefaultBranch: "main"},
	}}
	w = registerTestWorkspace(t, store, w)
	createSession := func(id, sessionState, slotState, parent string) (state.Session, string) {
		t.Helper()
		token := id + "-token"
		session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: "codex", TokenHash: state.HashToken(token)}
		if sessionState == "UNBOUND" {
			session.WorkspaceID = ""
		}
		var repositories []state.SlotRepository
		if session.WorkspaceID != "" && parent == "" {
			repositories = []state.SlotRepository{
				{RepositoryID: "repository-1", DirName: "repository-1", State: slotState},
				{RepositoryID: "repository-2", DirName: "repository-2", State: slotState},
			}
		}
		if _, err := store.CreateSlotSession(ctx, slotAtPath(t, m, session.WorkspaceID, id, filepath.Join(cfg.Storage.WorktreeRoot, id), 1, slotState), repositories, session, ""); err != nil {
			t.Fatal(err)
		}
		return session, token
	}

	for _, priorState := range []string{"EXPIRED", "ACTIVE", "ARCHIVED"} {
		prior, _ := createSession(strings.ToLower(priorState)+"-prior", priorState, "SNAPSHOTTED", "")
		if _, err := m.Resume(ctx, prior.ID, "codex", os.Getpid(), false); err == nil {
			t.Fatalf("%s parent resumed without usable recovery", priorState)
		}
	}

	noParent, _ := createSession("no-parent", "RESTORING", "RESTORING", "")
	if err := m.resumeRestoreJob(ctx, noParent.ID); err == nil || !strings.Contains(err.Error(), "no parent") {
		t.Fatalf("parentless restore error=%v", err)
	}
	expiredParent, _ := createSession("expired-parent", "EXPIRED", "SNAPSHOTTED", "")
	expiredChild, _ := createSession("expired-child", "RESTORING", "RESTORING", expiredParent.ID)
	if err := m.resumeRestoreJob(ctx, expiredChild.ID); err == nil || !strings.Contains(err.Error(), "expired or incomplete") {
		t.Fatalf("expired snapshot restore error=%v", err)
	}
	expiredSlot, err := store.Slot(ctx, expiredChild.ID)
	if err != nil || expiredSlot.State != "QUARANTINED" {
		t.Fatalf("expired restore slot=%+v err=%v", expiredSlot, err)
	}

	incompleteParent, _ := createSession("incomplete-parent", "ARCHIVED", "SNAPSHOTTED", "")
	snapshot := state.Snapshot{ID: "incomplete-snapshot", SessionID: incompleteParent.ID, RepositoryID: "repository-1", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	if err := store.SaveSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	incompleteChild, _ := createSession("incomplete-child", "RESTORING", "RESTORING", incompleteParent.ID)
	if err := m.resumeRestoreJob(ctx, incompleteChild.ID); err == nil || !strings.Contains(err.Error(), "expired or incomplete") {
		t.Fatalf("incomplete snapshot restore error=%v", err)
	}

	archived, _ := createSession("archived", "ARCHIVED", "SNAPSHOTTED", "")
	if err := m.snapshotSession(ctx, archived); err != nil {
		t.Fatalf("archived snapshot replay: %v", err)
	}
	active, _ := createSession("active", "ACTIVE", "LEASED", "")
	if err := m.snapshotSession(ctx, active); err == nil || !strings.Contains(err.Error(), "cannot be snapshotted") {
		t.Fatalf("invalid snapshot state error=%v", err)
	}
}
