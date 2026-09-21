package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
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
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(context.Background(), `UPDATE sessions SET lease_expires_at=? WHERE id=?`, expiresAt, sessionID); err != nil {
		t.Fatal(err)
	}
}

// createReleaseLeaseSession は ReleaseLease の判定に必要な session と slot だけを登録する。
// 解放受付の検査で worktree 準備まで実行すると、パッケージ全体の race 実行時に準備の待機期限へ依存する。
func createReleaseLeaseSession(t *testing.T, f *managerFixture, id, kind string, clientPID int, ownerPID ...int) {
	t.Helper()
	slot := testSlot(t, f.Manager, "", id, 1, "LEASED")
	session := state.Session{
		ID: id, SlotID: id, State: "ACTIVE", AgentKind: id,
		LeaseKind: kind, ClientPID: clientPID, TokenHash: state.HashToken("token"),
	}
	if len(ownerPID) == 1 {
		session.LeaseOwnerPID = ownerPID[0]
	}
	if _, err := f.Store.CreateSlotSession(context.Background(), slot, nil, session, ""); err != nil {
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
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	attrs, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, owner.Token, 0)
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
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, "wrong-token", 0); err == nil {
		t.Fatal("a lease owner with the wrong token was accepted")
	}
	if err := m.Release(ctx, owner.SessionID, owner.Token, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.resolveLeaseAttrs(ctx, state.LeaseKindPath, owner.SessionID, owner.Token, 0); err == nil {
		t.Fatal("a released lease owner was accepted")
	}
}

// 貸出の種別と親指定の組み合わせを検証する。
func TestResolveLeaseAttrsValidatesKindAndOwner(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	ctx := context.Background()
	for name, test := range map[string]struct {
		kind, owner  string
		ownerPID     int
		wantKind     string
		wantOwnerPID int
		wantErr      bool
	}{
		"agent by default":       {kind: "", wantKind: ""},
		"agent explicitly":       {kind: state.LeaseKindAgent, wantKind: ""},
		"agent with owner":       {kind: state.LeaseKindAgent, owner: "someone", wantErr: true},
		"agent with owner pid":   {kind: state.LeaseKindAgent, ownerPID: 4242, wantErr: true},
		"shell lease":            {kind: state.LeaseKindShell, wantKind: state.LeaseKindShell},
		"command lease":          {kind: state.LeaseKindCommand, wantKind: state.LeaseKindCommand},
		"path lease":             {kind: state.LeaseKindPath, wantKind: state.LeaseKindPath},
		"path lease with pid":    {kind: state.LeaseKindPath, ownerPID: 4242, wantKind: state.LeaseKindPath, wantOwnerPID: 4242},
		"path lease with no pid": {kind: state.LeaseKindPath, ownerPID: -1, wantKind: state.LeaseKindPath},
		"unknown kind":           {kind: "worktree", wantErr: true},
		"missing owner row":      {kind: state.LeaseKindPath, owner: "missing", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			attrs, err := f.Manager.resolveLeaseAttrs(ctx, test.kind, test.owner, "token", test.ownerPID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("kind=%q owner=%q was accepted", test.kind, test.owner)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if attrs.Kind != test.wantKind || attrs.OwnerSessionID != "" || attrs.OwnerPID != test.wantOwnerPID {
				t.Fatalf("attrs=%+v, want kind %q owner pid %d", attrs, test.wantKind, test.wantOwnerPID)
			}
		})
	}
}

// lease.ttl が 0 の設定では期限を持たせない。返却は親の終了と wx release だけになる。
func TestApplyLeaseAttrsSkipsExpiryWhenTTLIsZero(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t, func(s *managerFixtureSetup) { s.Config.Lease.TTL.Duration = 0 })
	session := state.Session{ID: "session"}
	f.Manager.applyLeaseAttrs(&session, leaseAttrs{Kind: state.LeaseKindPath, OwnerSessionID: "owner", OwnerPID: 4242})
	if session.LeaseKind != state.LeaseKindPath || session.LeaseOwnerSessionID != "owner" || session.LeaseOwnerPID != 4242 || session.LeaseExpiresAt != "" {
		t.Fatalf("session=%+v, want no deadline", session)
	}
	agent := state.Session{ID: "agent"}
	f.Manager.applyLeaseAttrs(&agent, leaseAttrs{})
	if agent.LeaseKind != "" || agent.LeaseExpiresAt != "" || agent.LeaseOwnerPID != 0 {
		t.Fatalf("agent session=%+v, want untouched lease columns", agent)
	}
}

