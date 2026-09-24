package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/pool"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
	"github.com/HappyOnigiri/WorktreeX/internal/textfmt"
	"github.com/HappyOnigiri/WorktreeX/internal/workspace"
)

// capacityProbeSlotID は doctor の容量診断が使う架空 slot の ID である。
// worktree root 直下の名前として扱うだけで、実体は作らない。
const capacityProbeSlotID = "doctor-capacity"

// ErrInsufficientPrepareSpace は準備を開始する前に空き容量が下限を下回った
// ことを表す。容量不足は再試行しても自然には解消しないため、job の retry
// budget を消費しない終端エラーとして扱う。
var ErrInsufficientPrepareSpace = errors.New("insufficient space for worktree preparation")

// ErrMissingLFSObjects は、容量を確保できても source から検証できない
// LFS object が残り、worktree への書込みを始められないことを表す。
var ErrMissingLFSObjects = errors.New("missing LFS objects cannot be repaired locally")

// CapacityVolume は volume ごとの必要量と空き容量である。
type CapacityVolume struct {
	Volume           string
	Target           string
	Required         int64
	Free             int64
	WorktreeRequired int64
	SharedRequired   int64
}

// CapacityReport は workspace 1 回分の容量診断を、repository 内訳と volume
// 集計に分けて保持する。診断表示は既存 diag.Finding の Details へ変換する。
type CapacityReport struct {
	Volumes      []CapacityVolume
	Repositories []workspace.CapacityEstimate
	WarmCount    int
	Sparse       bool
}

// InsufficientPrepareSpaceError は不足している volume を呼び出し側へ渡す。
type InsufficientPrepareSpaceError struct {
	Report CapacityReport
}

func (e *InsufficientPrepareSpaceError) Error() string {
	if e == nil {
		return ErrInsufficientPrepareSpace.Error()
	}
	parts := make([]string, 0, len(e.Report.Volumes))
	for _, volume := range e.Report.Volumes {
		if volume.Required > volume.Free {
			parts = append(parts, fmt.Sprintf("%s requires %d bytes but only %d are free", volume.Target, volume.Required, volume.Free))
		}
	}
	if len(parts) == 0 {
		return ErrInsufficientPrepareSpace.Error()
	}
	return ErrInsufficientPrepareSpace.Error() + ": " + strings.Join(parts, "; ")
}

func (e *InsufficientPrepareSpaceError) Unwrap() error { return ErrInsufficientPrepareSpace }

// MissingLFSObjectsError は準備前の cache 修復で残った object を呼び出し側へ渡す。
// 容量不足と同じく retry budget を消費せず、preparation state は FAILED のままにする。
type MissingLFSObjectsError struct {
	Report     CapacityReport
	Repository string
	Failures   []workspace.LFSRepairFailure
	Err        error
}

func (e *MissingLFSObjectsError) Error() string {
	if e == nil {
		return ErrMissingLFSObjects.Error()
	}
	if e.Err != nil {
		return ErrMissingLFSObjects.Error() + ": " + e.Err.Error()
	}
	parts := make([]string, 0, len(e.Failures))
	for _, failure := range e.Failures {
		parts = append(parts, failure.Error())
	}
	if len(parts) == 0 {
		return ErrMissingLFSObjects.Error()
	}
	return ErrMissingLFSObjects.Error() + ": " + strings.Join(parts, "; ")
}

func (e *MissingLFSObjectsError) Unwrap() error { return ErrMissingLFSObjects }

type capacityEstimator func(context.Context, *workspace.Preparer, config.Config, discovery.Repository, string) (workspace.CapacityEstimate, error)

// checkPrepareCapacity は target OID の準備が書込みを始める前に必要とする
// volume 別の容量を求める。Git/statfs が読めない回は report を返さず error と
// し、呼び出し側が「測れなかったので準備は続行する」方針を選べるようにする。
func (m *Manager) checkPrepareCapacity(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, cfg config.Config, multiplier int) (CapacityReport, error) {
	return m.checkPrepareCapacityWithEstimator(ctx, slot, w, resolved, repos, cfg, multiplier, m.estimateCapacity)
}

