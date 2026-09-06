package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) reconcileArtifacts(ctx context.Context) {
	if jobs, err := m.store.EnsureRecoveryJobs(ctx); err != nil {
		m.log.Error("reconstruct crash recovery jobs failed", "error", err)
	} else {
		for _, job := range jobs {
			m.schedule(job)
		}
	}
	if artifacts, err := m.store.SlotArtifacts(ctx); err == nil {
		for _, artifact := range artifacts {
			if artifact.State == "ALLOCATING" || artifact.State == "REGISTERING" {
				// 自プロセスで進行中の予約は中断された確保ではない。隔離すると確保側の CAS が落ち、正常な起動が失敗する。
				if m.reservationInFlight(artifact.ID) {
					continue
				}
				code := "STANDBY_ALLOCATION_INTERRUPTED"
				if slot, slotErr := m.store.Slot(ctx, artifact.ID); slotErr == nil && slot.OwnerSessionID != "" {
					code = "ALLOCATION_INTERRUPTED"
				}
				if quarantineErr := m.store.QuarantineReservedSlot(ctx, artifact.ID, code); quarantineErr != nil {
					m.log.Warn("slot reservation changed before reconciliation", "slot_id", artifact.ID, "error", quarantineErr)
				}
				continue
			}
			if artifact.State == "ARCHIVED" || artifact.State == "REMOVING" || artifact.State == "PREPARING" || artifact.State == "QUARANTINED" {
				continue
			}
			exists, statErr := m.ownedPathExists(artifact.Path)
			if statErr != nil {
				m.log.Warn("skip artifact path reconciliation without root ownership proof", "slot_id", artifact.ID, "path", artifact.Path, "error", statErr)
				continue
			}
			if !exists {
				if err := m.store.QuarantineMissingSlot(ctx, artifact.ID, "OWNED_PATH_MISSING"); err != nil {
					m.log.Error("quarantine missing owned path failed", "slot_id", artifact.ID, "error", err)
				}
			}
		}
	}
	diagnostics := m.artifactDiagnostics(ctx)
	// unknown_refs は現行 DB が説明しない孤児 ref で、`wx prune` で解消できる。件数が多く毎周同じ内容になるため、
	// per-item ではなくリポジトリ単位に束ね、そのリポジトリに新規記録があるときだけ 1 行出す。
	newOrphans := map[string]bool{}
	orphansByRepository := map[string]int{}
	present := map[string]bool{}
	for _, category := range []string{"unknown_paths", "missing_paths", "unknown_refs", "mismatched_refs", "missing_refs", "errors"} {
		items, _ := diagnostics[category].([]string)
		for _, item := range items {
			switch category {
			case "unknown_paths", "mismatched_refs":
				present[item] = true
				if _, err := m.store.QuarantineArtifact(ctx, category, item, artifactQuarantineReasons[category]); err != nil {
					m.log.Error("record quarantined artifact failed", "category", category, "artifact", item, "error", err)
				}
			case "unknown_refs":
				present[item] = true
				repositoryID, _, _ := strings.Cut(item, ":")
				orphansByRepository[repositoryID]++
				inserted, err := m.store.QuarantineArtifact(ctx, category, item, artifactQuarantineReasons[category])
				if err != nil {
					m.log.Error("record quarantined artifact failed", "category", category, "artifact", item, "error", err)
				}
				if inserted {
					newOrphans[repositoryID] = true
				}
				continue
			case "missing_refs":
				if _, ref, ok := strings.Cut(item, ":"); ok {
					_ = m.store.QuarantineMissingRecoveryRef(ctx, ref)
				}
			}
			m.log.Warn("artifact quarantined for manual inspection", "category", category, "artifact", item)
		}
	}
	for repositoryID := range newOrphans {
		m.log.Warn("recovery refs are not explained by the current database; run wx prune to remove them",
			"category", "unknown_refs", "repository", repositoryID, "refs", orphansByRepository[repositoryID])
	}
	// 再検出され続けるカテゴリだけを刈る。standby_slot・workspace_snapshot は reconcile が再検出しないため対象にしない。
	if err := m.store.PruneQuarantinedArtifacts(ctx, []string{"unknown_paths", "unknown_refs", "mismatched_refs"}, present); err != nil {
		m.log.Error("prune resolved quarantine records failed", "error", err)
	}
}

