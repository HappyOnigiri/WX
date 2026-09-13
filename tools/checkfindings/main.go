// checkfindings は diag.Finding の表示文が message ID を持つという規約を静的に守らせる。ID を書き忘れても
// internal/diag の Resolve は英語リテラルを残すため、訳さないと決めた文と登録を忘れた文が見分けられない。
// checkdisplay は日本語リテラル、checkcatalog は書かれた ID の実在だけを見るので、この穴はここで塞ぐ。
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
)

const exclusionsFileName = "findings-exclusions.txt"

// 免除の種別。exempt は恒久的に対象外とし、backlog は未移行の残務として警告だけを出す。
// 残務を消すには表示文を message ID へ移し、エントリを削除する。登録のない欠落だけが make ci を落とす。
const (
	kindExempt  = "exempt"
	kindBacklog = "backlog"
)

var guidance = []string{
	"finding guidance:",
	"- Give every display string of a diag.Finding a message ID in Messages; Resolve fills the string fields from the catalog.",
	"- Cause may stay a plain string when it only carries payload (an external error, a path, a command output); a prose literal needs Messages.Cause.",
	fmt.Sprintf("- Record an exception in %s as `<path><TAB>%s|%s<TAB><reason>`; the reason is required.", exclusionsFileName, kindExempt, kindBacklog),
	fmt.Sprintf("- %s entries stay reported as warnings; clear one by migrating the file and deleting its line.", kindBacklog),
	"- Delete the line of a file that no longer misses a message ID, and of a path that is no longer scanned; a stale entry is an error.",
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

// scan は本番コードの diag.Finding リテラルを走査し、message ID の無い表示文を報告する。
// テストは期待値として finding を組み立てるため対象外とする。
func scan(root string, kinds map[string]string) (problems, warnings []string, err error) {
	fset := token.NewFileSet()
	backlog := map[string]bool{}
	// seen は走査で見つけた登録済み path、missingFound はそのうち欠落が残っていたものである。
	// どちらにも入らない登録は、移行済みか存在しない path を指しているので stale として落とす。
	seen, missingFound := map[string]bool{}, map[string]bool{}
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
			composite, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, missing := range findingProblems(composite, file.Name.Name) {
				if excluded {
					// exempt はファイルごと対象外、backlog は未移行の残務として警告だけを出す。
					// どちらも「まだ欠落がある」ことを記録し、不要になった行を stale として検出する。
					missingFound[relative] = true
					if kind == kindBacklog {
						backlog[relative] = true
					}
					continue
				}
				position := fset.Position(missing.pos)
				problems = append(problems, fmt.Sprintf("%s:%d: diag.Finding literal has no message ID for %s; add it to Messages",
					relative, position.Line, strings.Join(missing.fields, ", ")))
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	for path := range backlog {
		warnings = append(warnings, path+": still carries diag.Finding display strings without a message ID (backlog)")
	}
	for path, kind := range kinds {
		switch {
		case !seen[path]:
			problems = append(problems, fmt.Sprintf("%s: %s entry names a file that is not scanned; delete the line", path, kind))
		case kind == kindBacklog && !missingFound[path]:
			problems = append(problems, fmt.Sprintf("%s: %s entry has no diag.Finding without a message ID left; delete the line", path, kind))
		}
	}
	sort.Strings(problems)
	sort.Strings(warnings)
	return problems, warnings, nil
}

// gap は 1 つの finding リテラルで message ID を欠いた表示文である。
type gap struct {
	pos    token.Pos
	fields []string
}

// findingProblems は複合リテラルが diag.Finding なら、message ID を欠いた表示文を返す。
// 型を省略した要素リテラルには要素型を補って辿り、型を書いた要素は ast.Inspect 側の走査に任せる。
func findingProblems(composite *ast.CompositeLit, packageName string) []gap {
	if elementType, ok := findingContainer(composite.Type, packageName); ok {
		var gaps []gap
		for _, element := range composite.Elts {
			// map と index 付きの要素は KeyValueExpr になる。値だけが finding である。
			if pair, ok := element.(*ast.KeyValueExpr); ok {
				element = pair.Value
			}
			inner, ok := element.(*ast.CompositeLit)
			if !ok || inner.Type != nil {
				continue
			}
			gaps = append(gaps, findingProblems(&ast.CompositeLit{Type: elementType, Elts: inner.Elts, Lbrace: inner.Lbrace}, packageName)...)
		}
		return gaps
	}
	if !isFindingType(composite.Type, packageName) || len(composite.Elts) == 0 {
		return nil
	}
	fields := literalFields(composite)
	var ids map[string]ast.Expr
	if messages, present := fields["Messages"]; present {
		literal, ok := messages.(*ast.CompositeLit)
		if !ok {
			// Messages を変数で渡す形は中身を静的に読めないため、判定せず通す。
			return nil
		}
		ids = literalFields(literal)
	}
	var missing []string
	for _, name := range []string{"Summary", "Action"} {
		if _, ok := fields[name]; ok {
			if _, resolved := ids[name]; !resolved {
				missing = append(missing, "Messages."+name)
			}
		}
	}
	// Cause は外部コマンドの出力やエラー文字列を入れる正当な経路があるため、
	// wx 自身の散文リテラルを含むときだけ message ID を要求する。
	if cause, ok := fields["Cause"]; ok && containsProse(cause) {
		if _, resolved := ids["Cause"]; !resolved {
			missing = append(missing, "Messages.Cause")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []gap{{pos: composite.Lbrace, fields: missing}}
}

// literalFields は複合リテラルのフィールド名と値の対応を返す。位置指定の要素は名前を持たないため含めない。
func literalFields(composite *ast.CompositeLit) map[string]ast.Expr {
	fields := map[string]ast.Expr{}
	for _, element := range composite.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := pair.Key.(*ast.Ident)
		if !ok {
			continue
		}
		fields[key.Name] = pair.Value
	}
	return fields
}

// isFindingType は式が diag.Finding を指すかを返す。
// package diag の中では型名だけで書かれるため、解析中のファイルの package 名で切り分ける。
// hookconfig にも Finding 型があり、package を見ないと別型を大量に誤検出する。
func isFindingType(expression ast.Expr, packageName string) bool {
	switch typeExpr := expression.(type) {
	case *ast.SelectorExpr:
		ident, ok := typeExpr.X.(*ast.Ident)
		return ok && ident.Name == "diag" && typeExpr.Sel.Name == "Finding"
	case *ast.Ident:
		return packageName == "diag" && typeExpr.Name == "Finding"
	}
	return false
}

// findingContainer は diag.Finding を要素に持つ slice・array・map なら、その要素型を返す。
// 要素の型は複合リテラルで省略できるため、要素へ降りるときに補う必要がある。
func findingContainer(expression ast.Expr, packageName string) (ast.Expr, bool) {
	var element ast.Expr
	switch container := expression.(type) {
	case *ast.ArrayType:
		element = container.Elt
	case *ast.MapType:
		element = container.Value
	default:
		return nil, false
	}
	if isFindingType(element, packageName) {
		return element, true
	}
	if _, ok := findingContainer(element, packageName); ok {
		return element, true
	}
	return nil, false
}

// containsProse は式が wx 自身の散文リテラルを含むかを返す。
// fmt.Sprintf の書式や `+` の連結も部分木として辿る。err.Error() や識別子だけの式は含まない。
func containsProse(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && prose(value) {
			found = true
		}
		return !found
	})
	return found
}

// prose は 2 文字以上の英単語を 2 つ以上含むかを返す。
// 語 1 つの断片や区切り文字は payload の組み立てなので、message ID の要求対象にしない。
func prose(value string) bool {
	words, length := 0, 0
	for _, r := range value + " " {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			length++
			continue
		}
		if length >= 2 {
			words++
		}
		length = 0
	}
	return words >= 2
}