// checkPrepareCapacityWithEstimator は容量見積りの取得だけを差し替えられる検証境界である。
// 通常の準備・doctor は estimateCapacity を使い、古い cache 形式や複数 volume の
// 集計境界は filesystem と Git の状態を作らずに同じ集計を検証できるようにする。
func (m *Manager) checkPrepareCapacityWithEstimator(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, cfg config.Config, multiplier int, estimateCapacity capacityEstimator) (CapacityReport, error) {
	multiplier = max(multiplier, 1)
	releaseRoot, err := m.holdRootForPath(slot.Path)
	if err != nil {
		return CapacityReport{}, fmt.Errorf("hold capacity root descriptor: %w", err)
	}
	defer releaseRoot()
	root, ok := m.rootForPath(slot.Path)
	if !ok {
		return CapacityReport{}, fmt.Errorf("capacity root is outside known wx roots")
	}
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		return CapacityReport{}, errors.New("capacity root descriptor is unavailable")
	}
	rootDirectory, err := owner.Open(".")
	if err != nil {
		return CapacityReport{}, fmt.Errorf("open capacity root descriptor: %w", err)
	}
	rootVolume, rootFree, err := m.volumeFreeBytes(rootDirectory)
	_ = rootDirectory.Close()
	if err != nil {
		return CapacityReport{}, fmt.Errorf("statfs worktree root: %w", err)
	}
	report := CapacityReport{WarmCount: multiplier}
	volumes := map[string]*CapacityVolume{rootVolume: {Volume: rootVolume, Target: root, Free: rootFree}}
	addWorktree := func(volume, target string, required int64) {
		entry := volumes[volume]
		if entry == nil {
			entry = &CapacityVolume{Volume: volume, Target: target}
			volumes[volume] = entry
		}
		entry.WorktreeRequired = capacityAdd(entry.WorktreeRequired, required)
		entry.Required = capacityAdd(entry.Required, required)
	}
	addShared := func(volume, target string, required int64) {
		entry := volumes[volume]
		if entry == nil {
			entry = &CapacityVolume{Volume: volume, Target: target}
			volumes[volume] = entry
		}
		entry.SharedRequired = capacityAdd(entry.SharedRequired, required)
		entry.Required = capacityAdd(entry.Required, required)
	}
	if w.Kind == "multi_repository" {
		rules, rulesErr := workspace.ResolveRootRules(string(w.Root), cfg.WorkspaceFor(string(w.Root)))
		if rulesErr != nil {
			return CapacityReport{}, fmt.Errorf("resolve workspace root copy rules: %w", rulesErr)
		}
		rootBytes, bytesErr := workspace.EstimateRootCopyBytes(string(w.Root), rules)
		if bytesErr != nil {
			return CapacityReport{}, fmt.Errorf("estimate workspace root copy bytes: %w", bytesErr)
		}
		addWorktree(rootVolume, root, capacityMul(rootBytes, multiplier))
	}
	storedByID := make(map[string]state.SlotRepository, len(repos))
	for _, stored := range repos {
		storedByID[stored.RepositoryID] = stored
	}
	preparer := m.newPreparer(cfg, slot)
	preparer.WorkspaceRoot = string(w.Root)
	seenCache := map[string]bool{}
	for _, item := range resolved {
		stored, exists := storedByID[string(item.Repository.ID)]
		if exists && (stored.State == "READY" || stored.State == "COLD") {
			continue
		}
		estimate, estimateErr := estimateCapacity(ctx, preparer, cfg, item.Repository, item.OID)
		if estimateErr != nil {
			return CapacityReport{}, fmt.Errorf("estimate repository %s capacity: %w", item.Repository.MainPath, estimateErr)
		}
		estimate.RepositoryID = string(item.Repository.ID)
		report.Repositories = append(report.Repositories, estimate)
		report.Sparse = report.Sparse || estimate.Sparse
		addWorktree(rootVolume, string(item.Repository.MainPath), capacityMul(estimate.WorktreeBytes, multiplier))
		if estimate.LFSCacheBytes == 0 {
			continue
		}
		common := string(item.Repository.CommonDir)
		directory, openErr := os.Open(common)
		if openErr != nil {
			return CapacityReport{}, fmt.Errorf("open repository common directory %s: %w", common, openErr)
		}
		volume, free, statErr := m.volumeFreeBytes(directory)
		_ = directory.Close()
		if statErr != nil {
			return CapacityReport{}, fmt.Errorf("statfs repository common directory %s: %w", common, statErr)
		}
		if existing := volumes[volume]; existing == nil {
			volumes[volume] = &CapacityVolume{Volume: volume, Target: common, Free: free}
		} else {
			// 同じ volume を複数 descriptor から測ったときは、診断中に空きが
			// 減った可能性を取りこぼさないよう小さい値を使う。
			existing.Free = min(existing.Free, free)
		}
		cacheBytes := int64(0)
		for _, object := range estimate.LFS {
			if object.Cached || object.CachePath == "" || seenCache[object.CachePath] {
				continue
			}
			seenCache[object.CachePath] = true
			cacheBytes = capacityAdd(cacheBytes, object.Size)
		}
		if len(estimate.LFS) == 0 {
			// 古い cache 形式を受け取る caller や、内訳を落とした値でも
			// LFS cache の下限を失わない。
			cacheBytes = estimate.LFSCacheBytes
		}
		// LFS object は同じ common directory を複数 slot が共有するため、
		// warm_count で重ねず一度だけ必要量へ加える。
		addShared(volume, common, cacheBytes)
	}
	for _, volume := range volumes {
		report.Volumes = append(report.Volumes, *volume)
	}
	slices.SortFunc(report.Volumes, func(left, right CapacityVolume) int {
		return strings.Compare(left.Volume, right.Volume)
	})
	return report, nil
}

