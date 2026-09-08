package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/state"
)

// leaseWorktreeFixture は貸出コマンド用の Manager と、貸出できる repository を用意する。
func leaseWorktreeFixture(t *testing.T, options ...managerFixtureOption) (*managerFixture, string) {
	t.Helper()
	requireDaemonIntegration(t)
	defaults := []managerFixtureOption{func(s *managerFixtureSetup) {
		s.Config.Worktree.Undefined = "cold"
		s.Config.Pool.WarmPerWorkspace = 0
		s.Config.Discovery.ReconcileInterval.Duration = time.Hour
	}}
	f := runningManagerFixture(t, append(defaults, options...)...)
	repo := filepath.Join(f.Root, "repo")
	initGitRepo(t, repo)
	return f, repo
}

// setLeaseExpiry は貸出の期限を直接書き換える。lease.ttl を待たずに期限掃引を確認するためである。
func setLeaseExpiry(t *testing.T, databasePath, sessionID, expiresAt string) {
	t.Helper()
	raw, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(context.Background(), `UPDATE sessions SET lease_expires_at=? WHERE id=?`, expiresAt, sessionID); err != nil {
		t.Fatal(err)
	}
}

// wx new の貸出は client を持たないため 45 秒の orphan 回収では返却されず、
// lease.ttl の期限が来たときだけ保存経路（SNAPSHOT ジョブ）を通って SNAPSHOTTED まで進む。
func TestPathLeaseSurvivesOrphanReconcileAndExpiresThroughSnapshot(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	session, err := store.SessionByID(ctx, lease.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.LeaseKind != state.LeaseKindPath || session.LeaseExpiresAt == "" {
		t.Fatalf("path lease session=%+v, want kind %s and a deadline", session, state.LeaseKindPath)
	}
	// heartbeat を最初から持たない貸出でも、orphan 回収は手を出さない。
	m.reconcileOrphans(ctx)
	if session, err := store.SessionByID(ctx, lease.SessionID); err != nil || session.State != "ACTIVE" {
		t.Fatalf("path lease was reclaimed as an orphan: session=%+v err=%v", session, err)
	}
	setLeaseExpiry(t, f.DatabasePath, lease.SessionID, state.FormatTime(time.Now().Add(-time.Minute)))
	m.reconcileExpiredLeases(ctx)
	waitUntil(t, 20*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	slot, err := store.Slot(ctx, lease.SessionID)
	if err != nil || slot.State != "SNAPSHOTTED" {
		t.Fatalf("expired lease slot=%+v err=%v", slot, err)
	}
	// 期限による返却も保存経路を通るため、復元に使える snapshot が残る。
	if snapshots, err := store.Snapshots(ctx, lease.SessionID); err != nil || len(snapshots) != 1 {
		t.Fatalf("expired lease snapshots=%+v err=%v", snapshots, err)
	}
	// 実体は retention.ended_worktree の間残り、wx shell --resume で戻せる。
	if _, err := os.Stat(lease.Path); err != nil {
		t.Fatalf("expired lease worktree is already gone: %v", err)
	}
}

// 親 session が返却されると、その親が wx new で用意した子貸出もまとめて返却される。
func TestOwnerReleaseReturnsChildLeases(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	attrs, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	child, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, attrs)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, child.SessionID, child.Token); err != nil {
		t.Fatal(err)
	}
	if stored, err := store.SessionByID(ctx, child.SessionID); err != nil || stored.LeaseOwnerSessionID != owner.SessionID {
		t.Fatalf("child lease owner=%+v err=%v", stored, err)
	}
	// 親が使用中の間は返却しない。
	m.reconcileExpiredLeases(ctx)
	if stored, err := store.SessionByID(ctx, child.SessionID); err != nil || stored.State != "ACTIVE" {
		t.Fatalf("child lease was released while its owner was in use: session=%+v err=%v", stored, err)
	}
	if err := m.Release(ctx, owner.SessionID, owner.Token, "test"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		stored, _ := store.SessionByID(ctx, child.SessionID)
		return stored.State == "ARCHIVED"
	})
	if snapshots, err := store.Snapshots(ctx, child.SessionID); err != nil || len(snapshots) != 1 {
		t.Fatalf("child lease snapshots=%+v err=%v", snapshots, err)
	}
}

