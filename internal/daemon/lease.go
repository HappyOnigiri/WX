package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// readyValidateMaxWorkers は READY 検証の並列度の上限である。
// repository ごとに git を起動するため、discovery.max_entries が許す規模の workspace でも
// 同時プロセス数が repository 数のまま膨らまないよう上限を置く。
const readyValidateMaxWorkers = 10

// 貸出がどの経路で応答したかを表す Lease.Route の値。client は待機中の表示に使う。
const (
	// RouteReady は準備を待たずに貸出せた完全一致 READY である。
	RouteReady = "ready"
	// RouteUpdate は READY 待機枠を要求 OID へ更新してから貸出す経路である。
	RouteUpdate = "update"
	// RouteColdStart は worktree を新たに用意する経路である。COLD repository を含む READY の貸出もここに入る。
	RouteColdStart = "cold-start"
	// RouteRestore は会話の再開で当時の worktree を復元する経路である。
	RouteRestore = "restore"
)

type Lease struct {
	SessionID       string `json:"session_id"`
	Token           string `json:"token"`
	Path            string `json:"path"`
	RootIdentity    string `json:"root_identity,omitempty"`
	SourceWorkspace string `json:"source_workspace,omitempty"`
	Ready           bool   `json:"ready"`
	// RepositoryDirs は Path 直下の repository directory 名で、client が agent の --add-dir へ渡す。
	// 単一 repository の貸出は Path が worktree そのものなので空になる。
	RepositoryDirs []string `json:"repository_dirs,omitempty"`
	// Route はこの貸出が選んだ経路（Route* のいずれか）である。
	// 分岐を確定できるのは貸出の側だけなので、client へ推測させず応答に載せる。
	Route string `json:"route,omitempty"`
	// ReadinessMode と ReadinessTimeoutMS は slot 内の repository 個別指定を合成した実効値である。
	// client は repository の main path を知らないため自分では解決できず、daemon が応答へ載せる。
	// 空・0 のときは client の global 設定へ落ちる。
	ReadinessMode      string `json:"readiness_mode,omitempty"`
	ReadinessTimeoutMS int    `json:"readiness_timeout_ms,omitempty"`
}

// leaseReadiness は slot 内の repository の readiness 個別指定を1つの実効値へ合成する。
// mode はどれかが full なら full にする。full 要求の早期起動は約束を破るが、early 要求を待たせるのは遅いだけである。
// timeout は最長へ寄せる。最短にすると最も遅い repository が必ず timeout する。
func leaseReadiness(cfg config.Config, repositories []discovery.Repository) (string, int) {
	mode := ""
	var timeout time.Duration
	for _, repository := range repositories {
		readiness := cfg.ReadinessForRepository(string(repository.MainPath))
		if mode != "full" {
			mode = readiness.Mode
		}
		if readiness.Timeout.Duration > timeout {
			timeout = readiness.Timeout.Duration
		}
	}
	return mode, int(timeout.Milliseconds())
}

// ResolveAndLease は cwd の workspace を解決して貸出す。
// attrs は必須引数にしている。可変長にすると渡し忘れをコンパイラが検出できず、
// 貸出が黙って agent 起動（lease_kind='agent'）として登録される。
func (m *Manager) ResolveAndLease(ctx context.Context, cwd string, branches []string, agent string, pid int, attrs leaseAttrs) (Lease, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		return Lease{}, err
	}
	return m.leaseWorkspace(ctx, w, branches, agent, pid, false, attrs)
}

