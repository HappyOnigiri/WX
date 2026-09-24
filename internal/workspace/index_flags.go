package workspace

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// GitValueFunc と GitRunFunc は、index flag の読み書きが worktree 内の Git を呼ぶために取る形である。
// どちらも Preparer.RunGitInWorktree に帰着し、env へ GIT_INDEX_FILE を置けば一時 index も対象にできる。
type GitValueFunc func(env []string, args ...string) (string, error)

type GitRunFunc func(env []string, input []byte, args ...string) (gitx.Result, error)

// IndexFlags は `git ls-files -v` が示す、stat 比較を抑止する index 項目をまとめる。
// SkipWorktree と AssumeUnchanged は同じ path で両立し得るため、設定は種別ごとに行う。
// Paths は index に載る全 path で、対象 index に実在する path だけへ flag を立てるために使う。
type IndexFlags struct {
	SkipWorktree    []string
	AssumeUnchanged []string
	FlaggedPaths    []string
	Paths           map[string]struct{}
}

// Blinding は、git status が worktree の変更を隠し得る項目があるかを返す。
func (f IndexFlags) Blinding() bool {
	return len(f.FlaggedPaths) > 0
}

// Retain は、この index に実在する path だけを元の順序で残す。
// update-index は index に無い path を渡すと失敗するため、設定の前段で使う。
func (f IndexFlags) Retain(paths []string) []string {
	kept := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := f.Paths[path]; ok {
			kept = append(kept, path)
		}
	}
	return kept
}

// Has は、指定 path に stat 比較を抑止する flag が立っているかを返す。
func (f IndexFlags) Has(path string) bool {
	return slices.Contains(f.FlaggedPaths, path)
}

// Without は、指定 path の flag を落とした写しを返す。
// 実体を worktree へ書き出した path は、以後 index ではなく worktree の内容が正になるため、
// 採取時の flag を立て直すと内容が HEAD へ巻き戻り、snapshot との照合も合わなくなる。
func (f IndexFlags) Without(paths []string) IndexFlags {
	if len(paths) == 0 {
		return f
	}
	removed := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		removed[path] = struct{}{}
	}
	keep := func(list []string) []string {
		kept := make([]string, 0, len(list))
		for _, path := range list {
			if _, dropped := removed[path]; !dropped {
				kept = append(kept, path)
			}
		}
		return kept
	}
	return IndexFlags{SkipWorktree: keep(f.SkipWorktree), AssumeUnchanged: keep(f.AssumeUnchanged), FlaggedPaths: keep(f.FlaggedPaths), Paths: f.Paths}
}

// ParseIndexFlags は `git ls-files -v -z` の `<tag><SP><path>\0` 列を解析する。
// tag の 'S' は skip-worktree、小文字は assume-unchanged を表し、's' は両方が立っている状態である。
func ParseIndexFlags(listing string) IndexFlags {
	flags := IndexFlags{Paths: map[string]struct{}{}}
	seen := map[string]struct{}{}
	for _, entry := range strings.Split(listing, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		tag, path := entry[0], entry[2:]
		flags.Paths[path] = struct{}{}
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
			flags.SkipWorktree = append(flags.SkipWorktree, path)
		}
		if assume {
			flags.AssumeUnchanged = append(flags.AssumeUnchanged, path)
		}
		flags.FlaggedPaths = append(flags.FlaggedPaths, path)
	}
	return flags
}

// ReadIndexFlags は env が指す index から stat 比較を抑止する項目を読む。
// env が nil なら worktree の index を、GIT_INDEX_FILE を含めば一時 index を見る。
// 判定できないまま進むと flag 付き path の内容を壊し得るため、呼び出し側は失敗を fail-closed として扱う。
func ReadIndexFlags(value GitValueFunc, env []string) (IndexFlags, error) {
	listing, err := value(env, "ls-files", "-v", "-z")
	if err != nil {
		return IndexFlags{}, fmt.Errorf("inspect index stat flags: %w", err)
	}
	return ParseIndexFlags(listing), nil
}

// NULPathList は path 一覧を `update-index -z --stdin` 向けの入力にする。
// update-index の --stdin は pathspec ではなく path をそのまま読むため magic を付けない。
func NULPathList(paths []string) []byte {
	var builder strings.Builder
	for _, path := range paths {
		builder.WriteString(path)
		builder.WriteByte(0)
	}
	return []byte(builder.String())
}

// ApplyIndexFlags は env が指す index へ、渡した flags と同じ path の flag を立てる。
// 対象 index に無い path は update-index が失敗させるため、そこに実在する path だけへ絞る。
func ApplyIndexFlags(run GitRunFunc, value GitValueFunc, env []string, flags IndexFlags) error {
	return updateIndexFlags(run, value, env, flags, "--skip-worktree", "--assume-unchanged", "apply index stat flags")
}

// ClearIndexFlags は env が指す index から、渡した flags と同じ path の flag を落とす。
// force checkout も flag 付き path は更新できないため、その path を書き直す前段で使う。
func ClearIndexFlags(run GitRunFunc, value GitValueFunc, env []string, flags IndexFlags) error {
	return updateIndexFlags(run, value, env, flags, "--no-skip-worktree", "--no-assume-unchanged", "clear index stat flags")
}

func updateIndexFlags(run GitRunFunc, value GitValueFunc, env []string, flags IndexFlags, skipOption, assumeOption, failure string) error {
	if !flags.Blinding() {
		return nil
	}
	current, err := ReadIndexFlags(value, env)
	if err != nil {
		return err
	}
	for _, group := range []struct {
		option string
		paths  []string
	}{
		{option: skipOption, paths: current.Retain(flags.SkipWorktree)},
		{option: assumeOption, paths: current.Retain(flags.AssumeUnchanged)},
	} {
		if len(group.paths) == 0 {
			continue
		}
		if _, err := run(env, NULPathList(group.paths), "update-index", "-z", group.option, "--stdin"); err != nil {
			return fmt.Errorf("%s: %w", failure, err)
		}
	}
	return nil
}

// worktreeIndexGit は worktree 内の Git 起動を、index flag の共有関数が取る形へ包む。
func (p *Preparer) worktreeIndexGit(ctx context.Context, target, identity string) (GitValueFunc, GitRunFunc) {
	value := func(env []string, args ...string) (string, error) {
		result, err := p.RunGitInWorktree(ctx, target, identity, env, nil, args...)
		if err != nil {
			return "", err
		}
		return result.Stdout, nil
	}
	run := func(env []string, input []byte, args ...string) (gitx.Result, error) {
		return p.RunGitInWorktree(ctx, target, identity, env, input, args...)
	}
	return value, run
}
