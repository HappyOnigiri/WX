// checkdisplay は「データは訳さない」という表示層の規約を静的に守らせる。
// 訳文は internal/i18n のカタログだけが持ち、描画側は message ID を引く。日本語のリテラルを
// 検査すると、全文置換の再導入と `if lang == Japanese` での日本語直書きが CI で落ちる。
package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const exclusionsFileName = "display-exclusions.txt"

// 免除の種別。exempt は恒久的に対象外とし、backlog は未移行の残務として警告だけを出す。
// 残務を消すには描画を message ID へ移し、エントリを削除する。登録のない日本語だけが make ci を落とす。
const (
	kindExempt  = "exempt"
	kindBacklog = "backlog"
)

var guidance = []string{
	"display guidance:",
	"- Keep Japanese text in internal/i18n's catalog and resolve it at render time through a message ID.",
	"- Do not translate payload values (paths, IDs, state names, times, external errors); pass them as template data.",
	fmt.Sprintf("- Record an exception in %s as `<path><TAB>%s|%s<TAB><reason>`; the reason is required.", exclusionsFileName, kindExempt, kindBacklog),
	fmt.Sprintf("- %s entries stay reported as warnings; clear one by migrating the file and deleting its line.", kindBacklog),
	"- Delete the line of a file that no longer holds a Japanese literal, and of a path that is no longer scanned; a stale entry is an error.",
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	kinds, err := loadExclusions(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	problems, warnings, err := scan(root, kinds)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "warning: "+warning)
	}
	if len(problems) == 0 {
		return
	}
	for _, problem := range problems {
		fmt.Fprintln(os.Stderr, problem)
	}
	for _, line := range guidance {
		fmt.Fprintln(os.Stderr, line)
	}
	os.Exit(1)
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

// scan は本番コードの文字列リテラルを走査し、日本語を含むものを報告する。
// テストは期待値として訳文を書くため対象外とする。
func scan(root string, kinds map[string]string) (problems, warnings []string, err error) {
	fset := token.NewFileSet()
	backlog := map[string]bool{}
	// seen は走査で見つけた登録済み path、japaneseFound はそのうち日本語リテラルが残っていたものである。
	// どちらにも入らない登録は、移行済みか存在しない path を指しているので stale として落とす。
	seen, japaneseFound := map[string]bool{}, map[string]bool{}
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		base := entry.Name()
		if filepath.Ext(base) != ".go" || strings.HasSuffix(base, "_test.go") {
			return nil
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)
		kind, excluded := kinds[relative]
		if excluded {
			seen[relative] = true
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil || !japanese(value) {
				return true
			}
			if excluded {
				// exempt はファイルごと対象外、backlog は未移行の残務として警告だけを出す。
				// どちらも「まだ日本語がある」ことを記録し、不要になった行を stale として検出する。
				japaneseFound[relative] = true
				if kind == kindBacklog {
					backlog[relative] = true
				}
				return true
			}
			position := fset.Position(literal.Pos())
			problems = append(problems, fmt.Sprintf("%s:%d: Japanese text in a string literal; move it to the i18n catalog", relative, position.Line))
			return true
		})
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	for path := range backlog {
		warnings = append(warnings, path+": still carries Japanese string literals (backlog)")
	}
	for path, kind := range kinds {
		switch {
		case !seen[path]:
			problems = append(problems, fmt.Sprintf("%s: %s entry names a file that is not scanned; delete the line", path, kind))
		case !japaneseFound[path]:
			problems = append(problems, fmt.Sprintf("%s: %s entry has no Japanese string literal left; delete the line", path, kind))
		}
	}
	sort.Strings(problems)
	sort.Strings(warnings)
	return problems, warnings, nil
}

// japanese はひらがな・カタカナ・漢字のいずれかを含むかを返す。
func japanese(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
