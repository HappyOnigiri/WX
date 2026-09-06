// checktestlayout はGo実装ファイルに、その名前を継いだテストファイルがあるかを検査する。
// 分割リファクタでテストが元のファイル名に取り残されるのを、レビューではなくこの検査で防ぐ。
package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const exclusionsFileName = "testlayout-exclusions.txt"

// 免除の種別。exemptは恒久的に対象外とし、backlogは未解消の残務として警告だけを出す。
// 残務を消すにはテストを移してエントリを削除する。登録のない欠落だけがmake ciを落とす。
const (
	kindExempt  = "exempt"
	kindBacklog = "backlog"
)

// テストファイル名は観点の接尾辞を許し、`<実装ファイル名>[_<観点>]_test.go` を最長一致で実装ファイルへ対応づける。
// GOOS/GOARCH別のファイルは基のファイル側のテストで検査するため対象外とする。
var (
	generatedHeader = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)
	buildSuffix     = map[string]bool{
		"aix": true, "android": true, "darwin": true, "dragonfly": true, "freebsd": true, "illumos": true,
		"ios": true, "js": true, "linux": true, "netbsd": true, "openbsd": true, "plan9": true, "solaris": true,
		"wasip1": true, "windows": true, "386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
		"mips": true, "mips64": true, "ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true, "wasm": true,
	}
)

var guidance = []string{
	"test-layout guidance:",
	"- Name a test file after the implementation file it exercises: <impl>_test.go, or <impl>_<aspect>_test.go.",
	"- When you split an implementation file, move its tests into test files named after the new files.",
	fmt.Sprintf("- Record an exception in %s as `<path><TAB>%s|%s<TAB><reason>`; the reason is required.", exclusionsFileName, kindExempt, kindBacklog),
	fmt.Sprintf("- %s entries stay reported as warnings; clear one by moving the tests and deleting its line.", kindBacklog),
}

// loadExclusions は種別と理由付きの免除一覧を読む。理由のない行はエラーにして、説明のない免除を防ぐ。
func loadExclusions(root string) (map[string]string, error) {
	file, err := os.Open(filepath.Join(root, exclusionsFileName))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	kinds := map[string]string{}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		fields := strings.SplitN(text, "\t", 3)
		if len(fields) != 3 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[2]) == "" {
			return nil, fmt.Errorf("%s:%d: every entry needs a path, a kind and a reason separated by tabs", exclusionsFileName, line)
		}
		kind := strings.TrimSpace(fields[1])
		if kind != kindExempt && kind != kindBacklog {
			return nil, fmt.Errorf("%s:%d: kind must be %s or %s", exclusionsFileName, line, kindExempt, kindBacklog)
		}
		kinds[filepath.ToSlash(filepath.Clean(strings.TrimSpace(fields[0])))] = kind
	}
	return kinds, scanner.Err()
}

// platformVariant はGOOS/GOARCH接尾辞を持つビルド別ファイルかを判定する。
func platformVariant(base string) bool {
	parts := strings.Split(base, "_")
	return len(parts) > 1 && buildSuffix[parts[len(parts)-1]]
}

func generatedFile(path string) bool {
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line != "" && !strings.HasPrefix(line, "//") {
			return false
		}
		if generatedHeader.MatchString(line) {
			return true
		}
	}
	return false
}

// mapTestFile はテストファイルの基底名を、最長一致する実装ファイルの基底名へ対応づける。対応先がなければ空文字を返す。
func mapTestFile(base string, impls map[string]bool) string {
	parts := strings.Split(base, "_")
	for count := len(parts); count > 0; count-- {
		if candidate := strings.Join(parts[:count], "_"); impls[candidate] {
			return candidate
		}
	}
	return ""
}

// checkDir は1パッケージ分のディレクトリを見て、テストファイルを持たない実装ファイルのpathを返す。
func checkDir(dir string, entries []os.DirEntry, root string) []string {
	impls := map[string]bool{}
	var candidates, tests []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		if strings.HasSuffix(name, "_test.go") {
			tests = append(tests, name)
			continue
		}
		base := strings.TrimSuffix(name, ".go")
		impls[base] = true
		if !platformVariant(base) && !generatedFile(filepath.Join(dir, name)) {
			candidates = append(candidates, base)
		}
	}
	tested := map[string]bool{}
	for _, name := range tests {
		if target := mapTestFile(strings.TrimSuffix(name, "_test.go"), impls); target != "" {
			tested[target] = true
		}
	}
	var missing []string
	for _, base := range candidates {
		if tested[base] {
			continue
		}
		path, err := filepath.Rel(root, filepath.Join(dir, base+".go"))
		if err != nil {
			path = filepath.Join(dir, base+".go")
		}
		missing = append(missing, filepath.ToSlash(path))
	}
	return missing
}

func run(root string, out io.Writer) int {
	kinds, err := loadExclusions(root)
	if err != nil {
		_, _ = fmt.Fprintln(out, err.Error())
		return 1
	}
	var violations, warnings, failures []string
	reported := map[string]bool{}
	for _, directory := range []string{"cmd", "internal", "tools"} {
		walkErr := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				return nil
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, item := range checkDir(path, entries, root) {
				reported[item] = true
				switch kinds[item] {
				case kindExempt:
				case kindBacklog:
					warnings = append(warnings, fmt.Sprintf("%s:1: test-layout: no test file is named after this implementation file; known backlog", item))
				default:
					violations = append(violations, fmt.Sprintf("%s:1: test-layout: no test file is named after this implementation file", item))
				}
			}
			return nil
		})
		if walkErr != nil {
			failures = append(failures, walkErr.Error())
		}
	}
	// 対応済みになったエントリを残すと免除が実態から離れるため、古い行は失敗として報告する。
	for path, kind := range kinds {
		if kind == kindBacklog && !reported[path] {
			failures = append(failures, fmt.Sprintf("%s:1: test-layout: stale %s entry in %s; the file now has a test file named after it", path, kind, exclusionsFileName))
		}
	}
	sort.Strings(warnings)
	sort.Strings(violations)
	sort.Strings(failures)
	for _, line := range append(append(warnings, violations...), failures...) {
		_, _ = fmt.Fprintln(out, line)
	}
	// 案内はファイルごとに繰り返さず、末尾へ1回だけ出す。
	if len(warnings)+len(violations) > 0 {
		for _, line := range guidance {
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