// artifactQuarantineReasons は quarantined_artifacts.reason をカテゴリごとに分ける。
// 所有権証明そのものが失敗した経路（standby_slot など）とは別の文言にし、`wx status` で束ねたときに区別できるようにする。
var artifactQuarantineReasons = map[string]string{
	"unknown_paths":   "path is not registered in the current database",
	"unknown_refs":    "recovery ref is not explained by the current database",
	"mismatched_refs": "recovery ref points at an object the current database does not expect",
}

func (m *Manager) ownedPathExists(path string) (bool, error) {
	// 置換・消失したhistorical rootは通常の欠損ではなく所有権エラーとして扱う。
	root, ok := m.rootForPath(path)
	if !ok {
		return false, fmt.Errorf("%w: path is outside known wx roots", state.ErrOwnership)
	}
	release, err := m.holdRootForPath(path)
	if err != nil {
		return false, err
	}
	defer release()
	owner := m.rootHandleForPath(path)
	if owner == nil {
		return false, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return false, err
	}
	relative, ok := relativeWithinRoot(root, path)
	if !ok {
		return false, fmt.Errorf("%w: path is outside root", state.ErrOwnership)
	}
	_, err = owner.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: inspect owned path: %w", state.ErrOwnership, err)
	}
	return true, nil
}

func (m *Manager) rootPathsFromStore(ctx context.Context) ([]string, error) {
	// Storeを読めないdegraded状態ではlive descriptor集合へfallbackし、診断を空にしない。
	rows, err := m.store.Roots(ctx)
	if err == nil {
		paths := make([]string, 0, len(rows))
		for _, row := range rows {
			paths = append(paths, filepath.Clean(row.Path))
		}
		return paths, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	paths := make([]string, 0, len(m.roots))
	for root := range m.roots {
		paths = append(paths, root)
	}
	return paths, err
}

func (m *Manager) ownedRootArtifactPaths(root string) ([]string, error) {
	// 列挙単位は<workspace-id>/<slot-id>または_unbound/<slot-id>のslot directoryである。
	// configurable root内の無関係なdirectoryをartifactと誤認しないよう、namespaceの形も検証する。
	root = filepath.Clean(root)
	m.mu.RLock()
	active, known := m.roots[root]
	m.mu.RUnlock()
	var release func()
	var err error
	if known && active {
		_, release, err = m.rootDescriptor(root)
	} else {
		_, release, err = m.existingRootDescriptor(root)
	}
	if err != nil {
		return nil, err
	}
	defer release()
	owner := m.rootHandleForRoot(root)
	if owner == nil {
		return nil, fmt.Errorf("%w: root descriptor is unavailable", state.ErrOwnership)
	}
	if err := verifyRootDescriptorPath(root, owner); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(owner.FS(), ".")
	if errors.Is(err, os.ErrNotExist) {
		entries = nil
	} else if err != nil {
		return nil, fmt.Errorf("%w: inspect wx root namespace: %w", state.ErrOwnership, err)
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		if entry.Name() != unboundNamespace && !domain.ValidShortID(entry.Name()) {
			continue
		}
		slotPaths, readErr := ownedSlotDirectories(owner, root, entry.Name())
		if readErr != nil {
			return nil, readErr
		}
		paths = append(paths, slotPaths...)
	}
	return paths, nil
}

func ownedSlotDirectories(owner *os.Root, root, namespace string) ([]string, error) {
	slots, err := fs.ReadDir(owner.FS(), namespace)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect workspace slots: %w", state.ErrOwnership, err)
	}
	paths := make([]string, 0, len(slots))
	for _, slotEntry := range slots {
		if slotEntry.Type()&os.ModeSymlink != 0 || !slotEntry.IsDir() {
			continue
		}
		info, infoErr := owner.Lstat(filepath.FromSlash(path.Join(namespace, slotEntry.Name())))
		if errors.Is(infoErr, os.ErrNotExist) {
			continue
		}
		if infoErr != nil {
			return nil, fmt.Errorf("%w: inspect slot directory: %w", state.ErrOwnership, infoErr)
		}
		if info.IsDir() {
			paths = append(paths, filepath.Join(root, namespace, slotEntry.Name()))
		}
	}
	return paths, nil
}