// 親の指定は既存の session token 検証を通す。誤った token や終了済みの親では貸出自体を断る。
func TestResolveLeaseAttrsAuthenticatesTheOwnerSession(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	m := f.Manager
	ctx := context.Background()
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, "wrong-token"); err == nil {
		t.Fatal("a lease owner with the wrong token was accepted")
	}
	if err := m.Release(ctx, owner.SessionID, owner.Token, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, owner.Token); err == nil {
		t.Fatal("a released lease owner was accepted")
	}
}

// 貸出の種別と親指定の組み合わせを検証する。
func TestResolveLeaseAttrsValidatesKindAndOwner(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	for name, test := range map[string]struct {
		kind, owner string
		wantKind    string
		wantErr     bool
	}{
		"agent by default":  {kind: "", wantKind: ""},
		"agent explicitly":  {kind: state.LeaseKindAgent, wantKind: ""},
		"agent with owner":  {kind: state.LeaseKindAgent, owner: "someone", wantErr: true},
		"shell lease":       {kind: state.LeaseKindShell, wantKind: state.LeaseKindShell},
		"command lease":     {kind: state.LeaseKindCommand, wantKind: state.LeaseKindCommand},
		"path lease":        {kind: state.LeaseKindPath, wantKind: state.LeaseKindPath},
		"unknown kind":      {kind: "worktree", wantErr: true},
		"missing owner row": {kind: state.LeaseKindPath, owner: "missing", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			attrs, err := f.Manager.resolveLeaseAttrs(ctx, test.kind, test.owner, "token")
			if test.wantErr {
				if err == nil {
					t.Fatalf("kind=%q owner=%q was accepted", test.kind, test.owner)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if attrs.Kind != test.wantKind || attrs.OwnerSessionID != "" {
				t.Fatalf("attrs=%+v, want kind %q", attrs, test.wantKind)
			}
		})
	}
}

// lease.ttl が 0 の設定では期限を持たせない。返却は親の終了と wx release だけになる。
func TestApplyLeaseAttrsSkipsExpiryWhenTTLIsZero(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t, func(s *managerFixtureSetup) { s.Config.Lease.TTL.Duration = 0 })
	session := state.Session{ID: "session"}
	f.Manager.applyLeaseAttrs(&session, leaseAttrs{Kind: state.LeaseKindPath, OwnerSessionID: "owner"})
	if session.LeaseKind != state.LeaseKindPath || session.LeaseOwnerSessionID != "owner" || session.LeaseExpiresAt != "" {
		t.Fatalf("session=%+v, want no deadline", session)
	}
	agent := state.Session{ID: "agent"}
	f.Manager.applyLeaseAttrs(&agent, leaseAttrs{})
	if agent.LeaseKind != "" || agent.LeaseExpiresAt != "" {
		t.Fatalf("agent session=%+v, want untouched lease columns", agent)
	}
}

// wx release は agent session と生きたプロセスを持つ貸出を拒否し、
// 返却できる貸出だけを保存経路へ進める。
func TestReleaseLeaseRefusesAgentSessionsAndLiveProcesses(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	agent, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, agent.SessionID, agent.Token); err != nil {
		t.Fatal(err)
	}
	_, err = m.ReleaseLease(ctx, agent.SessionID, "wx-release", false)
	if err == nil || !strings.Contains(err.Error(), "wx clear --all") {
		t.Fatalf("agent session release error=%v, want the wx clear guidance", err)
	}
	// 生きた client を持つ shell 貸出も、その shell の終了に任せる。
	shell, err := m.leaseWithPolicy(ctx, repo, nil, "wx-shell", os.Getpid(), false, leaseAttrs{Kind: state.LeaseKindShell})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, shell.SessionID, shell.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseLease(ctx, shell.SessionID, "wx-release", false); err == nil {
		t.Fatal("a lease with a live client was released")
	}
	if _, err := m.ReleaseLease(ctx, "missing", "wx-release", false); err == nil {
		t.Fatal("an unknown session was released")
	}
	// client を持たない wx new の貸出は返却でき、二度目は使用中でないとして断られる。
	detached, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, detached.SessionID, detached.Token); err != nil {
		t.Fatal(err)
	}
	reply, err := m.ReleaseLease(ctx, detached.SessionID, "wx-release", false)
	if err != nil {
		t.Fatal(err)
	}
	if reply["released"] != true || reply["discarded"] != false {
		t.Fatalf("release reply=%+v", reply)
	}
	waitUntil(t, 20*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, detached.SessionID)
		return session.State == "ARCHIVED"
	})
	if _, err := m.ReleaseLease(ctx, detached.SessionID, "wx-release", false); err == nil {
		t.Fatal("an already released lease was released again")
	}
}

