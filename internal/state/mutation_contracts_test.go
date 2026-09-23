package state

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

type mutationTestCommitter struct {
	err error
}

func (c mutationTestCommitter) Commit() error { return c.err }

func TestMutationFillSlotRepositoriesPrefersSessionForDetachedRows(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,remote_name,first_seen_at,last_seen_at) VALUES('slot-repository','/slot-repository','/slot-repository/.git','',?,?)`, now(), now()); err != nil {
		t.Fatal(err)
	}
	// 空の slot_id は detached session の表現である。両方の map に異なる repository を入れ、
	// 間違ったキーを選ぶと結果に現れるようにする。
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slots(id,workspace_id,generation,root_id,rel_path,state,created_at,updated_at) VALUES('', 'workspace',1,?,'workspace/empty','ARCHIVED',?,?)`, testRootID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO sessions(id,workspace_id,slot_id,state,agent_kind,session_token_hash,created_at) VALUES('detached','workspace','','ARCHIVED','codex',?,?)`, HashToken("detached"), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO slot_repositories(slot_id,repository_id,dir_name,state,requested_ref,base_oid,prepare_fingerprint) VALUES('','slot-repository','slot','ARCHIVED','','','')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO session_repositories(session_id,repository_id,relative_path,ordinal) VALUES('detached','repository','',0)`); err != nil {
		t.Fatal(err)
	}
	rows := []SlotSummary{{SlotID: "", SessionID: "detached"}}
	if err := store.fillSlotRepositories(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if len(rows[0].Repositories) != 1 || rows[0].Repositories[0] != "/workspace" {
		t.Fatalf("detached repositories=%v, want session repository", rows[0].Repositories)
	}
}

func TestMutationStandbyCapacityAndGenerationChecksCommitSuccessfully(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	slot := Slot{ID: "capacity-boundary", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", "capacity-boundary"), State: "PREPARING"}
	if _, err := store.CreateStandby(ctx, Slot{ID: "existing-capacity", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/existing-capacity", State: "PREPARING"}, nil); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.ReserveStandbyIfNeeded(ctx, slot, 1)
	if err != nil || reserved {
		t.Fatalf("exact-capacity reserve=%v err=%v", reserved, err)
	}
	job, created, err := store.CreateStandbyIfNeeded(ctx, slot, nil, 1)
	if err != nil || created || job.ID != "" {
		t.Fatalf("exact-capacity create job=%+v created=%v err=%v", job, created, err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET generation=2 WHERE id='workspace'`); err != nil {
		t.Fatal(err)
	}
	reserved, err = store.ReserveStandbyIfNeeded(ctx, slot, 2)
	if err != nil || reserved {
		t.Fatalf("stale-generation reserve=%v err=%v", reserved, err)
	}
}

func TestMutationStaleStandbyGenerationPropagatesCommitFailure(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("commit failed")
	reserved, err := finishUnreservedStandbyReservation(mutationTestCommitter{err: wantErr})
	if reserved || !errors.Is(err, wantErr) {
		t.Fatalf("stale-generation reservation=%v err=%v, want false and commit error", reserved, err)
	}
}
