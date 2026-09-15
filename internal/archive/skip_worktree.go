package archive

import (
	"fmt"

	"github.com/HappyOnigiri/WX/internal/workspace"
)

// gitValueFunc と gitRunFunc は、snapshot と restore が worktree 内の Git を呼ぶために閉じ込めた関数である。
// どちらも Preparer.RunGitInWorktree に帰着し、この package から他の経路で Git を起動しない。
// index flag の読み書きは standby の更新と共有するため、workspace 側の定義の別名にしている。
type gitValueFunc = workspace.GitValueFunc

type gitRunFunc = workspace.GitRunFunc

// indexFlags と各操作は workspace と共有する。restore 固有の materializeSkipped だけがこの package に残る。
type indexFlags = workspace.IndexFlags

var (
	readIndexFlags  = workspace.ReadIndexFlags
	applyIndexFlags = workspace.ApplyIndexFlags
	nulPathList     = workspace.NULPathList
)

// materializeSkipped は、渡した path のうち index で skip-worktree が立つものを worktree へ書き出し、書き出した path を返す。
// sparse 範囲外で行った作業を restore で戻すために使い、内容は呼び出し時点の index から取る。
// checkout-index は skip-worktree 付きの path を渡されると、それ以前の path を書き出したうえで失敗するため先に flag を外す。
// commentlint:allow-long -- flag を外してから書き出す順序が入れ替えられない理由を説明する
func materializeSkipped(run gitRunFunc, value gitValueFunc, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	current, err := readIndexFlags(value, nil)
	if err != nil {
		return nil, err
	}
	skipped := make(map[string]struct{}, len(current.SkipWorktree))
	for _, path := range current.SkipWorktree {
		skipped[path] = struct{}{}
	}
	targets := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := skipped[path]; ok {
			targets = append(targets, path)
		}
	}
	if len(targets) == 0 {
		return nil, nil
	}
	if _, err := run(nil, nulPathList(targets), "update-index", "-z", "--no-skip-worktree", "--stdin"); err != nil {
		return nil, fmt.Errorf("clear skip-worktree before materializing: %w", err)
	}
	if _, err := run(nil, nulPathList(targets), "checkout-index", "-f", "-z", "--stdin"); err != nil {
		return nil, fmt.Errorf("materialize paths outside the sparse cone: %w", err)
	}
	return targets, nil
}
