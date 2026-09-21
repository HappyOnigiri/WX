package workspace

import (
	"os"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// dropUnmaterializedSubmodules は update が実体化しなかった子を origin 復元の対象から外す。
// `submodule.<name>.update=none` のような update policy は `submodule update` を exit 0 のまま子を省略し、
// 子の worktree path を空のまま残す。そこへ `git -C <path> remote set-url origin` を実行すると Git が
// 親 worktree を解決し、source repository の origin を子 URL へ書き換えて不変条件を破る。
// 取り下げた子は decisions へも反映し、post-checkout hook が再初期化しないようにする。
// commentlint:allow-long -- 実体化の確認を省くと source repository の origin が壊れる経緯を残す
func (p *Preparer) dropUnmaterializedSubmodules(repo discovery.Repository, target string, eligible []materializedSubmodule, decisions []submoduleProbe, stats *submoduleStats) []materializedSubmodule {
	kept := eligible[:0]
	for _, module := range eligible {
		if submoduleIsMaterialized(target, module.module.path) {
			kept = append(kept, module)
			continue
		}
		p.logSkip("submodule was not materialized by the repository update policy",
			"repository", string(repo.MainPath), "submodule", module.module.name, "path", module.module.path)
		if module.index >= 0 && module.index < len(decisions) {
			decisions[module.index].eligible = false
			decisions[module.index].skipReason = SubmoduleReasonNotMaterialized
		}
		p.recordSubmoduleOutcome(repo, module.module, SubmoduleActionSkipped, SubmoduleReasonNotMaterialized)
		stats.skipped.Add(1)
	}
	return kept
}

// submoduleIsMaterialized は子の worktree path に子自身の `.git` があるかだけを判定する。
// `.git` を持たない path への `git -C` は親 worktree を解決するため、子への Git 操作はこの確認の後に置く。
func submoduleIsMaterialized(target, path string) bool {
	child := filepath.Join(target, path)
	if !domain.IsWithin(target, child) {
		return false
	}
	_, err := os.Lstat(filepath.Join(child, ".git"))
	return err == nil
}