func (m *Manager) leaseWorkspace(ctx context.Context, w discovery.Workspace, branches []string, agent string, pid int, cold bool, attrs leaseAttrs) (Lease, error) {
	var err error
	w, err = m.store.CanonicalWorkspace(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	w, generation, err := m.store.UpsertWorkspaceGeneration(ctx, w)
	if err != nil {
		return Lease{}, err
	}
	// 補充はこの貸出を根拠に hot / cold を決める。last_leased_at を書く前に並走されても cold と判定させない。
	endLease := m.beginWorkspaceLease(string(w.ID))
	defer endLease()
	resolved, err := pool.ResolveBranches(ctx, m.git, w, branches)
	if err != nil {
		return Lease{}, err
	}
	// 準備設定の上書きを伴う貸出は必ず cold start にする。既存 worktree の再利用や差分更新では
	// 上書きが準備の一部にしか効かず、測る対象が上書きの効果にならない。
	// 上書きを混ぜた fingerprint を持つ slot も、この経路を通らないため通常の貸出へ紛れない。
	cold = cold || !attrs.Prepare.IsZero()
	reuseStandby, _ := m.Config().ReuseStandbyForWorkspace(string(w.Root))
	if !cold && reuseStandby {
		return m.leaseReusableStandby(ctx, w, resolved, generation, branches, agent, pid, attrs)
	}
	attempts, budget := 0, 0
	if !cold {
		// 再試行の予算は設定値ではなく実際の候補数に合わせる。併走するleaseやGCに1件ずつ奪われても、
		// 残る候補を見切ってからcold startへ落ちるためである。+1は探索中にREADYへ変わったslotの分。
		count, countErr := m.store.ReadySlotCount(ctx, string(w.ID))
		if countErr != nil {
			return Lease{}, countErr
		}
		budget = count + 1
	}
	for ; attempts < budget; attempts++ {
		ready, ok, err := m.store.ReadySlot(ctx, string(w.ID))
		if err != nil {
			return Lease{}, err
		}
		if !ok {
			break
		}
		lease, leased, leaseErr := func() (Lease, bool, error) {
			releaseRoot, holdErr := m.holdRootForPath(ready.Path)
			if holdErr != nil {
				m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, holdErr)
				return Lease{}, false, holdErr
			}
			defer releaseRoot()
			valid, matchErr := m.readyMatches(ctx, ready, resolved)
			if matchErr != nil {
				// 併走するleaseがREADYを奪った場合は所有権自体は疑わしくないため、隔離せず次の候補へ回す。
				if errors.Is(matchErr, state.ErrOwnership) && !errors.Is(matchErr, state.ErrSlotStateIneligible) {
					m.quarantineOwnershipFailure(ready.ID, []string{"READY"}, matchErr)
				}
				return Lease{}, false, matchErr
			}
			if !valid {
				return Lease{}, false, nil
			}
			repositories, repositoryErr := m.store.SlotRepositories(ctx, ready.ID)
			if repositoryErr != nil {
				return Lease{}, false, repositoryErr
			}
			leasePathValue := leasePath(ready.Path, w.Kind, repositories)
			rootIdentity, identityErr := m.ensureLeaseRoot(ready.Path, leasePathValue)
			if identityErr != nil {
				_ = m.store.SetSlotState(context.Background(), ready.ID, []string{"READY"}, "QUARANTINED", "LEASE_ROOT_OWNERSHIP_UNCERTAIN")
				return Lease{}, false, fmt.Errorf("pin ready lease root: %w", identityErr)
			}
			token, tokenErr := state.TokenHex()
			if tokenErr != nil {
				return Lease{}, false, tokenErr
			}
			hasCold := false
			for _, repository := range repositories {
				hasCold = hasCold || repository.State == "COLD"
			}
			sessionState := "ACTIVE"
			if hasCold {
				sessionState = "STARTING"
			}
			session := state.Session{ID: ready.ID, WorkspaceID: string(w.ID), SlotID: ready.ID, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
			m.applyLeaseAttrs(&session, attrs)
			if retainErr := m.retainLease(session.ID, leasePathValue); retainErr != nil {
				return Lease{}, false, retainErr
			}
			if hasCold {
				job, leaseErr := m.store.LeaseReadyWithCold(ctx, ready.ID, session)
				if leaseErr == nil {
					m.schedule(job)
					return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false, RepositoryDirs: leaseRepositoryDirs(ready.Path, leasePathValue, repositories), Route: RouteColdStart}.withReadiness(m.Config(), w), true, nil
				}
				m.releaseLease(session.ID)
				return Lease{}, false, nil
			}
			if replenishJob, replenished, leaseErr := m.store.LeaseReadyWithReplenishment(ctx, ready.ID, session); leaseErr == nil {
				m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
				return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true, RepositoryDirs: leaseRepositoryDirs(ready.Path, leasePathValue, repositories), Route: RouteReady}.withReadiness(m.Config(), w), true, nil
			}
			m.releaseLease(session.ID)
			return Lease{}, false, nil
		}()
		if leaseErr != nil {
			// READY取得後に状態が変わっただけならslotをSTALEにせず、残るREADYまたはcold allocateで応答する。
			if errors.Is(leaseErr, state.ErrSlotStateIneligible) {
				continue
			}
			// tracked変更は再試行で消えないため、貸出を失敗させずにSTALEへ落とし、
			// 残るREADYかcold startで応答する。幽霊READYを次の保守tickまで残さない。
			if errors.Is(leaseErr, workspace.ErrTrackedChanges) && len(branches) == 0 {
				_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
				m.log.Info("ready standby retired after tracked changes were found", "workspace_id", w.ID, "slot_id", ready.ID, "reason", leaseErr)
				continue
			}
			return Lease{}, leaseErr
		}
		if leased {
			return lease, nil
		}
		if len(branches) > 0 {
			break
		}
		mismatch := m.describeReadyMismatch(ctx, ready, resolved)
		_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
		m.log.Info("ready standby retired after validation", append([]any{"workspace_id", w.ID, "slot_id", ready.ID}, mismatch.logArgs()...)...)
	}
	if attempts > 0 {
		// 待機枠があったのにcold startへ落ちた事実は、記録しないと後から追跡できない。
		m.log.Info("warm lease fell back to a cold start", "workspace_id", w.ID, "ready_candidates", budget-1, "attempts", attempts)
	}
	return m.allocate(ctx, w, resolved, generation, agent, pid, attrs, "STARTING", "")
}

