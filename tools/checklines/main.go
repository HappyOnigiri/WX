// checklines はGoの実装ファイルの行数を検査する。
// 分割の判断はレビューではなくこの検査で促し、警告と失敗の2段階で報告する。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// 案内は直し方の強さが警告と失敗で変わるため段階ごとに分け、両者に共通する禁じ手をsharedGuidanceに置く。
// 行数はしきい値の定数から埋める。リテラルで持つと定数を変えたときに案内だけが古い数値を主張する。
func warningGuidance() []string {
	return []string{
		fmt.Sprintf("warning guidance (%d lines or more):", warningLineLimit),
		fmt.Sprintf("- A file with %d or more lines fails make ci.", errorLineLimit),
		"- Prefer splitting now; deferring is acceptable while the file keeps to one responsibility, or while the split is outside the scope of the current change.",
		"- Extract what the current change adds into its own file whenever it stands on its own.",
	}
}

func errorGuidance() []string {
	return []string{
		fmt.Sprintf("error guidance (%d lines or more):", errorLineLimit),
		"- Fix this even when the split reaches beyond the scope of the current change.",
		fmt.Sprintf("- Do not stop just below %d lines; split by responsibility and aim for fewer than %d lines per file.", errorLineLimit, warningLineLimit),
	}
}

func sharedGuidance() []string {
	return []string{
		"- Move whole responsibilities to sibling files in the same package (public API, state transitions, OS/Git adapters, rendering).",
		"- Do not delete comments or pack statements to satisfy the limit.",
		"- See docs/architecture.md for the boundaries.",
	}
}

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

// generatedFile は `// Code generated ... DO NOT EDIT.` を持つ生成物かを判定する。
// 生成物は分割で縮められないため検査対象から外す。構文エラーで判定できないファイルは生成物とみなさず、行数の検査を続ける。
func generatedFile(path string, content []byte) bool {
	file, err := parser.ParseFile(token.NewFileSet(), path, content, parser.PackageClauseOnly|parser.ParseComments)
	if err != nil && file == nil {
		return false
	}
	return ast.IsGenerated(file)
}

// targetFile は検査対象のGo実装ファイルかを判定する。テストコードとsymlinkは対象外とする。
// 生成物は内容を読まないと判定できないため、ここではなくrunでgeneratedFileにより除外する。
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
			if generatedFile(path, content) {
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
	// 案内はファイルごとに繰り返さず、該当した段階の分だけ末尾へ1回出す。
	var guidance []string
	if len(warnings) > 0 {
		guidance = append(guidance, warningGuidance()...)
	}
	if len(violations) > 0 {
		guidance = append(guidance, errorGuidance()...)
	}
	if len(guidance) > 0 {
		for _, line := range append(guidance, sharedGuidance()...) {
			_, _ = fmt.Fprintln(out, line)
		}
	}
	if len(violations)+len(failures) > 0 {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(".", os.Stderr))
}
