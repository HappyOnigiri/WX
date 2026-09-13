package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// 未保全の理由コード。子の作業の種別を表し、capsule を作る対象の判定と、保存できなかった子の記録に使う。
// 保存できた子は記録から外れるため、残る値は「wx が保存できない条件」を表す。
const (
	// ReasonModified は子の index・worktree に tracked file の差分が残ることを表す。
	ReasonModified = "MODIFIED"
	// ReasonUntracked は子に未追跡 file が残ることを表す。
	ReasonUntracked = "UNTRACKED"
	// ReasonCommitMoved は子の HEAD が親の記録する gitlink と違うことを表す。
	ReasonCommitMoved = "COMMIT_MOVED"
	// ReasonMissingObject は親の index が指す gitlink OID が source のローカル module に無いことを表す。
	// 復元は必ずそのローカル module を clone 元にするため、無ければ復元経路に object が残らない。
	ReasonMissingObject = "MISSING_OBJECT"
	// ReasonUnmergedIndex は子の index に未解消の衝突が残り、index tree を書けなかったことを表す。
	ReasonUnmergedIndex = "UNMERGED_INDEX"
	// ReasonSaveFailed は子の capsule 保存自体が失敗したことを表す。
	ReasonSaveFailed = "SAVE_FAILED"
	// ReasonUndetermined は probe が失敗して判定できなかったことを表す。保護する側へ倒すために記録する。
	ReasonUndetermined = "UNDETERMINED"
)

// unsavedSubmoduleReasonOrder は報告と永続化の並びを固定する理由コードの順序である。
// 検出の経路ごとに順序が変わると、同じ状態でも保存される文字列が変わってしまう。
var unsavedSubmoduleReasonOrder = []string{ReasonModified, ReasonUntracked, ReasonCommitMoved, ReasonMissingObject, ReasonUnmergedIndex, ReasonSaveFailed, ReasonUndetermined}

// UnsavedSubmodule は snapshot が保存できなかった submodule 1 件である。capsule を作れた子はここに残らない。
// Path は slot 内の worktree からの相対 path で、submodule を特定する前に probe が失敗した場合だけ空になる。
type UnsavedSubmodule struct {
	Path    string
	Reasons []string
}

// submoduleWork は親の snapshot だけでは戻らない子の作業を path ごとの理由として返し、列挙できた子も返す。
// 理由が付いた子が capsule の保存対象になり、保存できた子は呼び出し側が理由ごと取り下げる。
// statusOutput は clean 判定と同じ `status --porcelain=v2` の出力で、観測時点をずらさないため再取得しない。
// probe 自体が失敗した場合は判定不能として保護側へ倒し、snapshot は成功させる。親の保存は既に済んでおり、
// ここで失敗させると resume の手段まで失うためである。
// commentlint:allow-long -- probe の失敗を snapshot の失敗へ昇格させない理由を残す
func (m *Manager) submoduleWork(ctx context.Context, repo discovery.Repository, worktree, identity, statusOutput string) (map[string][]string, []workspace.Submodule) {
	reasons := statusSubmoduleReasons(statusOutput)
	modules, err := m.Preparer.Submodules(ctx, worktree, "HEAD", identity)
	if err != nil {
		reasons[""] = append(reasons[""], ReasonUndetermined)
		return reasons, nil
	}
	for path, reason := range m.submoduleObjectReasons(ctx, repo, modules) {
		reasons[path] = append(reasons[path], reason)
	}
	return reasons, modules
}

// statusSubmoduleReasons は porcelain v2 の submodule field から未保全の理由を path ごとに集める。
// 見るのは 3 列目だけで、`N...` は submodule ではない。rename・copy の `2` 行にも同じ field が並ぶため両方を対象にする。
func statusSubmoduleReasons(output string) map[string][]string {
	out := map[string][]string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		var fields []string
		var path string
		switch {
		case strings.HasPrefix(line, "1 "):
			fields = strings.SplitN(line, " ", 9)
			if len(fields) != 9 {
				continue
			}
			path = fields[8]
		case strings.HasPrefix(line, "2 "):
			// rename・copy 行の path 欄は `<path>\t<origPath>` で、現在の配置先は tab の手前にある。
			fields = strings.SplitN(line, " ", 10)
			if len(fields) != 10 {
				continue
			}
			path, _, _ = strings.Cut(fields[9], "\t")
		default:
			continue
		}
		field := fields[2]
		if len(field) != 4 || field[0] != 'S' {
			continue
		}
		for index, reason := range map[int]string{1: ReasonCommitMoved, 2: ReasonModified, 3: ReasonUntracked} {
			if field[index] != '.' {
				out[path] = append(out[path], reason)
			}
		}
	}
	return out
}

// submoduleObjectReasons は親が commit 済みの gitlink OID を、復元が使う source のローカル module で確かめる。
// ローカル module が無い submodule は判定対象から外す。user hook が network から clone する運用では
// それが正常であり、未保全にすると全 slot が恒久的に保護されて回収されなくなる。
func (m *Manager) submoduleObjectReasons(ctx context.Context, repo discovery.Repository, modules []workspace.Submodule) map[string]string {
	out := map[string]string{}
	commonModules := filepath.Join(string(repo.CommonDir), "modules")
	for _, module := range modules {
		source := filepath.Join(commonModules, module.Name)
		if !domain.IsWithin(commonModules, source) {
			continue
		}
		info, statErr := os.Stat(source)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if _, err := m.Git.Run(ctx, source, "--git-dir=.", "cat-file", "-e", module.OID+"^{commit}"); err != nil {
			out[module.Path] = objectProbeReason(err)
		}
	}
	return out
}

// objectProbeReason は `cat-file -e` の失敗を、復元が使えない object と probe 自体の失敗に分ける。
// Git が走って拒んだ場合は、復元の submoduleUpstream が同じ probe で同じ判断をするため不在として扱う。
// object が無いときの終了状態は peel 指定の有無で 1 と 128 に分かれ、終了コードでは不在と壊れた module を見分けられない。
// Git を起動できなかった場合だけ、何も判定できなかったこととして扱う。
// commentlint:allow-long -- 終了コードで不在を見分けない理由を残す
func objectProbeReason(err error) string {
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) {
		return ReasonMissingObject
	}
	return ReasonUndetermined
}

// sortedUnsavedSubmodules は path 順・理由コード順に整えて重複を落とす。
func sortedUnsavedSubmodules(reasons map[string][]string) []UnsavedSubmodule {
	if len(reasons) == 0 {
		return nil
	}
	paths := make([]string, 0, len(reasons))
	for path := range reasons {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]UnsavedSubmodule, 0, len(paths))
	for _, path := range paths {
		seen := map[string]bool{}
		for _, reason := range reasons[path] {
			seen[reason] = true
		}
		ordered := make([]string, 0, len(seen))
		for _, reason := range unsavedSubmoduleReasonOrder {
			if seen[reason] {
				ordered = append(ordered, reason)
			}
		}
		if len(ordered) == 0 {
			continue
		}
		out = append(out, UnsavedSubmodule{Path: path, Reasons: ordered})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
