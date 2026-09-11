package daemon

import (
	"context"
	"errors"
	"fmt"
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

func (m *Manager) allocate(ctx context.Context, w discovery.Workspace, resolved []pool.Resolved, generation int, agent string, pid int, attrs leaseAttrs, sessionState, parent string, pendingAgentID ...string) (Lease, error) {
	rootPath, rootID, err := m.activeRoot()
	if err != nil {
		return Lease{}, err
	}
	token, err := state.TokenHex()
	if err != nil {
		return Lease{}, err
	}
	slotState := "PREPARING"
	jobKind := "PREPARE"
	if sessionState == "RESTORING" {
		slotState = "RESTORING"
		jobKind = "RESTORE"
	}
	var lastErr error
	for range idAllocationAttempts {
		id, idErr := newSlotID()
		if idErr != nil {
			return Lease{}, idErr
		}
		lease, retry, allocErr := m.allocateWithID(ctx, id, rootPath, rootID, token, w, resolved, generation, agent, pid, attrs, sessionState, slotState, jobKind, parent, pendingAgentID...)
		if allocErr == nil {
			return lease, nil
		}
		if !retry {
			return Lease{}, allocErr
		}
		lastErr = allocErr
	}
	return Lease{}, fmt.Errorf("allocate slot: %w", lastErr)
}

const idAllocationAttempts = 10

// allocationRoute は新規 slot の貸出が積んだ job 種別を、client へ返す経路名へ写す。
func allocationRoute(jobKind string) string {
	if jobKind == "RESTORE" {
		return RouteRestore
	}
	return RouteColdStart
}

func (m *Manager) allocateWithID(ctx context.Context, id, rootPath, rootID, token string, w discovery.Workspace, resolved []pool.Resolved, generation int, agent string, pid int, attrs leaseAttrs, sessionState, slotState, jobKind, parent string, pendingAgentID ...string) (Lease, bool, error) {
	relPath, err := slotRelPath(string(w.ID), id)
	if err != nil {
		return Lease{}, false, err
	}
	slotPath := filepath.Join(rootPath, relPath)
	releaseRoot, err := m.holdRootForPath(slotPath)
	if err != nil {
		return Lease{}, false, err
	}
	defer releaseRoot()
	repos, err := m.slotRepos(slotPath, w, resolved, generation, nil, attrs.Prepare)
	if err != nil {
		return Lease{}, false, err
	}
	// 上書きは準備 job が読めるよう slot 行へ残す。fingerprint 側は slotRepos が同じ上書きで計算している。
	prepareOverride, err := attrs.Prepare.Encode()
	if err != nil {
		return Lease{}, false, err
	}
	if sessionState == "RESTORING" {
		for i := range repos {
			repos[i].State = "RESTORING"
		}
	}
	leasePathValue := leasePath(slotPath, w.Kind, repos)
	session := state.Session{ID: id, WorkspaceID: string(w.ID), SlotID: id, ParentSessionID: parent, State: sessionState, AgentKind: agent, ClientPID: pid, TokenHash: state.HashToken(token)}
	m.applyLeaseAttrs(&session, attrs)
	if sessionState == "RESTORING" {
		if len(pendingAgentID) > 0 {
			session.PendingAgentSessionID = pendingAgentID[0]
		}
		if session.PendingAgentSessionID == "" {
			old, err := m.store.SessionByID(ctx, parent)
			if err != nil {
				return Lease{}, false, err
			}
			session.PendingAgentSessionID = old.AgentSessionID
		}
	}
	if _, err := os.Lstat(slotPath); err == nil {
		return Lease{}, true, fmt.Errorf("%w: %s", errSlotPathExists, slotPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Lease{}, false, err
	}
	endReservation := m.beginReservation(id)
	defer endReservation()
	if err := m.store.ReserveSlot(ctx, state.Slot{ID: id, WorkspaceID: string(w.ID), Generation: generation, RootID: rootID, RelPath: relPath, OwnerSessionID: id, PrepareOverride: prepareOverride}); err != nil {
		return Lease{}, state.IsIDCollision(err), err
	}
	quarantineReservation := func() {
		if quarantineErr := m.store.QuarantineReservedSlot(context.Background(), id, "ALLOCATION_FAILED"); quarantineErr != nil {
			m.log.Error("quarantine failed slot reservation failed", "slot_id", id, "error", quarantineErr)
		}
	}
	slotIdentity, leaseIdentity, err := m.createSlotRoot(slotPath, leasePathValue)
	if err != nil {
		if errors.Is(err, errSlotPathExists) {
			return Lease{}, false, errors.Join(err, m.store.AbandonSlotReservation(ctx, id))
		}
		quarantineReservation()
		return Lease{}, false, err
	}
	if err := m.store.ConfirmSlotCreation(ctx, id, slotIdentity); err != nil {
		quarantineReservation()
		return Lease{}, false, err
	}
	if err := m.retainLease(id, leasePathValue); err != nil {
		quarantineReservation()
		return Lease{}, false, err
	}
	job, err := m.store.RegisterReservedSlotSession(ctx, id, repos, session, slotState, jobKind)
	if err != nil {
		m.releaseLease(id)
		quarantineReservation()
		return Lease{}, false, err
	}
	m.schedule(job)
	m.startBackground(m.runBackgroundGC)
	return Lease{SessionID: id, Token: token, Path: leasePathValue, RootIdentity: leaseIdentity, SourceWorkspace: string(w.Root), Ready: false, RepositoryDirs: leaseRepositoryDirs(slotPath, leasePathValue, repos), Route: allocationRoute(jobKind)}.withReadiness(m.Config(), w), false, nil
}

// workspace未確定slot用の予約namespaceであり、通常のworkspace IDには"_"接頭辞を許さない。
const unboundNamespace = "_unbound"

func slotRelPath(workspaceID, slotID string) (string, error) {
	// 作成側とorphan scanの列挙側で共有するroot相対layoutを生成する。
	if err := validateLayoutComponent("slot id", slotID); err != nil {
		return "", err
	}
	if err := validateLayoutComponent("workspace id", workspaceID); err != nil {
		return "", err
	}
	return filepath.Join(workspaceID, slotID), nil
}

func validateLayoutComponent(kind, value string) error {
	// root外への脱出とwxの予約namespaceとの衝突を拒否する。
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, `/\`) || strings.HasPrefix(value, "_") {
		return fmt.Errorf("%w: %s %q cannot be a wx layout path component", state.ErrOwnership, kind, value)
	}
	return nil
}

func leasePath(slotPath, kind string, repos []state.SlotRepository) string {
	// 単一repositoryだけはagentのCWDをworktreeにし、slotの所有権markerを親に残す。
	if kind == "repository" && len(repos) == 1 && repos[0].DirName != "" {
		return filepath.Join(slotPath, repos[0].DirName)
	}
	return slotPath
}

// leaseRepositoryDirs は lease path 直下の repository directory 名を、slot 作成時に記録した dir_name から返す。
// 設定や remote 名から再計算すると dir_name の上書き・dir_source・重複時の suffix とずれ、
// 存在しない path を渡された agent は黙って無視するため、記録済みの名前だけを権威にする。
// commentlint:allow-long -- 再計算してはいけない理由を残す
func leaseRepositoryDirs(slotPath, leasePathValue string, repos []state.SlotRepository) []string {
	// 単一 repository の貸出は lease path が worktree そのもので、配下に渡す repository がない。
	if leasePathValue != slotPath {
		return nil
	}
	var dirs []string
	for _, repo := range repos {
		if repo.DirName == "" {
			continue
		}
		dirs = append(dirs, repo.DirName)
	}
	return dirs
}

var errSlotPathExists = errors.New("unregistered slot path already exists")

func (m *Manager) createSlotRoot(slotPath, leasePathValue string) (string, string, error) {
	// allocationはmanagerがpinしたroot descriptorで行い、返すinode identityでclient側の置換検出を可能にする。
	root, err := config.ExpandHome(m.Config().Storage.WorktreeRoot)
	if err != nil {
		return "", "", err
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, slotPath) || !domain.IsWithin(root, leasePathValue) {
		return "", "", fmt.Errorf("slot path %s is outside wx worktree root", slotPath)
	}
	owner, closeOwner, err := m.rootDescriptor(root)
	if err != nil {
		return "", "", err
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return "", "", err
	}
	relativeSlot, ok := relativeWithinRoot(root, slotPath)
	if !ok {
		return "", "", errors.New("slot path is outside wx worktree root")
	}
	relativeLease, ok := relativeWithinRoot(root, leasePathValue)
	if !ok {
		return "", "", errors.New("lease path is outside wx worktree root")
	}
	m.mu.RLock()
	barrier := m.beforeSlotRootCreate
	m.mu.RUnlock()
	if barrier != nil {
		barrier()
	}
	if parent := filepath.Dir(relativeSlot); parent != "." {
		if err := owner.MkdirAll(parent, 0o700); err != nil {
			return "", "", fmt.Errorf("create slot namespace safely: %w", err)
		}
	}
	if err := owner.Mkdir(relativeSlot, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", "", fmt.Errorf("%w: %s", errSlotPathExists, slotPath)
		}
		return "", "", fmt.Errorf("create slot root safely: %w", err)
	}
	if relativeLease != relativeSlot {
		if err := owner.Mkdir(relativeLease, 0o700); err != nil {
			return "", "", fmt.Errorf("create lease root safely: %w", err)
		}
	}
	slotIdentity, err := directoryIdentityAt(owner, relativeSlot)
	if err != nil {
		return "", "", fmt.Errorf("open allocated slot root: %w", err)
	}
	if relativeLease == relativeSlot {
		return slotIdentity, slotIdentity, nil
	}
	leaseIdentity, err := directoryIdentityAt(owner, relativeLease)
	if err != nil {
		return "", "", fmt.Errorf("open allocated lease root: %w", err)
	}
	return slotIdentity, leaseIdentity, nil
}

func directoryIdentityAt(owner *os.Root, relative string) (string, error) {
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", err
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

func (m *Manager) ensureLeaseRoot(slotPath, leasePathValue string) (string, error) {
	// COLD eviction後もclientが直ちにCWDを開けるよう、既存root descriptor経由でlease directoryを復元する。
	if leasePathValue == slotPath {
		return m.ownedDirectoryIdentity(slotPath)
	}
	root, ok := m.rootForPath(slotPath)
	if !ok {
		return "", fmt.Errorf("%w: lease path %s has no registered worktree root", state.ErrOwnership, leasePathValue)
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, leasePathValue) {
		return "", fmt.Errorf("lease path %s is outside wx worktree root", leasePathValue)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return "", err
	}
	relative, ok := relativeWithinRoot(root, leasePathValue)
	if !ok || relative == "." {
		return "", fmt.Errorf("%w: lease path is outside wx worktree root", state.ErrOwnership)
	}
	if err := owner.MkdirAll(relative, 0o700); err != nil {
		return "", fmt.Errorf("create lease root safely: %w", err)
	}
	return directoryIdentityAt(owner, relative)
}

func (m *Manager) ownedDirectoryIdentity(path string) (string, error) {
	// path名ではなく所有root descriptorからinode identityを取得し、置換をfail closedにする。
	root, ok := m.rootForPath(path)
	if !ok {
		var err error
		root, err = config.ExpandHome(m.Config().Storage.WorktreeRoot)
		if err != nil {
			return "", err
		}
	}
	root = filepath.Clean(root)
	if !domain.IsWithin(root, path) {
		return "", fmt.Errorf("lease path %s is outside wx worktree root", path)
	}
	owner, closeOwner, err := m.existingRootDescriptor(root)
	if err != nil {
		return "", err
	}
	defer closeOwner()
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil {
		return "", err
	}
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		return "", fmt.Errorf("open lease root: %w", err)
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	return identity, nil
}

// slotRepos は slot の repository 行を組む。override は貸出要求が指定した準備設定の上書きで、
// fingerprint と更新互換 fingerprint を上書き後の設定で計算させる。
// 上書きを混ぜないと、既定設定の fingerprint を持つ slot が別設定で準備され、後の貸出で再利用される。
func (m *Manager) slotRepos(slotPath string, w discovery.Workspace, resolved []pool.Resolved, generation int, hot map[string]bool, override config.PrepareOverride) ([]state.SlotRepository, error) {
	// hotにないrepositoryはCOLDとして記録し、実際のlease時までcheckoutを遅らせる。
	cfg := override.Apply(m.Config())
	out := make([]state.SlotRepository, 0, len(resolved))
	taken := map[string]bool{}
	for _, r := range resolved {
		dirName := workspace.UniqueDirName(workspace.RepositoryDirName(r.Repository, cfg), taken)
		if err := validateLayoutComponent("repository directory", dirName); err != nil {
			return nil, err
		}
		fp, err := workspace.Fingerprint(generation, r.OID, r.Repository, cfg)
		if err != nil {
			return nil, err
		}
		compatibility, err := workspace.UpdateCompatibilityFingerprint(generation, r.Repository, cfg)
		if err != nil {
			return nil, err
		}
		repoState := "PREPARING"
		if hot != nil && !hot[string(r.Repository.ID)] {
			repoState = "COLD"
		}
		out = append(out, state.SlotRepository{RepositoryID: string(r.Repository.ID), DirName: dirName, WorktreePath: filepath.Join(slotPath, dirName), State: repoState, RequestedRef: r.RequestedRef, BaseOID: r.OID, Fingerprint: fp, CompatibilityFingerprint: compatibility})
	}
	return out, nil
}

func newSlotID() (string, error) { return domain.NewShortID() }

// beginReservation は自プロセスで進行中の slot 予約を記録し、解除する関数を返す。
// reconcile は ALLOCATING・REGISTERING を中断された確保と見なすため、進行中の予約を除外できるようにする。
func (m *Manager) beginReservation(id string) func() {
	m.mu.Lock()
	if m.inFlightReservations == nil {
		m.inFlightReservations = map[string]bool{}
	}
	m.inFlightReservations[id] = true
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		delete(m.inFlightReservations, id)
		m.mu.Unlock()
	}
}

// reservationInFlight は id の予約が自プロセスで進行中かを返す。
func (m *Manager) reservationInFlight(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.inFlightReservations[id]
}

// beginWorkspaceLease は貸出処理が進行中の workspace を記録し、解除する関数を返す。
// 並走する補充が、まだ last_leased_at を書いていない使用中の repository を cold と判定しないようにする。
func (m *Manager) beginWorkspaceLease(workspaceID string) func() {
	m.mu.Lock()
	if m.leasingWorkspaces == nil {
		m.leasingWorkspaces = map[string]int{}
	}
	m.leasingWorkspaces[workspaceID]++
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.leasingWorkspaces[workspaceID] <= 1 {
			delete(m.leasingWorkspaces, workspaceID)
		} else {
			m.leasingWorkspaces[workspaceID]--
		}
		m.mu.Unlock()
	}
}

// workspaceLeaseInFlight は workspace の貸出が自プロセスで進行中かを返す。
func (m *Manager) workspaceLeaseInFlight(workspaceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.leasingWorkspaces[workspaceID] > 0
}
