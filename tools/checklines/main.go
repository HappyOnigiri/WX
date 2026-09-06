// checklines はGoの実装ファイルの行数を検査する。
// 分割の判断はレビューではなくこの検査で促し、警告と失敗の2段階で報告する。
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 上限は「以上」で判定する。warningLineLimitは報告のみ、errorLineLimitはmake ciを落とす。
const (
	warningLineLimit = 600
	errorLineLimit   = 1000
)

// splitGuidance は違反の直し方を示す。行数を削るのではなく責務で切り出すことを促す。
// commentlint:allow-long -- 出力文面と対応させるため分割しない
const splitGuidance = "split guidance: keep the package and move responsibilities into sibling files (public API, state transitions, OS/Git adapters, rendering); do not split a single responsibility just to fit the limit. See docs/architecture.md for the boundaries."

// countLines は改行で終わらない最終行も1行として数える。
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	lines := strings.Count(string(content), "\n")
	if !strings.HasSuffix(string(content), "\n") {
		lines++
	}
	return lines
}

// targetFile は検査対象のGo実装ファイルかを判定する。テストコードと生成物以外のsymlinkは対象外とする。
func targetFile(path string, entry fs.DirEntry) bool {
	if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
		return false
	}
	return strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
}

func run(root string, out io.Writer) int {
	var warnings, violations, failures []string
	for _, directory := range []string{"cmd", "internal", "tools"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !targetFile(path, entry) {
				return nil
			}
			content, err := os.ReadFile(path)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: read: %v", path, err))
				return nil
			}
			switch lines := countLines(content); {
			case lines >= errorLineLimit:
				violations = append(violations, fmt.Sprintf("%s:1: file-length: %d lines; must be fewer than %d", path, lines, errorLineLimit))
			case lines >= warningLineLimit:
				warnings = append(warnings, fmt.Sprintf("%s:1: file-length: %d lines; warning at %d or more", path, lines, warningLineLimit))
			}
			return nil
		})
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	sort.Strings(warnings)
	sort.Strings(violations)
	sort.Strings(failures)
	for _, line := range append(append(warnings, violations...), failures...) {
		_, _ = fmt.Fprintln(out, line)
	}
	// 案内はファイルごとに繰り返さず、行数の指摘があったときだけ末尾へ1回出す。
	if len(warnings)+len(violations) > 0 {
		_, _ = fmt.Fprintln(out, splitGuidance)
	}
	if len(violations)+len(failures) > 0 {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(".", os.Stderr))
}