// estimateCapacity は doctor と準備 preflight が共有する要求 OID 別の daemon cache。
// copy mode と CoW 下限を鍵へ含め、sparse 選択が有効なときは毎回再計算して
// 設定・選択変更後の古い見積りを使わない。cache miss の同時重複は許容する。
func (m *Manager) estimateCapacity(ctx context.Context, preparer *workspace.Preparer, cfg config.Config, repo discovery.Repository, oid string) (workspace.CapacityEstimate, error) {
	oid = strings.TrimSpace(oid)
	key := strings.Join([]string{
		string(repo.ID), oid,
		cfg.CopyModeForWorkspaceRepository(preparer.WorkspaceRoot, repo.RelativePath, string(repo.MainPath)),
		strconv.Itoa(cfg.COWMinSizeKiBForWorkspaceRepository(preparer.WorkspaceRoot, repo.RelativePath, string(repo.MainPath))),
	}, "\x00")
	sparse, err := preparer.SparseCheckoutEnabled(ctx, repo)
	if err != nil {
		return workspace.CapacityEstimate{}, err
	}
	if !sparse {
		m.capacityMu.Lock()
		if m.capacityCache != nil {
			if cached, ok := m.capacityCache[key]; ok {
				if err := workspace.RefreshLFSCacheState(&cached); err != nil {
					m.capacityMu.Unlock()
					return workspace.CapacityEstimate{}, err
				}
				m.capacityCache[key] = cached
				m.capacityMu.Unlock()
				return cached, nil
			}
		}
		m.capacityMu.Unlock()
	}
	estimate, err := preparer.EstimateCapacity(ctx, repo, oid)
	if err != nil {
		return workspace.CapacityEstimate{}, err
	}
	if !estimate.Sparse {
		m.capacityMu.Lock()
		if m.capacityCache == nil {
			m.capacityCache = map[string]workspace.CapacityEstimate{}
		}
		m.capacityCache[key] = estimate
		m.capacityMu.Unlock()
	}
	return estimate, nil
}

func (m *Manager) volumeFreeBytes(file *os.File) (string, int64, error) {
	if m != nil && m.freeSpace != nil {
		return m.freeSpace(file)
	}
	return domain.VolumeFreeBytes(file)
}

