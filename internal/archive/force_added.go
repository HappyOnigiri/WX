package archive

import (
	"fmt"
	"sort"
	"strings"
)

// forceAddWorktreeArgs は、一時 index へ取りこぼした path を明示指定で追加する add の引数を返す。
// pathspec は `--pathspec-file-nul` で stdin から literal に渡す。path に magic と解釈され得る文字が
// あっても、そのままの名前として扱わせるためである。
func forceAddWorktreeArgs() []string {
	return []string{"add", "--sparse", "-f", "--pathspec-from-file=-", "--pathspec-file-nul"}
}

// addWorktreeContents は env が指す一時 index へ worktree の現状を取り込む。
// `git add -A` は ignore 規則に一致する path を飛ばすため、これだけでは利用者が `git add -f` で
// 実 index へ入れた ignored file の worktree 内容が tree から丸ごと落ちる。
// 実 index に載るのに取り込めなかった path を洗い出し、`-f` で追加し直してその穴を塞ぐ。
// 対象を実 index の登録済み path に限るので、無視されたままの未追跡 file は従来どおり保存しない。
// commentlint:allow-long -- ignored file を取りこぼす理由と、対象を実 index に限る理由を残す
func addWorktreeContents(value gitValueFunc, run gitRunFunc, env []string) error {
	if _, err := run(env, nil, addWorktreeArgs()...); err != nil {
		return err
	}
	missing, err := missingIndexPaths(value, env)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := run(env, nulPathList(missing), forceAddWorktreeArgs()...); err != nil {
		return fmt.Errorf("add force-added ignored paths: %w", err)
	}
	return nil
}

// missingIndexPaths は、実 index に載るのに env の一時 index へ取り込めなかった path を昇順で返す。
// worktree から消えている path は tree に載せる内容が無いので除く。`add -A` は実体の無い path を
// 追加できず、pathspec が一致しないとして add 全体を失敗させるためでもある。
func missingIndexPaths(value gitValueFunc, env []string) ([]string, error) {
	tracked, err := readIndexFlags(value, nil)
	if err != nil {
		return nil, err
	}
	staged, err := readIndexFlags(value, env)
	if err != nil {
		return nil, err
	}
	listing, err := value(nil, "ls-files", "-z", "--deleted")
	if err != nil {
		return nil, fmt.Errorf("list index paths missing from the working tree: %w", err)
	}
	deleted := nulPathSet(listing)
	missing := make([]string, 0, len(tracked.Paths))
	for path := range tracked.Paths {
		if _, present := staged.Paths[path]; present {
			continue
		}
		if _, gone := deleted[path]; gone {
			continue
		}
		missing = append(missing, path)
	}
	sort.Strings(missing)
	return missing, nil
}

// nulPathSet は NUL 区切りの path 一覧を集合にする。末尾の区切りが生む空要素は捨てる。
func nulPathSet(listing string) map[string]struct{} {
	paths := map[string]struct{}{}
	for _, path := range strings.Split(listing, "\x00") {
		if path == "" {
			continue
		}
		paths[path] = struct{}{}
	}
	return paths
}
