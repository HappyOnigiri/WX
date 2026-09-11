package archive

import (
	"fmt"
	"strings"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

// gitValueFunc と gitRunFunc は、snapshot と restore が worktree 内の Git を呼ぶために閉じ込めた関数である。
// どちらも Preparer.RunGitInWorktree に帰着し、この package から他の経路で Git を起動しない。
type gitValueFunc func(env []string, args ...string) (string, error)

type gitRunFunc func(env []string, input []byte, args ...string) (gitx.Result, error)

// indexFlags は `git ls-files -v` が示す、stat 比較を抑止する index 項目をまとめる。
// skipWorktree と assumeUnchanged は同じ path で両立し得るため、設定は種別ごとに行う。
// paths は index に載る全 path で、対象 index に実在する path だけへ flag を立てるために使う。
type indexFlags struct {
	skipWorktree    []string
	assumeUnchanged []string
	flaggedPaths    []string
	paths           map[string]struct{}
}

// blinding は、git status が worktree の変更を隠し得る項目があるかを返す。
func (f indexFlags) blinding() bool {
	return len(f.flaggedPaths) > 0
}

// retain は、この index に実在する path だけを元の順序で残す。
// update-index は index に無い path を渡すと失敗するため、設定の前段で使う。
func (f indexFlags) retain(paths []string) []string {
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := f.paths[path]; ok {
			kept = append(kept, path)
		}
	}
	return kept
}

// parseIndexFlags は `git ls-files -v -z` の `<tag><SP><path>\0` 列を解析する。
// tag の 'S' は skip-worktree、小文字は assume-unchanged を表し、's' は両方が立っている状態である。
func parseIndexFlags(listing string) indexFlags {
	flags := indexFlags{paths: map[string]struct{}{}}
	seen := map[string]struct{}{}
	for _, entry := range strings.Split(listing, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		tag, path := entry[0], entry[2:]
		flags.paths[path] = struct{}{}
		skip := tag == 'S' || tag == 's'
		assume := tag >= 'a' && tag <= 'z'
		if !skip && !assume {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		if skip {
			flags.skipWorktree = append(flags.skipWorktree, path)
		}
		if assume {
			flags.assumeUnchanged = append(flags.assumeUnchanged, path)
		}
		flags.flaggedPaths = append(flags.flaggedPaths, path)
	}
	return flags
}

// readIndexFlags は env が指す index から stat 比較を抑止する項目を読む。
// env が nil なら worktree の index を、GIT_INDEX_FILE を含めば snapshot 用の一時 index を見る。
// 判定できないまま進むと flag 付き path の個人版を記録し得るため、呼び出し側は失敗を fail-closed として扱う。
func readIndexFlags(value gitValueFunc, env []string) (indexFlags, error) {
	listing, err := value(env, "ls-files", "-v", "-z")
	if err != nil {
		return indexFlags{}, fmt.Errorf("inspect index stat flags: %w", err)
	}
	return parseIndexFlags(listing), nil
}

// nulPathList は path 一覧を `update-index -z --stdin` 向けの入力にする。
// update-index の --stdin は pathspec ではなく path をそのまま読むため magic を付けない。
func nulPathList(paths []string) []byte {
	var builder strings.Builder
	for _, path := range paths {
		builder.WriteString(path)
		builder.WriteByte(0)
	}
	return []byte(builder.String())
}

// applyIndexFlags は env が指す index へ、採取時と同じ path の flag を立てる。
// snapshot は一時 index を元 index と同じ盲目度にするために、restore は read-tree が落とした flag を戻すために使う。
// 対象 index に無い path は update-index が失敗させるため、そこに実在する path だけへ絞る。
func applyIndexFlags(run gitRunFunc, value gitValueFunc, env []string, flags indexFlags) error {
	if !flags.blinding() {
		return nil
	}
	current, err := readIndexFlags(value, env)
	if err != nil {
		return err
	}
	for _, group := range []struct {
		option string
		paths  []string
	}{
		{option: "--skip-worktree", paths: current.retain(flags.skipWorktree)},
		{option: "--assume-unchanged", paths: current.retain(flags.assumeUnchanged)},
	} {
		if len(group.paths) == 0 {
			continue
		}
		if _, err := run(env, nulPathList(group.paths), "update-index", "-z", group.option, "--stdin"); err != nil {
			return fmt.Errorf("apply index stat flags: %w", err)
		}
	}
	return nil
}