// enforcePrepareCapacity は PREPARING/RESTORING の state transition を容量不足
// の書込み前に確定する。測定不能は warn のみで成功扱いにする。
func (m *Manager) enforcePrepareCapacity(ctx context.Context, slot state.Slot, w discovery.Workspace, resolved []pool.Resolved, repos []state.SlotRepository, cfg config.Config) (CapacityReport, error) {
	report, err := m.checkPrepareCapacity(ctx, slot, w, resolved, repos, cfg, 1)
	if err != nil {
		if m.log != nil {
			m.log.Warn("prepare capacity estimate unavailable; continuing without preflight", "slot_id", slot.ID, "error", err)
		}
		return CapacityReport{}, nil
	}
	if report.Sparse {
		if m.log != nil {
			m.log.Warn("sparse checkout makes the capacity estimate non-blocking", "slot_id", slot.ID)
		}
	} else {
		for _, volume := range report.Volumes {
			if volume.Required <= volume.Free {
				continue
			}
			code := "PREPARE_INSUFFICIENT_SPACE"
			if slot.State == "RESTORING" {
				code = "RESTORE_INSUFFICIENT_SPACE"
			}
			if err := m.store.SetSlotState(ctx, slot.ID, []string{slot.State}, "FAILED", code); err != nil {
				return report, err
			}
			return report, &InsufficientPrepareSpaceError{Report: report}
		}
	}
	// report.Repositories には、容量検査を通った repository の LFS 内訳が
	// そのまま残る。sparse checkout でも LFS path の完全性検証は必要なため、
	// 容量不足を非ブロッキングにしたまま cache を修復する。
	preparer := m.newPreparer(cfg, slot)
	preparer.WorkspaceRoot = string(w.Root)
	byID := make(map[string]discovery.Repository, len(resolved))
	for _, item := range resolved {
		byID[string(item.Repository.ID)] = item.Repository
	}
	for _, estimate := range report.Repositories {
		if len(estimate.LFS) == 0 || estimate.MissingLFSObjects == 0 || estimate.RepositoryID == "" {
			continue
		}
		repo, ok := byID[estimate.RepositoryID]
		if !ok {
			continue
		}
		repaired, repairErr := preparer.RepairLFSObjects(ctx, repo, estimate.LFS)
		m.invalidateCapacityCache(repo)
		if repairErr != nil {
			failure := &MissingLFSObjectsError{Report: report, Repository: estimate.RepositoryID, Err: repairErr}
			if err := m.markLFSPreflightFailed(ctx, slot); err != nil {
				return report, err
			}
			return report, failure
		}
		if len(repaired.Unresolved) == 0 {
			continue
		}
		failure := &MissingLFSObjectsError{Report: report, Repository: estimate.RepositoryID, Failures: repaired.Unresolved}
		if err := m.markLFSPreflightFailed(ctx, slot); err != nil {
			return report, err
		}
		return report, failure
	}
	return report, nil
}

func (m *Manager) invalidateCapacityCache(repo discovery.Repository) {
	if m == nil {
		return
	}
	prefix := string(repo.ID) + "\x00"
	// 要求 OID は cache key に含まれるが、修復後の doctor が古い欠落を表示し
	// 続けないことを優先し、同 repository の要求 OID をまとめて無効化する。
	m.capacityMu.Lock()
	for key := range m.capacityCache {
		if strings.HasPrefix(key, prefix) {
			delete(m.capacityCache, key)
		}
	}
	m.capacityMu.Unlock()
}

func (m *Manager) markLFSPreflightFailed(ctx context.Context, slot state.Slot) error {
	code := "PREPARE_LFS_MISSING"
	if slot.State == "RESTORING" {
		code = "RESTORE_LFS_MISSING"
	}
	return m.store.SetSlotState(ctx, slot.ID, []string{slot.State}, "FAILED", code)
}

func lfsObjectsByRepository(report CapacityReport) map[string][]workspace.LFSObjectInfo {
	objects := make(map[string][]workspace.LFSObjectInfo)
	for _, estimate := range report.Repositories {
		if estimate.RepositoryID == "" || len(estimate.LFS) == 0 {
			continue
		}
		objects[estimate.RepositoryID] = append([]workspace.LFSObjectInfo(nil), estimate.LFS...)
	}
	return objects
}

func capacityMul(value int64, multiplier int) int64 {
	if value <= 0 || multiplier <= 0 {
		return 0
	}
	if int64(multiplier) > math.MaxInt64/value {
		return math.MaxInt64
	}
	return value * int64(multiplier)
}

func capacityAdd(left, right int64) int64 {
	if right < 0 || left > int64(^uint64(0)>>1)-right {
		return int64(^uint64(0) >> 1)
	}
	return left + right
}

