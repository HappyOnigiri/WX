package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// mismatchPathLimit はログへ載せる path の最大件数。残りは件数だけを添える。
const mismatchPathLimit = 5

// readyMismatch は READY 候補が完全一致しなかった理由である。
// reason は分類、detail は対象を特定する文字列で、どちらもログにだけ載せる。
type readyMismatch struct {
	reason string
	detail string
}

// logArgs はログの key-value 引数を返す。理由が無いときは何も足さない。
func (r readyMismatch) logArgs() []any {
	if r.reason == "" {
		return nil
	}
	args := []any{"mismatch", r.reason}
	if r.detail != "" {
		args = append(args, "mismatch_detail", r.detail)
	}
	return args
}

// describeReadyMismatch は readyMatches が完全一致を否定した理由を組み立てる。
// UPDATE や cold start へ落ちた後だけ呼ぶ。include の内容 hash と Git 起動を伴うため、完全一致の経路には置かない。
// 診断の失敗は貸出へ伝播させず、reason に unknown を入れて detail に原因を書く。
func (m *Manager) describeReadyMismatch(ctx context.Context, slot state.Slot, resolved []pool.Resolved) readyMismatch {
	repositories, err := m.store.SlotRepositories(ctx, slot.ID)
	if err != nil {
		return readyMismatch{reason: "unknown", detail: err.Error()}
	}
	if len(repositories) != len(resolved) {
		return readyMismatch{reason: "repository_set", detail: fmt.Sprintf("stored=%d requested=%d", len(repositories), len(resolved))}
	}
	stored := make(map[string]state.SlotRepository, len(repositories))
	for _, repository := range repositories {
		stored[repository.RepositoryID] = repository
	}
	preparer := m.newPreparer(m.Config(), slot)
	for _, requested := range resolved {
		row, ok := stored[string(requested.Repository.ID)]
		if !ok {
			return readyMismatch{reason: "repository_set", detail: "repository " + string(requested.Repository.ID) + " is not registered to the slot"}
		}
		if row.State != "READY" && row.State != "COLD" {
			return readyMismatch{reason: "repository_state", detail: row.DirName + ": " + row.State}
		}
		if row.BaseOID != requested.OID {
			return readyMismatch{reason: "oid", detail: fmt.Sprintf("%s: %s -> %s", row.DirName, shortOID(row.BaseOID), shortOID(requested.OID))}
		}
		fingerprint, err := workspace.Fingerprint(slot.Generation, requested.OID, requested.Repository, m.Config())
		if err != nil {
			return readyMismatch{reason: "unknown", detail: err.Error()}
		}
		if fingerprint != row.Fingerprint {
			return readyMismatch{reason: "fingerprint", detail: m.describeFingerprintDrift(ctx, preparer, slot, row, requested)}
		}
	}
	// repository row が全て一致するなら、実体側の検査で落ちている。理由の文面は検査の失敗をそのまま使う。
	for _, requested := range resolved {
		row := stored[string(requested.Repository.ID)]
		if row.State != "READY" {
			continue
		}
		if err := preparer.ValidateReady(ctx, requested.Repository, row.WorktreePath, requested.OID); err != nil {
			return readyMismatch{reason: "worktree", detail: row.DirName + ": " + err.Error()}
		}
	}
	return readyMismatch{reason: "unknown", detail: "no stored condition differed; the candidate was likely taken by a concurrent lease"}
}

// describeFingerprintDrift は fingerprint の差を include/link の配置差として説明する。
// 配置差が無い場合は、manifest やコピー設定のような配置に現れない入力が変わったことを示す。
func (m *Manager) describeFingerprintDrift(ctx context.Context, preparer *workspace.Preparer, slot state.Slot, row state.SlotRepository, requested pool.Resolved) string {
	planned, err := preparer.RepositoryPlacements(ctx, requested.Repository, requested.OID)
	if err != nil {
		return row.DirName + ": placement plan failed: " + err.Error()
	}
	previous, err := m.store.Placements(ctx, slot.ID)
	if err != nil {
		return row.DirName + ": " + err.Error()
	}
	drift := placementDrift(placementsFor(previous, row.RepositoryID), planned)
	if drift == "" {
		return row.DirName + ": include placements are identical; a manifest, copy mode, or CoW threshold input changed"
	}
	return row.DirName + ": " + drift
}