func (m *Manager) leaseReusableStandby(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved, generation int, branches []string, agent string, pid int, attrs leaseAttrs) (Lease, error) {
	candidates, err := m.store.ReadySlots(ctx, string(w.ID))
	if err != nil {
		return Lease{}, err
	}
	// 更新可能な古い候補が先に並んでも、完全一致する候補を常に優先する。
	for _, candidate := range candidates {
		matched, matchErr := m.readyMatches(ctx, candidate, resolved)
		if matchErr != nil {
			if errors.Is(matchErr, state.ErrOwnership) && !errors.Is(matchErr, state.ErrSlotStateIneligible) {
				m.quarantineOwnershipFailure(candidate.ID, []string{"READY"}, matchErr)
			}
			if errors.Is(matchErr, workspace.ErrTrackedChanges) && len(branches) == 0 {
				_ = m.store.SetSlotState(ctx, candidate.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
				m.log.Info("ready standby retired after tracked changes were found", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", matchErr)
			}
			continue
		}
		if !matched {
			continue
		}
		lease, leased, leaseErr := m.leaseMatchingReady(ctx, w, candidate, agent, pid, attrs)
		if leaseErr != nil {
			if standbyStateRace(leaseErr) || strings.Contains(leaseErr.Error(), "slot is no longer READY") {
				continue
			}
			return Lease{}, leaseErr
		}
		if leased {
			return lease, nil
		}
	}
	for _, candidate := range candidates {
		lease, updated, updateErr := m.leaseUpdatingStandby(ctx, w, candidate, resolved, agent, pid, attrs)
		if updateErr != nil {
			switch {
			case errors.Is(updateErr, state.ErrOwnership) && !errors.Is(updateErr, state.ErrSlotStateIneligible):
				m.quarantineOwnershipFailure(candidate.ID, []string{"READY"}, updateErr)
			case standbyStateRace(updateErr):
				m.log.Info("standby update candidate lost to a concurrent transition", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", updateErr)
			case errors.Is(updateErr, workspace.ErrTrackedChanges) && len(branches) == 0:
				// tracked変更は貸出を跨いで残るため、READYのままだと毎回棄却してcold startを払い、
				// 次の保守tickまでstatusのreadyにも数えられ続ける。更新不適格と同じくSTALEにして補充へ回す。
				// --branch指定を除く理由も同じで、main向けstandbyをbranch要求のために捨てない。
				_ = m.store.SetSlotState(ctx, candidate.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
				m.log.Info("standby retired after tracked changes were found", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", updateErr)
			case errors.Is(updateErr, workspace.ErrUpdateIneligible) && len(branches) == 0:
				// 更新不適格なstandbyは残しても毎回cold startになるだけなので、非reuse経路と同じくSTALEにして補充へ回す。
				// --branch指定を除くのは、main向けのstandbyをbranch要求のために捨てないためである。
				_ = m.store.SetSlotState(ctx, candidate.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
				// 更新互換fingerprintの入力は保存していないため、現在値を添えて設定変更との対応を追えるようにする。
				m.log.Info("standby retired as not updateable", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", updateErr,
					"generation", candidate.Generation, "copy_mode", copyModeSummary(m.Config(), w), "cow_min_size_kib", cowThresholdSummary(m.Config(), w))
			default:
				m.log.Info("standby update candidate rejected before writes", "workspace_id", w.ID, "slot_id", candidate.ID, "reason", updateErr)
			}
			continue
		}
		if updated {
			return lease, nil
		}
	}
	if len(candidates) > 0 {
		m.log.Info("warm lease fell back to a cold start", "workspace_id", w.ID, "ready_candidates", len(candidates), "attempts", len(candidates))
	}
	return m.allocate(ctx, w, resolved, generation, agent, pid, attrs, "STARTING", "")
}

func (m *Manager) readyMatches(ctx context.Context, s state.Slot, resolved []pool.Resolved) (bool, error) {
	root, ok := m.rootForPath(s.Path)
	if !ok {
		configured, configuredErr := config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if configuredErr != nil || !domain.IsWithin(configured, s.Path) {
			return false, nil
		}
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	defer closeOwner()
	relativeSlot, ok := relativeWithinRoot(root, s.Path)
	if !ok {
		return false, fmt.Errorf("%w: ready slot path is outside wx root", state.ErrOwnership)
	}
	slotInfo, err := owner.Lstat(relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	if slotInfo.Mode()&os.ModeSymlink != 0 || !slotInfo.IsDir() {
		return false, nil
	}
	slotDirectory, _, err := domain.OpenDirectoryAt(owner, relativeSlot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: open ready slot root: %w", state.ErrOwnership, err)
	}
	if err := slotDirectory.Close(); err != nil {
		return false, fmt.Errorf("%w: close ready slot root: %w", state.ErrOwnership, err)
	}
	return m.readyRepositoriesMatch(ctx, s, resolved, root, owner)
}

func (m *Manager) readyRepositoriesMatch(ctx context.Context, s state.Slot, resolved []pool.Resolved, root string, owner *os.Root) (bool, error) {
	repos, err := m.store.SlotRepositories(ctx, s.ID)
	if err != nil {
		return false, err
	}
	if len(repos) != len(resolved) {
		return false, nil
	}
	byID := map[string]state.SlotRepository{}
	for _, r := range repos {
		byID[r.RepositoryID] = r
	}
	// 安価な突き合わせだけを先に直列で済ませる。
	// 「ソース側で base が進んだ」という最も普通の不一致を、repository 数ぶんの Git 起動を始める前に返すためである。
	stored := make([]state.SlotRepository, len(resolved))
	for i, r := range resolved {
		found, ok := byID[string(r.Repository.ID)]
		if !ok || (found.State != "READY" && found.State != "COLD") || found.BaseOID != r.OID {
			return false, nil
		}
		stored[i] = found
	}
	preparer := m.newPreparer(m.Config(), s)
	results := make([]readyRepositoryResult, len(resolved))
	var wait sync.WaitGroup
	gate := make(chan struct{}, min(runtime.NumCPU(), readyValidateMaxWorkers))
	for i := range resolved {
		gate <- struct{}{}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-gate }()
			matched, err := m.readyRepositoryMatches(ctx, s, preparer, resolved[i], stored[i], root, owner)
			results[i] = readyRepositoryResult{matched: matched, err: err}
		}()
	}
	wait.Wait()
	// index 昇順で最初の非 OK を採り、直列版がその repository で返していた判定に揃える。
	// 到着順に採ると、同じ破損に対して貸出のたびに違うエラーが返り、daemon log と client の再現性が失われる。
	for _, result := range results {
		if result.err != nil {
			return false, result.err
		}
		if !result.matched {
			return false, nil
		}
	}
	return true, nil
}

// readyRepositoryResult は repository 1 件の検証結果である。
// 不一致（matched=false, err=nil）とエラーは呼び出し側で扱いが違うため畳まない。
type readyRepositoryResult struct {
	matched bool
	err     error
}

// readyRepositoryMatches は repository 1 件が READY 候補として再利用できるかを検証する。
// ファイル I/O と Git 起動を伴うため repository ごとに並列で呼ばれる。
// owner と preparer は候補 slot 全体で共有し、この経路は読み取りだけで両者の状態を変えない。
func (m *Manager) readyRepositoryMatches(ctx context.Context, s state.Slot, preparer *workspace.Preparer, r pool.Resolved, stored state.SlotRepository, root string, owner *os.Root) (bool, error) {
	fp, err := workspace.Fingerprint(s.Generation, r.OID, r.Repository, m.Config())
	if err != nil {
		return false, err
	}
	if fp != stored.Fingerprint {
		return false, nil
	}
	if stored.State == "COLD" {
		unmaterialized, coldErr := coldWorktreeUnmaterialized(owner, root, stored.WorktreePath)
		if coldErr != nil || !unmaterialized {
			return false, coldErr
		}
		return true, nil
	}
	relative, ok := relativeWithinRoot(root, stored.WorktreePath)
	if !ok {
		return false, fmt.Errorf("%w: ready worktree path is outside wx root", state.ErrOwnership)
	}
	info, err := owner.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect ready worktree path: %w", state.ErrOwnership, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, nil
	}
	directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
	if openErr != nil {
		return false, fmt.Errorf("%w: open ready worktree path: %w", state.ErrOwnership, openErr)
	}
	if closeErr := directory.Close(); closeErr != nil {
		return false, fmt.Errorf("%w: close ready worktree path: %w", state.ErrOwnership, closeErr)
	}
	if err := preparer.ValidateReady(ctx, r.Repository, stored.WorktreePath, r.OID); err != nil {
		return false, err
	}
	return true, nil
}

func coldWorktreeUnmaterialized(owner *os.Root, root, worktreePath string) (bool, error) {
	relative, ok := relativeWithinRoot(root, worktreePath)
	if !ok {
		return false, fmt.Errorf("%w: cold worktree path is outside wx root", state.ErrOwnership)
	}
	info, err := owner.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, nil
	}
	directory, _, openErr := domain.OpenDirectoryAt(owner, relative)
	if openErr != nil {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, openErr)
	}
	entries, readErr := directory.Readdirnames(1)
	closeErr := directory.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, fmt.Errorf("%w: inspect cold worktree path: %w", state.ErrOwnership, readErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("%w: close cold worktree path: %w", state.ErrOwnership, closeErr)
	}
	return len(entries) == 0, nil
}

// leaseWithPolicy は RPC の新規作成要求を検証する。一時許可は設定や standby の補充対象を変更しない。
// attrs を必須引数にしている理由は ResolveAndLease と同じである。
func (m *Manager) leaseWithPolicy(ctx context.Context, cwd string, branches []string, agent string, pid int, force bool, attrs leaseAttrs) (Lease, error) {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	w, err := discoverer.Resolve(ctx, cwd)
	if err != nil {
		w, err = m.resolveRetiredSlotPath(ctx, discoverer, cwd, err)
		if err != nil {
			return Lease{}, err
		}
	}
	mode := m.Config().WorktreeMode(string(w.Root))
	lease := attrs
	// 貸出コマンドは現在のディレクトリで動く選択肢を持たないため、off の workspace では方針の選び直しを促す。
	if isLeaseKind(lease.Kind) && mode == "off" {
		return Lease{}, fmt.Errorf("workspace %s is configured not to use a worktree; change worktree.undefined or the workspace policy %s", w.Root, WorktreeDisabledMarker)
	}
	if !force && mode != "hot" && mode != "cold" {
		if isLeaseKind(lease.Kind) {
			return Lease{}, fmt.Errorf("worktree creation is not authorized; configure this workspace with: wx config --workspace %s worktree cold", shellQuote(string(w.Root)))
		}
		return Lease{}, errors.New("worktree creation is not authorized; select a worktree policy or use --worktree")
	}
	return m.leaseWorkspace(ctx, w, branches, agent, pid, force || mode == "cold", lease)
}

// shellQuote は POSIX shell の単一引用符で path を囲む。
// 空白や shell 展開文字を含む workspace root でも、案内をそのまま実行できる形にする。
func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}

// WorktreeDisabledMarker は worktree を使わない設定の workspace へ貸出コマンドが来たことを示す機械可読なトークンである。
// RPC のエラーは文字列で届くため、CLI はこの区別で失敗（1）ではなく引数エラー（2）として終える。
const WorktreeDisabledMarker = "worktree=disabled"

// IsWorktreeDisabled は貸出コマンドが worktree を使わない workspace で断られたかを返す。
func IsWorktreeDisabled(err error) bool {
	return err != nil && strings.Contains(err.Error(), WorktreeDisabledMarker)
}

// resolveRetiredSlotPath は解決できなかった cwd が畳まれた slot のものなら、その slot の workspace root で解決し直す。
// resume では会話に記録された cwd が渡り、slot を畳んだ後は実体がないため、同じ workspace に新しい worktree を作って会話を続けられるようにする。
// 逆引きに失敗したときは DB の失敗も含めて cause を返し、解決できなかった理由を別の失敗に置き換えない。
func (m *Manager) resolveRetiredSlotPath(ctx context.Context, discoverer discovery.Discoverer, cwd string, cause error) (discovery.Workspace, error) {
	root, err := m.store.WorkspaceRootForSlotPath(ctx, filepath.Clean(cwd))
	if err != nil || root == "" {
		return discovery.Workspace{}, cause
	}
	w, err := discoverer.Resolve(ctx, root)
	if err != nil {
		return discovery.Workspace{}, cause
	}
	return w, nil
}

// withReadiness は貸出応答へ slot 単位の readiness 実効値を載せる。
func (l Lease) withReadiness(cfg config.Config, w discovery.Workspace) Lease {
	l.ReadinessMode, l.ReadinessTimeoutMS = leaseReadiness(cfg, w.Repositories)
	return l
}