// prepareCapacityFindings は登録済み workspace の source repository を読むだけで
// 次の slot と warm_count 分の準備容量を報告する。容量不足でも自動処置は行わず、
// doctor の finding だけを返す。
func (m *Manager) prepareCapacityFindings(ctx context.Context) []diag.Finding {
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckPrepareCapacity,
			"the preparation capacity checks could not read registered workspaces",
			message("diag.capacity.workspaces_unreadable"), "", err)}
	}
	findings := make([]diag.Finding, 0, len(roots)+1)
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	for _, root := range roots {
		w, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			findings = append(findings, capacityUncheckedFinding(root, resolveErr))
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, w, nil)
		if resolveErr != nil {
			findings = append(findings, capacityUncheckedFinding(string(w.Root), resolveErr))
			continue
		}
		rootPath, rootID, rootErr := m.activeRoot()
		if rootErr != nil {
			findings = append(findings, capacityUncheckedFinding(string(w.Root), rootErr))
			continue
		}
		// slot pathはroot自身ではなくroot直下の架空slotにする。root自身は
		// rootForPathが「rootの内側」と見なさず、容量検査が必ず未検査になる。
		// 診断はsource repositoryの読取りとstatfsだけを行い、この実体は作らない。
		report, reportErr := m.checkPrepareCapacity(ctx,
			state.Slot{
				ID: capacityProbeSlotID, WorkspaceID: string(w.ID), RootID: rootID,
				RelPath: capacityProbeSlotID, Path: filepath.Join(rootPath, capacityProbeSlotID),
			},
			w, resolved, nil, m.Config(), 1)
		if reportErr != nil {
			findings = append(findings, capacityUncheckedFinding(string(w.Root), reportErr))
			continue
		}
		warmCount, _ := m.Config().WarmCountForWorkspace(string(w.Root))
		findings = append(findings, capacityReportFindings(string(w.Root), report, warmCount)...)
	}
	if len(findings) == 0 {
		findings = append(findings, diag.Finding{
			Check: diag.CheckPrepareCapacity, Severity: diag.SeverityOK,
			Summary:  "no registered workspace needs a preparation capacity check",
			Messages: diag.FindingMessages{Summary: message("diag.capacity.none")},
		})
	}
	return findings
}

func capacityUncheckedFinding(root string, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckPrepareCapacity, Severity: diag.SeverityUnchecked, Target: root,
		Summary: "the preparation capacity could not be estimated", Cause: err.Error(),
		Action:    "fix the reported workspace or repository problem, then run wx doctor again",
		DependsOn: diag.CheckWorktreeRegistration,
		Messages: diag.FindingMessages{
			Summary: message("diag.capacity.unchecked"),
			Action:  message("diag.capacity.action_unchecked"),
		},
	}
}

