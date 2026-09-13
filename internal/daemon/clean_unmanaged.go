package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// CleanUnmanaged は wx の予約 namespace 配下にありながら DB が説明しない実体を、利用者の明示指定で削除する。
// 登録済み slot の削除権限を DB の登録だけで決める規則とは別の経路である。ここでの対象は登録が無いこと自体が条件で、
// slot として採用（DB への登録・reconcile や GC の対象化）は一切行わず、`wx gc` や reconcile からは呼ばない。
// clean run（clean_runs・clean_targets）には載せない。clean_targets は slot ID を要求し、登録外の実体は持たないためである。
// commentlint:allow-long -- 既存の削除権限の規則との違いと、run に載せない理由を入口へ 1 か所で残す
func (m *Manager) CleanUnmanaged(ctx context.Context, dryRun bool) (map[string]any, error) {
	expected, err := m.unmanagedExpectationSet(ctx)
	if err != nil {
		return nil, err
	}
	roots, rootsErr := m.rootPathsFromStore(ctx)
	errs := []string{}
	if rootsErr != nil {
		errs = append(errs, fmt.Sprintf("list worktree root generations: %v", rootsErr))
	}
	targets, removed := []unmanagedTarget{}, 0
	for _, root := range roots {
		found, count, err := m.cleanUnmanagedRoot(ctx, root, expected, dryRun)
		if err != nil {
			errs = append(errs, fmt.Sprintf("inspect root %s: %v", root, err))
			continue
		}
		targets = append(targets, found...)
		removed += count
	}
	sortUnmanagedTargets(targets)
	if removed > 0 {
		// 登録外の分は slot 別の実測に載らないので差し引きでは合わせられない。root 合計を測り直して追随させる。
		m.remeasureRootUsage()
	}
	return map[string]any{
		"dry_run": dryRun, "targets": targets, "summary": unmanagedSummary(targets), "errors": errs,
	}, nil
}

// unmanagedTarget は CleanUnmanaged が返す対象 1 件である。
// State の語彙は clean_targets と揃え、CLI が同じ描画を使えるようにする。
type unmanagedTarget struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// cleanUnmanagedRoot は root 世代 1 つを pin し、その descriptor の上で列挙と削除を続けて行う。
// 列挙と削除で pin を取り直すと、その間の置換を検出できないためである。
func (m *Manager) cleanUnmanagedRoot(ctx context.Context, root string, expected unmanagedExpectations, dryRun bool) ([]unmanagedTarget, int, error) {
	root = filepath.Clean(root)
	targets, removed := []unmanagedTarget{}, 0
	err := m.withVerifiedRoot(root, func(owner *os.Root) error {
		artifacts, listErr := unmanagedArtifactsAt(owner, root, expected)
		if listErr != nil {
			return listErr
		}
		for _, artifact := range artifacts {
			target := unmanagedTarget{Path: artifact.Path, Kind: string(artifact.Kind), State: cleanTargetPending}
			if !dryRun {
				target.State, target.Reason = m.removeUnmanagedArtifact(ctx, owner, artifact)
				if target.State == cleanTargetDone {
					removed++
				}
			}
			targets = append(targets, target)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return targets, removed, nil
}

// removeUnmanagedArtifact は対象 1 件を pin 済み descriptor の配下で削除し、結果の状態と理由を返す。
// slot directory は削除の直前に同じ slot lock を取り、取得後に登録が増えていないことを確かめる。
// 並走する準備が同じ path を作り始めていれば skip し、書き込み途中の worktree を消さない。
func (m *Manager) removeUnmanagedArtifact(ctx context.Context, owner *os.Root, artifact unmanagedArtifact) (string, string) {
	if artifact.Kind == unmanagedSlotDirectory {
		rootID := m.rootGenerationID(artifact.Root)
		if rootID == "" {
			return cleanTargetFailed, fmt.Sprintf("worktree root %s has no registered generation", artifact.Root)
		}
		_, release, err := m.slotLocks.Acquire(ctx, rootID+"\x00"+filepath.Clean(artifact.RelPath))
		if err != nil {
			return cleanTargetFailed, err.Error()
		}
		defer release()
		if registered, err := m.registeredAfterLock(ctx, artifact); err != nil {
			return cleanTargetFailed, err.Error()
		} else if registered {
			return cleanTargetSkipped, "a slot was registered at this path while wx was preparing to delete it"
		}
	}
	if err := removeOwnedPath(owner, artifact.RelPath); err != nil {
		return cleanTargetFailed, err.Error()
	}
	return cleanTargetDone, ""
}

// registeredAfterLock は lock 取得後に DB を引き直し、対象の path が登録済みになっていないかを返す。
func (m *Manager) registeredAfterLock(ctx context.Context, artifact unmanagedArtifact) (bool, error) {
	artifacts, err := m.store.SlotArtifacts(ctx)
	if err != nil {
		return false, err
	}
	_, registered := expectedSlotPaths(artifacts)[filepath.Clean(artifact.Path)]
	return registered, nil
}

// rootGenerationID は root 世代の登録 ID を返す。登録が読めない root では空を返す。
func (m *Manager) rootGenerationID(root string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rootIDs[filepath.Clean(root)]
}

// unmanagedSummary は状態別の件数を返す。CLI の表示と終了コードの根拠になる。
func unmanagedSummary(targets []unmanagedTarget) map[string]int {
	summary := map[string]int{"total": len(targets)}
	for _, key := range []string{cleanTargetPending, cleanTargetDone, cleanTargetFailed, cleanTargetSkipped} {
		summary[key] = 0
	}
	for _, target := range targets {
		summary[target.State]++
	}
	return summary
}

func sortUnmanagedTargets(targets []unmanagedTarget) {
	sort.Slice(targets, func(i, j int) bool { return targets[i].Path < targets[j].Path })
}
