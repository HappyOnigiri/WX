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
// skipWorktree と assumeUnchanged は同じ path で両立し得るため、解除と再設定は種別ごとに行う。
// paths は index に載る全 path で、tree 適用後に残っている path だけへ flag を戻すために使う。
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

// flagged は flag が付いた path を index 順・重複なしで返す。
func (f indexFlags) flagged() []string {
	return f.flaggedPaths
}

// retain は、この index に実在する path だけを元の順序で残す。
// update-index は index に無い path を渡すと失敗するため、再設定の前段で使う。
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

// readIndexFlags は worktree の index から stat 比較を抑止する項目を読む。
// 判定できないまま進むと未 snapshot の作業を失うため、呼び出し側は失敗を fail-closed として扱う。
func readIndexFlags(value gitValueFunc) (indexFlags, error) {
	listing, err := value(nil, "ls-files", "-v", "-z")
	if err != nil {
		return indexFlags{}, fmt.Errorf("inspect index stat flags: %w", err)
	}
	return parseIndexFlags(listing), nil
}

// literalPathspecs は path 一覧を `--pathspec-from-file=- --pathspec-file-nul` 向けの入力にする。
// path に glob 文字が含まれ得るため `:(literal)` を前置し、意図しない範囲へ広げない。
func literalPathspecs(paths []string) []byte {
	var builder strings.Builder
	for _, path := range paths {
		builder.WriteString(":(literal)")
		builder.WriteString(path)
		builder.WriteByte(0)
	}
	return []byte(builder.String())
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

// clearIndexFlags は、read-tree が worktree の file を書き換えられるよう flag を外す。
// skip-worktree の項目が残ったままの read-tree は、拒否されるか file を書き換えずに素通りする。
func clearIndexFlags(run gitRunFunc, flags indexFlags) error {
	if len(flags.skipWorktree) > 0 {
		if _, err := run(nil, nulPathList(flags.skipWorktree), "update-index", "--no-skip-worktree", "-z", "--stdin"); err != nil {
			return fmt.Errorf("clear skip-worktree before restore: %w", err)
		}
	}
	if len(flags.assumeUnchanged) > 0 {
		if _, err := run(nil, nulPathList(flags.assumeUnchanged), "update-index", "--no-assume-unchanged", "-z", "--stdin"); err != nil {
			return fmt.Errorf("clear assume-unchanged before restore: %w", err)
		}
	}
	return nil
}

// reapplyIndexFlags は tree 適用後に、採取時と同じ path へ flag を戻す。
// tree が消した path は index に無く update-index が失敗するため、現在の index に残るものだけへ戻す。
func reapplyIndexFlags(run gitRunFunc, value gitValueFunc, flags indexFlags) error {
	if !flags.blinding() {
		return nil
	}
	current, err := readIndexFlags(value)
	if err != nil {
		return err
	}
	if skip := current.retain(flags.skipWorktree); len(skip) > 0 {
		if _, err := run(nil, nulPathList(skip), "update-index", "--skip-worktree", "-z", "--stdin"); err != nil {
			return fmt.Errorf("reapply skip-worktree after restore: %w", err)
		}
	}
	if assume := current.retain(flags.assumeUnchanged); len(assume) > 0 {
		if _, err := run(nil, nulPathList(assume), "update-index", "--assume-unchanged", "-z", "--stdin"); err != nil {
			return fmt.Errorf("reapply assume-unchanged after restore: %w", err)
		}
	}
	return nil
}
