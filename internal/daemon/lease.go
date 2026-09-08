package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

type Lease struct {
	SessionID       string `json:"session_id"`
	Token           string `json:"token"`
	Path            string `json:"path"`
	RootIdentity    string `json:"root_identity,omitempty"`
	SourceWorkspace string `json:"source_workspace,omitempty"`
	Ready           bool   `json:"ready"`
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
					return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: false}, true, nil
				}
				m.releaseLease(session.ID)
				return Lease{}, false, nil
			}
			if replenishJob, replenished, leaseErr := m.store.LeaseReadyWithReplenishment(ctx, ready.ID, session); leaseErr == nil {
				m.handleNormalSessionSuccess(ctx, w, replenishJob, replenished)
				return Lease{SessionID: session.ID, Token: token, Path: leasePathValue, RootIdentity: rootIdentity, SourceWorkspace: string(w.Root), Ready: true}, true, nil
			}
			m.releaseLease(session.ID)
			return Lease{}, false, nil
		}()
		if leaseErr != nil {
			// READY取得後に状態が変わっただけならslotをSTALEにせず、残るREADYまたはcold allocateで応答する。
			if errors.Is(leaseErr, state.ErrSlotStateIneligible) {
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
		_ = m.store.SetSlotState(ctx, ready.ID, []string{"READY"}, "STALE", "READY_VALIDATION_FAILED")
	}
	if attempts > 0 {
		// 待機枠があったのにcold startへ落ちた事実は、記録しないと後から追跡できない。
		m.log.Info("warm lease fell back to a cold start", "workspace_id", w.ID, "ready_candidates", budget-1, "attempts", attempts)
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
	preparer := m.newPreparer(m.Config(), s)
	for _, r := range resolved {
		stored, ok := byID[string(r.Repository.ID)]
		if !ok || (stored.State != "READY" && stored.State != "COLD") || stored.BaseOID != r.OID {
			return false, nil
		}
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
			continue
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
	if lease.Kind != "" && mode == "off" {
		return Lease{}, fmt.Errorf("workspace %s is configured not to use a worktree; change worktree.undefined or the workspace policy %s", w.Root, WorktreeDisabledMarker)
	}
	if !force && mode != "hot" && mode != "cold" {
		return Lease{}, errors.New("worktree creation is not authorized; select a worktree policy or use --worktree")
	}
	return m.leaseWorkspace(ctx, w, branches, agent, pid, force || mode == "cold", lease)
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