// wx release は agent session と生きたプロセスを持つ貸出を拒否し、
// 返却できる貸出だけを保存経路へ進める。
func TestReleaseLeaseRefusesAgentSessionsAndLiveProcesses(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	createReleaseLeaseSession(t, f, "agent", state.LeaseKindAgent, os.Getpid())
	_, err := m.ReleaseLease(ctx, "agent", "wx-release", false)
	if err == nil || !strings.Contains(err.Error(), "wx clear --all") {
		t.Fatalf("agent session release error=%v, want the wx clear guidance", err)
	}
	// 生きた client を持つ shell 貸出も、その shell の終了に任せる。
	createReleaseLeaseSession(t, f, "shell", state.LeaseKindShell, os.Getpid())
	if _, err := m.ReleaseLease(ctx, "shell", "wx-release", false); err == nil {
		t.Fatal("a lease with a live client was released")
	}
	if _, err := m.ReleaseLease(ctx, "missing", "wx-release", false); err == nil {
		t.Fatal("an unknown session was released")
	}
	// client を持たない wx new の貸出は返却でき、二度目は使用中でないとして断られる。
	createReleaseLeaseSession(t, f, "detached", state.LeaseKindPath, 0)
	reply, err := m.ReleaseLease(ctx, "detached", "wx-release", false)
	if err != nil {
		t.Fatal(err)
	}
	if reply["released"] != true || reply["discarded"] != false || reply["job_kind"] != "SNAPSHOT" {
		t.Fatalf("release reply=%+v", reply)
	}
	session, err := store.SessionByID(ctx, "detached")
	if err != nil || session.State != "RELEASING" {
		t.Fatalf("released session=%+v err=%v, want RELEASING until the snapshot job runs", session, err)
	}
	slot, err := store.Slot(ctx, "detached")
	if err != nil || slot.State != "DRAINING" {
		t.Fatalf("released slot=%+v err=%v, want DRAINING until the snapshot job runs", slot, err)
	}
	if _, err := m.ReleaseLease(ctx, "detached", "wx-release", false); err == nil {
		t.Fatal("an already released lease was released again")
	}
	// wx -n が渡した所有 PID は終了要求へ応答できる相手ではないため、生きていても拒否の理由にしない。
	// wx clear --all も同じ判定で、この貸出をその場で返却できる。
	createReleaseLeaseSession(t, f, "direct", state.LeaseKindPath, 0, os.Getpid())
	if !m.detachedLease(ctx, "direct") {
		t.Fatal("a lease whose owner pid is alive was treated as having a live client")
	}
	if _, err := m.ReleaseLease(ctx, "direct", "wx-release", false); err != nil {
		t.Fatalf("a lease with a live owner pid was refused: %v", err)
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
	// 返却と同じ transaction で REMOVE を積むので、保存の完了を待たず 1 回で予約が通る。
	reply, err := m.ReleaseLease(ctx, lease.SessionID, "wx-release", true)
	if err != nil || reply["discarded"] != true {
		t.Fatalf("release reply=%+v err=%v", reply, err)
	}
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
	owner, err := m.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReady(ctx, m, 10*time.Second, owner.SessionID, owner.Token); err != nil {
		t.Fatal(err)
	}
	attrs, err := m.resolveLeaseAttrs(ctx, state.LeaseKindShell, owner.SessionID, owner.Token, 0)
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
	if _, err := f.Manager.leaseWithPolicy(ctx, repo, nil, "codex", os.Getpid(), false, leaseAttrs{}); err == nil || IsWorktreeDisabled(err) {
		t.Fatalf("agent lease error=%v, want the existing authorization failure", err)
	}
}

// 方針未設定の貸出コマンドには、解決済み workspace root へ設定を保存する案内を返す。
func TestLeaseGuidanceUsesWorkspaceRootForNonAgentLeases(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t, func(s *managerFixtureSetup) { s.Config.Worktree.Undefined = "ask" })
	repo := filepath.Join(f.Root, "repo with 'quote' $HOME `tick`")
	initGitRepo(t, repo)
	nested := filepath.Join(repo, "nested", "cwd")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := "worktree creation is not authorized; configure this workspace with: wx config --workspace '" + strings.ReplaceAll(repo, "'", "'\\''") + "' worktree cold"
	for _, test := range []struct {
		name  string
		kind  string
		agent string
		pid   int
	}{
		{name: "shell", kind: state.LeaseKindShell, agent: "wx-shell", pid: os.Getpid()},
		{name: "command", kind: state.LeaseKindCommand, agent: "wx-run", pid: os.Getpid()},
		{name: "path", kind: state.LeaseKindPath, agent: "wx-path"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.Manager.leaseWithPolicy(context.Background(), nested, nil, test.agent, test.pid, false, leaseAttrs{Kind: test.kind})
			if err == nil || err.Error() != want {
				t.Fatalf("lease error=%v, want %q", err, want)
			}
			if strings.Contains(err.Error(), "--worktree") {
				t.Fatalf("lease error=%v, must not suggest --worktree", err)
			}
		})
	}
}