// --discard は保存を要求せず削除へ進める。実体は残らず、snapshot も作らない。
func TestReleaseLeaseDiscardsWithoutSaving(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	// 保存ジョブが走っている間は削除を予約できないので、返却済みの貸出へ --discard を追いかけさせる。
	waitUntil(t, 30*time.Second, func() bool {
		reply, err := m.ReleaseLease(ctx, lease.SessionID, "wx-release", true)
		return err == nil && reply["discarded"] == true
	})
	waitUntil(t, 20*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		_, pathErr := os.Stat(lease.Path)
		return slot.State == "ARCHIVED" && os.IsNotExist(pathErr)
	})
}

// 貸出は種別をまたいで復元できる。agent 会話は従来どおり厳密一致に留める。
func TestResumeAgentMatchesAcrossLeaseKinds(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct {
		agent, originalAgent string
		kind, originalKind   string
		want                 bool
	}{
		"same agent":                     {agent: "codex", originalAgent: "codex", want: true},
		"different agents":               {agent: "codex", originalAgent: "claude"},
		"shell resumes a path lease":     {agent: "wx-shell", originalAgent: "wx-path", kind: state.LeaseKindShell, originalKind: state.LeaseKindPath, want: true},
		"shell resumes a command lease":  {agent: "wx-shell", originalAgent: "wx-run", kind: state.LeaseKindShell, originalKind: state.LeaseKindCommand, want: true},
		"lease does not resume an agent": {agent: "wx-shell", originalAgent: "codex", kind: state.LeaseKindShell},
		"agent does not resume a lease":  {agent: "codex", originalAgent: "wx-path", originalKind: state.LeaseKindPath},
	} {
		t.Run(name, func(t *testing.T) {
			if got := resumeAgentMatches(test.agent, test.originalAgent, test.kind, test.originalKind); got != test.want {
				t.Fatalf("resumeAgentMatches(%q,%q,%q,%q)=%v, want %v", test.agent, test.originalAgent, test.kind, test.originalKind, got, test.want)
			}
		})
	}
}