func capacityReportFindings(root string, report CapacityReport, warmCount int) []diag.Finding {
	findings := make([]diag.Finding, 0, len(report.Volumes))
	for _, volume := range report.Volumes {
		oneRequired := volume.Required
		warmRequired := int64(0)
		if warmCount > 0 {
			warmRequired = capacityMul(volume.WorktreeRequired, warmCount)
		}
		if volume.WorktreeRequired == 0 && volume.SharedRequired == 0 {
			// CapacityVolume を直接組み立てる既存 caller には従来の意味を
			// 保つ。通常の report は上の内訳を必ず持つ。
			warmRequired = capacityMul(oneRequired, warmCount)
		} else if warmCount > 0 {
			warmRequired = capacityAdd(warmRequired, volume.SharedRequired)
		}
		severity := diag.SeverityInfo
		summary, summaryID := "the next worktree fits the available preparation capacity", "diag.capacity.ok"
		cause := fmt.Sprintf("one worktree for %s needs %s and the volume has %s free", root, textfmt.HumanBytes(oneRequired), textfmt.HumanBytes(volume.Free))
		causeID := "diag.capacity.ok_cause"
		causeMessage := message(causeID, "Root", root, "Required", textfmt.HumanBytes(oneRequired), "Free", textfmt.HumanBytes(volume.Free), "Target", volume.Target)
		action := "no action is required"
		actionID := "diag.capacity.action_none"
		actionMessage := message(actionID, "Target", volume.Target)
		switch {
		case !report.Sparse && oneRequired > volume.Free:
			severity = diag.SeverityProblem
			summary, summaryID = "the next worktree cannot be prepared with the available capacity", "diag.capacity.insufficient"
			cause = fmt.Sprintf("one worktree for %s needs %s on %s, but only %s is free", root, textfmt.HumanBytes(oneRequired), volume.Target, textfmt.HumanBytes(volume.Free))
			causeID = "diag.capacity.insufficient_cause"
			causeMessage = message(causeID, "Root", root, "Required", textfmt.HumanBytes(oneRequired), "Free", textfmt.HumanBytes(volume.Free), "Target", volume.Target)
			action = fmt.Sprintf("free space on %s, then retry the worktree preparation", volume.Target)
			actionID = "diag.capacity.action_free"
			actionMessage = message(actionID, "Target", volume.Target)
		case !report.Sparse && warmCount > 0 && warmRequired > volume.Free:
			summary, summaryID = "the configured standby capacity does not fit on this volume", "diag.capacity.warm_short"
			cause = fmt.Sprintf("one worktree needs %s and %d warm slot(s) need %s, while %s has %s free", textfmt.HumanBytes(oneRequired), warmCount, textfmt.HumanBytes(warmRequired), volume.Target, textfmt.HumanBytes(volume.Free))
			causeID = "diag.capacity.warm_cause"
			causeMessage = message(causeID, "One", textfmt.HumanBytes(oneRequired), "Count", warmCount, "Warm", textfmt.HumanBytes(warmRequired), "Target", volume.Target, "Free", textfmt.HumanBytes(volume.Free))
		}
		if report.Sparse {
			// sparse checkout では tree 全体を積んだ値を下限とみなせないため、
			// 不足していても doctor の problem や準備の拒否にはしない。
			severity = diag.SeverityInfo
			summary, summaryID = "preparation capacity was measured but sparse checkout makes it non-blocking", "diag.capacity.sparse"
			cause = fmt.Sprintf("the estimate for %s includes the full tree while sparse checkout may materialize fewer paths", root)
			causeID = "diag.capacity.sparse_cause"
			causeMessage = message(causeID, "Root", root)
		}
		details, detailMessages := capacityDetails(report, oneRequired, warmRequired, warmCount)
		findings = append(findings, diag.Finding{
			Check: diag.CheckPrepareCapacity, Severity: severity, Summary: summary, Target: volume.Target,
			Cause: cause, Action: action, Details: details,
			Messages: diag.FindingMessages{Summary: message(summaryID), Cause: causeMessage, Action: actionMessage, Details: detailMessages},
		})
	}
	return findings
}

func capacityDetails(report CapacityReport, oneRequired, warmRequired int64, warmCount int) ([]string, []i18n.Message) {
	details := []string{
		fmt.Sprintf("one slot: %s", textfmt.HumanBytes(oneRequired)),
		fmt.Sprintf("warm_count %d: %s", warmCount, textfmt.HumanBytes(warmRequired)),
	}
	messages := []i18n.Message{
		message("diag.detail.capacity_one", "Value", textfmt.HumanBytes(oneRequired)),
		message("diag.detail.capacity_warm", "Count", warmCount, "Value", textfmt.HumanBytes(warmRequired)),
	}
	for _, estimate := range report.Repositories {
		details = append(details, fmt.Sprintf("repository: worktree %s, LFS expanded %s, LFS cache %s, missing LFS object(s) %d, CoW avoidable %s",
			textfmt.HumanBytes(estimate.WorktreeBytes), textfmt.HumanBytes(estimate.LFSExpandedBytes), textfmt.HumanBytes(estimate.LFSCacheBytes), estimate.MissingLFSObjects, textfmt.HumanBytes(estimate.COWAvoidableBytes)))
		messages = append(messages, message("diag.detail.capacity_repository", "Worktree", textfmt.HumanBytes(estimate.WorktreeBytes), "LFS", textfmt.HumanBytes(estimate.LFSExpandedBytes), "Cache", textfmt.HumanBytes(estimate.LFSCacheBytes), "Missing", estimate.MissingLFSObjects, "COW", textfmt.HumanBytes(estimate.COWAvoidableBytes)))
	}
	return details, messages
}