// 準備設定の上書きは貸出属性へ載る前に検証する。
// 不正な値のまま準備へ進めると、測定用の設定が黙って既定へ落ちた結果を比較表に並べてしまう。
func TestWithPrepareOverrideValidatesTheRequestedValues(t *testing.T) {
	t.Parallel()
	minSize := 64
	attrs, err := leaseAttrs{Kind: state.LeaseKindPath}.withPrepareOverride(config.CopyModeCopy, &minSize)
	if err != nil || attrs.Kind != state.LeaseKindPath || attrs.Prepare.CopyMode != config.CopyModeCopy {
		t.Fatalf("attrs=%+v err=%v, want the override carried with the lease kind", attrs, err)
	}
	if attrs.Prepare.COWMinSizeKiB == nil || *attrs.Prepare.COWMinSizeKiB != minSize {
		t.Fatalf("override=%+v, want the lower bound carried", attrs.Prepare)
	}
	empty, err := leaseAttrs{}.withPrepareOverride("", nil)
	if err != nil || !empty.Prepare.IsZero() {
		t.Fatalf("attrs=%+v err=%v, want no override without a request", empty, err)
	}
	negative := -1
	invalid := []struct {
		copyMode string
		minSize  *int
	}{{copyMode: "clone"}, {minSize: &negative}}
	for _, test := range invalid {
		if _, err := (leaseAttrs{}).withPrepareOverride(test.copyMode, test.minSize); err == nil {
			t.Fatalf("override copy_mode=%q min_size=%v, want it rejected", test.copyMode, test.minSize)
		}
	}
}

// setLeaseOwnerPID は貸出の所有 PID を直接書き換える。実際に wx -n を終了させずに回収を確認するためである。
func setLeaseOwnerPID(t *testing.T, databasePath, sessionID string, pid int) {
	t.Helper()
	raw := openTestDatabase(t, databasePath)
	if _, err := raw.ExecContext(context.Background(), `UPDATE sessions SET lease_owner_pid=? WHERE id=?`, pid, sessionID); err != nil {
		t.Fatal(err)
	}
}

// wx -n の中から取った wx new の貸出は、起動元プロセスが生きている間は返却されず、
// 消えたときに保存経路（DRAINING → SNAPSHOT）を通って返却される。
func TestDirectLeaseIsReturnedWhenItsLaunchingProcessExits(t *testing.T) {
	t.Parallel()
	f, repo := leaseWorktreeFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	lease, err := m.leaseWithPolicy(ctx, repo, nil, "wx-path", 0, false, leaseAttrs{Kind: state.LeaseKindPath, OwnerPID: os.Getpid()})
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
	if session.LeaseOwnerPID != os.Getpid() || session.ClientPID != 0 {
		t.Fatalf("direct lease session=%+v, want an owner pid without a client pid", session)
	}
	// 起動元が生きている間は、期限も来ていないので返却しない。
	m.reconcileExpiredLeases(ctx)
	if session, err := store.SessionByID(ctx, lease.SessionID); err != nil || session.State != "ACTIVE" {
		t.Fatalf("a lease with a live owner was released: session=%+v err=%v", session, err)
	}
	// 存在しない PID へ差し替えると、次の一巡で保存してから返す。
	setLeaseOwnerPID(t, f.DatabasePath, lease.SessionID, 99999999)
	m.reconcileExpiredLeases(ctx)
	waitUntil(t, 20*time.Second, func() bool {
		session, _ := store.SessionByID(ctx, lease.SessionID)
		return session.State == "ARCHIVED"
	})
	slot, err := store.Slot(ctx, lease.SessionID)
	if err != nil || slot.State != "SNAPSHOTTED" {
		t.Fatalf("released slot=%+v err=%v", slot, err)
	}
	if snapshots, err := store.Snapshots(ctx, lease.SessionID); err != nil || len(snapshots) != 1 {
		t.Fatalf("direct lease snapshots=%+v err=%v, want the work saved before release", snapshots, err)
	}
}

// 所有 PID を持たない素の wx new は、この一巡では回収しない。
func TestDirectLeaseReconcileIgnoresLeasesWithoutAnOwnerPID(t *testing.T) {
	t.Parallel()
	f := manualManagerFixture(t)
	store, m := f.Store, f.Manager
	ctx := context.Background()
	createReleaseLeaseSession(t, f, "detached", state.LeaseKindPath, 0)
	m.releaseExitedDirectLeases(ctx)
	if session, err := store.SessionByID(ctx, "detached"); err != nil || session.State != "ACTIVE" {
		t.Fatalf("a lease without an owner pid was released: session=%+v err=%v", session, err)
	}
}