// placementDrift は記録済みの配置と新しい計画の差を、path の一覧として返す。
// 接頭辞は追加が+、削除が-、内容や配置方式の変更が~である。差が無ければ空文字列を返す。
func placementDrift(previous, planned []state.Placement) string {
	previousByPath := make(map[string]state.Placement, len(previous))
	for _, placement := range previous {
		previousByPath[placement.RelativePath] = placement
	}
	plannedByPath := make(map[string]state.Placement, len(planned))
	for _, placement := range planned {
		plannedByPath[placement.RelativePath] = placement
	}
	var changes []string
	for path, placement := range plannedByPath {
		old, ok := previousByPath[path]
		switch {
		case !ok:
			changes = append(changes, "+"+path)
		case !old.SameSource(placement):
			changes = append(changes, "~"+path)
		}
	}
	for path := range previousByPath {
		if _, ok := plannedByPath[path]; !ok {
			changes = append(changes, "-"+path)
		}
	}
	if len(changes) == 0 {
		return ""
	}
	sort.Strings(changes)
	if len(changes) <= mismatchPathLimit {
		return strings.Join(changes, " ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(changes[:mismatchPathLimit], " "), len(changes)-mismatchPathLimit)
}

// cowThresholdSummary は workspace の各 repository へ効いている CoW 共有下限を返す。
// 更新互換fingerprintは repository ごとに解決した下限を含むため、workspace root の値で代表させない。
func cowThresholdSummary(c config.Config, w discovery.Workspace) string {
	parts := make([]string, 0, len(w.Repositories))
	for _, repository := range w.Repositories {
		root := repositoryWorkspaceRootForLease(repository)
		parts = append(parts, fmt.Sprintf("%s=%d", filepath.Base(string(repository.MainPath)), c.COWMinSizeKiBForWorkspaceRepository(root, repository.RelativePath, string(repository.MainPath))))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// copyModeSummary は workspace の各 repository へ効いているコピー方式を返す。
// 更新互換fingerprintは repository ごとに解決した方式を含むため、global の値で代表させない。
func copyModeSummary(c config.Config, w discovery.Workspace) string {
	parts := make([]string, 0, len(w.Repositories))
	for _, repository := range w.Repositories {
		root := repositoryWorkspaceRootForLease(repository)
		parts = append(parts, fmt.Sprintf("%s=%s", filepath.Base(string(repository.MainPath)), c.CopyModeForWorkspaceRepository(root, repository.RelativePath, string(repository.MainPath))))
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}

// shortOID はログ用にOIDを短縮する。空文字列と短い値はそのまま返す。
func shortOID(oid string) string {
	if len(oid) <= 12 {
		return oid
	}
	return oid[:12]
}

// updateMismatch は更新予約時に、判定に使った値から不一致の理由を返す。
// 予約後は slot_repositories が新しい値へ入れ替わり差を復元できないため、比較した場所で組み立てる。
// 一致していた repository には空の理由を返し、呼び出し側が最初にずれた repository だけを記録できるようにする。
func updateMismatch(stored state.SlotRepository, requested pool.Resolved, fingerprint string, previous, planned []state.Placement) readyMismatch {
	if stored.BaseOID != requested.OID {
		detail := fmt.Sprintf("%s: %s -> %s", stored.DirName, shortOID(stored.BaseOID), shortOID(requested.OID))
		if drift := placementDrift(previous, planned); drift != "" {
			detail += " / " + drift
		}
		return readyMismatch{reason: "oid", detail: detail}
	}
	if fingerprint == stored.Fingerprint {
		return readyMismatch{}
	}
	drift := placementDrift(previous, planned)
	if drift == "" {
		return readyMismatch{reason: "fingerprint", detail: stored.DirName + ": include placements are identical; a manifest, copy mode, or CoW threshold input changed"}
	}
	return readyMismatch{reason: "fingerprint", detail: stored.DirName + ": " + drift}
}
