package state

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestForgetWorkspaceRefusesLiveRecoveryMappings は、復元資産を残したまま登録を消さないことと、
// その拒否が破棄で解消できる種類だと呼び出し側へ伝わることを固定する。
func TestForgetWorkspaceRefusesLiveRecoveryMappings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		seed  func(*testing.T, *Store, context.Context)
		want  error
		clear func(*testing.T, *Store, context.Context)
	}{
		{
			// 貸出中の拒否は破棄では解消しないので、案内する操作を分けるために種類も分ける。
			name: "leased slot",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ACTIVE", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "LEASED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrWorkspaceInUse,
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='session'`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.ExecContext(ctx, `UPDATE slots SET state='ARCHIVED',owner_session_id=NULL WHERE id='slot'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "session mapping",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "ARCHIVED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrWorkspaceHasRecovery,
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if _, err := store.db.ExecContext(ctx, `UPDATE sessions SET state='EXPIRED' WHERE id='session'`); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "repository snapshot",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveSnapshot(ctx, Snapshot{ID: "snapshot", SessionID: "session", RepositoryID: "repository", HeadOID: "head", HeadRef: "refs/wx/recovery/head", IndexTreeOID: "index", WorktreeOID: "worktree", WorktreeRef: "refs/wx/recovery/worktree", Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrWorkspaceHasRecovery,
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "workspace snapshot",
			seed: func(t *testing.T, store *Store, ctx context.Context) {
				session := Session{ID: "session", WorkspaceID: "workspace", SlotID: "slot", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("token")}
				if _, err := store.CreateSlotSession(ctx, Slot{ID: "slot", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/slot", State: "ARCHIVED"}, nil, session, ""); err != nil {
					t.Fatal(err)
				}
				if err := store.SaveWorkspaceSnapshot(ctx, WorkspaceSnapshot{SessionID: "session", RootID: testRootID, RelPath: "_recovery/workspace-snapshots/snapshot.tar", SHA256: strings.Repeat("a", 64), Status: "ARCHIVED", CreatedAt: now(), ExpiresAt: FormatTime(time.Now().Add(time.Hour))}); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrWorkspaceHasRecovery,
			clear: func(t *testing.T, store *Store, ctx context.Context) {
				if err := store.ExpireSessionSnapshots(ctx, "session"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			ctx := context.Background()
			test.seed(t, store, ctx)
			err := store.ForgetWorkspace(ctx, "/workspace")
			if !errors.Is(err, test.want) {
				t.Fatalf("forget error=%v, want %v", err, test.want)
			}
			// 破棄で解消するのは復元資産の拒否だけなので、案内もそのときだけ出す。
			if strings.Contains(err.Error(), "--discard-recovery") != errors.Is(err, ErrWorkspaceHasRecovery) {
				t.Fatalf("refusal %q does not match the guidance for %v", err, test.want)
			}
			if _, err := store.Workspace(ctx, "workspace"); err != nil {
				t.Fatalf("refused forget removed workspace: %v", err)
			}
			test.clear(t, store, ctx)
			if err := store.ForgetWorkspace(ctx, "/workspace"); err != nil {
				t.Fatalf("forget after recovery cleanup: %v", err)
			}
		})
	}
}

// TestForgetBlockersSeparateReclaimableSlotsFromRefusals は、呼び出し側が回収する待機 slot を
// 事前判定では数えず、回収しないまま登録を消そうとすれば断ることを固定する。
// 登録が先に消えると worktree の所有権を証明できず、実体が回収不能になる。
func TestForgetBlockersSeparateReclaimableSlotsFromRefusals(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	seedWorkspace(t, store)
	ctx := context.Background()
	if _, err := store.CreateStandby(ctx, Slot{ID: "standby", WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: "workspace/standby", State: "READY"}, nil); err != nil {
		t.Fatal(err)
	}
	// 準備が終わって READY になった待機枠を作る。実行待ちの PREPARE が残っていれば、slot とは別の理由で断られる。
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET state='SUCCEEDED' WHERE kind='PREPARE'`); err != nil {
		t.Fatal(err)
	}
	blockers, err := store.WorkspaceForgetBlockers(ctx, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if err := blockers.Err(); err != nil {
		t.Fatalf("standby slot blocked forget before it was reclaimed: %v", err)
	}
	slots, err := store.ReclaimableSlots(ctx, "workspace")
	if err != nil || len(slots) != 1 || slots[0].ID != "standby" || slots[0].State != "READY" {
		t.Fatalf("reclaimable slots=%+v err=%v", slots, err)
	}
	if err := store.ForgetWorkspace(ctx, "/workspace"); !errors.Is(err, ErrWorkspaceInUse) {
		t.Fatalf("forget error=%v, want the unreclaimed slot to be refused", err)
	}
}

func TestForgetWorkspaceStopsAtEveryDurableBoundary(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"slots", "sessions", "snapshots", "workspace_snapshots", "jobs"} {
		t.Run("query "+table, func(t *testing.T) {
			store := openTestStore(t)
			seedWorkspace(t, store)
			if _, err := store.db.Exec(`DROP TABLE ` + table); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatalf("forget succeeded without %s", table)
			}
		})
	}

	newArchivedWorkspace := func(t *testing.T) *Store {
		t.Helper()
		store := openTestStore(t)
		seedWorkspace(t, store)
		ctx := context.Background()
		session := Session{ID: "archived", WorkspaceID: "workspace", SlotID: "archived", State: "EXPIRED", AgentKind: "codex", TokenHash: HashToken("archived")}
		if _, err := store.CreateSlotSession(ctx, Slot{ID: session.SlotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", session.SlotID), State: "ARCHIVED"}, nil, session, ""); err != nil {
			t.Fatal(err)
		}
		return store
	}
	for _, test := range []struct {
		name    string
		trigger string
	}{
		{name: "slot update", trigger: `CREATE TRIGGER fail_forget_slot BEFORE UPDATE ON slots BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "session update", trigger: `CREATE TRIGGER fail_forget_session BEFORE UPDATE ON sessions BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "job update", trigger: `CREATE TRIGGER fail_forget_job BEFORE UPDATE ON jobs BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "repository membership delete", trigger: `CREATE TRIGGER fail_forget_membership BEFORE DELETE ON workspace_repositories BEGIN SELECT RAISE(ABORT,'fault'); END`},
		{name: "workspace delete", trigger: `CREATE TRIGGER fail_forget_workspace BEFORE DELETE ON workspaces BEGIN SELECT RAISE(ABORT,'fault'); END`},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newArchivedWorkspace(t)
			if test.name == "job update" {
				if _, err := store.db.Exec(`INSERT INTO jobs(id,kind,state,attempt,not_before) VALUES('finished','PREPARE','SUCCEEDED',0,NULL)`); err != nil {
					t.Fatal(err)
				}
				if _, err := store.db.Exec(`UPDATE jobs SET workspace_id='workspace' WHERE id='finished'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(test.trigger); err != nil {
				t.Fatal(err)
			}
			if err := store.ForgetWorkspace(context.Background(), "/workspace"); err == nil {
				t.Fatal("forget succeeded despite durable cleanup fault")
			}
		})
	}
}