// wx new が出した貸出も wx shell --resume で開ける。
// これが通らないと、期限や wx release で保存された path 貸出の内容を取り戻す経路が無くなる。
func TestShellResumeReopensAPathLease(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ReleaseLease(ctx, lease.SessionID, "wx-release", false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 20*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	resumed, err := m.Resume(ctx, lease.SessionID, "wx-shell", os.Getpid(), false, ResumeOptions{Lease: leaseAttrs{Kind: state.LeaseKindShell}})
	if err != nil {
		t.Fatalf("resume a path lease as a shell lease: %v", err)
	}
	if err := waitReady(ctx, m, 20*time.Second, resumed.SessionID, resumed.Token); err != nil {
		t.Fatal(err)
	}
	session, err := store.SessionByID(ctx, resumed.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// 復元後の session は実際に動いている種別で登録され、wx slots の AGENT 列もそれを指す。
	if session.AgentKind != "wx-shell" || session.LeaseKind != state.LeaseKindShell {
		t.Fatalf("resumed session=%+v, want a shell lease", session)
	}
	// agent 会話は従来どおり厳密一致のままで、貸出の種別では開けない。
	if _, err := m.Resume(ctx, lease.SessionID, "codex", os.Getpid(), false); err == nil {
		t.Fatal("a lease session was resumed as an agent conversation")
	}
}

// 期限掃引と親連動は、プロセスが生きている貸出（実行中の wx shell / wx run）を返却しない。
// この 2 経路が生存を見ないと、動いているシェルの worktree が使用中のまま返却へ落ちる。
func TestExpiredAndOrphanedLeasesSkipRunningProcesses(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	attrs, err := m.resolveLeaseAttrs(ctx, state.LeaseKindShell, owner.SessionID, owner.Token)
	if err != nil {
		t.Fatal(err)
	}
	shell, err := m.leaseWithPolicy(ctx, repo, nil, "wx-shell", os.Getpid(), false, attrs)
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, shell.SessionID, shell.Token); err != nil {
		t.Fatal(err)
	}
	// 期限が過ぎていても、そのシェルが動いている間は返却しない。
	setLeaseExpiry(t, f.DatabasePath, shell.SessionID, state.FormatTime(time.Now().Add(-time.Minute)))
	m.reconcileExpiredLeases(ctx)
	if stored, err := store.SessionByID(ctx, shell.SessionID); err != nil || stored.State != "ACTIVE" {
		t.Fatalf("a running shell lease was released at its deadline: session=%+v err=%v", stored, err)
	}
	// 親が終了しても、その子が動いている間は返却しない。
	if err := m.Release(ctx, owner.SessionID, owner.Token, "test"); err != nil {
		t.Fatal(err)
	}
	m.releaseOrphanedChildLeases(ctx)
	if stored, err := store.SessionByID(ctx, shell.SessionID); err != nil || stored.State != "ACTIVE" {
		t.Fatalf("a running child lease was released when its owner ended: session=%+v err=%v", stored, err)
	}
}

// 保存まで進んだ貸出へ --discard を追いかけさせると、再実行しても変わらない理由が返る。
func TestReleaseLeaseDiscardReportsAnAlreadyRemovedSlot(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, lease.SessionID, lease.Token); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 30*time.Second, func() bool {
		reply, err := m.ReleaseLease(ctx, lease.SessionID, "wx-release", true)
		return err == nil && reply["discarded"] == true
	})
	waitUntil(t, 20*time.Second, func() bool {
		slot, _ := store.Slot(ctx, lease.SessionID)
		return slot.State == "ARCHIVED" || slot.State == "REMOVING"
	})
	reply, err := m.ReleaseLease(ctx, lease.SessionID, "wx-release", true)
	if err != nil {
		t.Fatal(err)
	}
	if reply["discarded"] != false || reply["discard_pending"] != DiscardPendingRemoved {
		t.Fatalf("discard reply=%+v, want %q so the CLI does not advise another run", reply, DiscardPendingRemoved)
	}
}

// 返却の書き込みが失敗したら、成功として返さない。
// wx release が終了コード 0 を返すと、貸出が使用中のまま残っていることを誰も検知できない。
func TestReleaseLeaseWithoutTokenReturnsWriteFailures(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	if err := f.Store.Close(); err != nil {
		t.Fatal(err)
	}
	candidate := state.OrphanCandidate{ID: "session", WorkspaceID: "workspace", SlotID: "slot"}
	if err := f.Manager.releaseLeaseWithoutToken(context.Background(), candidate, "test"); err == nil {
		t.Fatal("a failed release was reported as a success")
	}
}

// worktree を使わない設定の workspace では貸出コマンドを断り、CLI が引数エラーへ落とせる印を残す。
func TestLeaseRefusesWorkspacesConfiguredWithoutAWorktree(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t, func(s *managerFixtureSetup) { s.Config.Worktree.Undefined = "off" })
	ctx := context.Background()
	_, err := f.Manager.leaseWithPolicy(ctx, repo, nil, "wx-shell", os.Getpid(), false, leaseAttrs{Kind: state.LeaseKindShell})
	if !IsWorktreeDisabled(err) {
		t.Fatalf("lease error=%v, want the disabled-worktree marker", err)
	}
	// agent 起動は従来どおり、貸出の許可がないという既存の失敗で断られる。
	if _, err := f.Manager.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false); err == nil || IsWorktreeDisabled(err) {
		t.Fatalf("agent lease error=%v, want the existing authorization failure", err)
	}
}
