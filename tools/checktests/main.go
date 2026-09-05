// checktests はテストコードの規約をASTで検査する。
// 規約はレビューやAGENTS.mdの記述ではなくこの検査で強制し、違反をmake ciで落とす。
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
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type violation struct {
	path    string
	line    int
	rule    string
	message string
}

// marker は検査を免除するコメントで、理由の記載を必須とする。
var marker = regexp.MustCompile(`socketlint:allow-tempdir -- (.+)$`)

// socketNameSuffix はunix socketとして扱うファイル名の判定に使う。
const socketNameSuffix = ".sock"

// tempDirCall は t.TempDir() 形式の呼び出しかを判定する。
func tempDirCall(node ast.Expr) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "TempDir" && len(call.Args) == 0
}

// joinCall は filepath.Join 呼び出しならその引数を返す。
func joinCall(node ast.Expr) ([]ast.Expr, bool) {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return nil, false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Join" {
		return nil, false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "filepath" {
		return nil, false
	}
	return call.Args, true
}

// socketLiteral は引数が socket 名の文字列リテラルならその値を返す。
func socketLiteral(node ast.Expr) (string, bool) {
	literal, ok := node.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil || !strings.HasSuffix(value, socketNameSuffix) {
		return "", false
	}
	return value, true
}

// derived はテンポラリディレクトリ由来の識別子集合を保ちながら、式がその由来かを判定する。
type derived map[string]bool

func (d derived) expr(node ast.Expr) bool {
	if tempDirCall(node) {
		return true
	}
	if identifier, ok := node.(*ast.Ident); ok {
		return d[identifier.Name]
	}
	args, ok := joinCall(node)
	if !ok {
		return false
	}
	for _, arg := range args {
		if d.expr(arg) {
			return true
		}
	}
	return false
}

// record は左辺の識別子がテンポラリディレクトリ由来かを更新する。
// 多値代入の右辺は追跡できないため、由来なしとして上書きする。
func (d derived) record(left, right []ast.Expr) {
	for i, target := range left {
		identifier, ok := target.(*ast.Ident)
		if !ok || identifier.Name == "_" {
			continue
		}
		d[identifier.Name] = len(left) == len(right) && d.expr(right[i])
	}
}

// checkFile は t.TempDir() 由来のパスからunix socketを組み立てている箇所を返す。
// t.TempDir() のパスにはテスト名が入るため、テスト名が伸びるとsun_pathの上限を超えてbindが失敗する。
func checkFile(path string, allowed map[int]bool) ([]violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	if err != nil {
		return nil, err
	}
	var issues []violation
	// 識別子の追跡は関数ごとに区切り、同名の変数が別の関数から漏れないようにする。
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		issues = append(issues, checkFunction(path, fset, function, allowed)...)
	}
	return issues, nil
}

func checkFunction(path string, fset *token.FileSet, function *ast.FuncDecl, allowed map[int]bool) []violation {
	var issues []violation
	names := derived{}
	ast.Inspect(function, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			names.record(typed.Lhs, typed.Rhs)
		case *ast.CallExpr:
			args, ok := joinCall(typed)
			if !ok {
				return true
			}
			name := ""
			for _, arg := range args {
				if value, isSocket := socketLiteral(arg); isSocket {
					name = value
				}
			}
			line := fset.PositionFor(typed.Lparen, false).Line
			if name == "" || allowed[line] || allowed[line-1] {
				return true
			}
			for _, arg := range args {
				if names.expr(arg) {
					issues = append(issues, violation{path, line, "socket-tempdir", fmt.Sprintf("socket %q is built from t.TempDir(); use testsupport.SocketPath, or exempt with a `socketlint:allow-tempdir -- <理由>` comment", name)})
					break
				}
			}
		}
		return true
	})
	return issues
}

// markerLines は免除コメントの行番号と、理由を欠いた記載の違反を返す。
func markerLines(path string) (map[int]bool, []violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil, nil, err
	}
	allowed := map[int]bool{}
	var issues []violation
	for _, group := range file.Comments {
		for _, comment := range group.List {
			line := fset.PositionFor(comment.Slash, false).Line
			if !strings.Contains(comment.Text, "socketlint:allow-tempdir") {
				continue
			}
			match := marker.FindStringSubmatch(comment.Text)
			if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
				issues = append(issues, violation{path, line, "marker-format", "socketlint:allow-tempdir requires ` -- <理由>`"})
				continue
			}
			allowed[line] = true
		}
	}
	return allowed, issues, nil
}

func run(root string, out io.Writer) int {
	var issues []violation
	var failures []string
	for _, directory := range []string{"cmd", "internal", "tools"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			allowed, markerIssues, err := markerLines(path)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: parse/read: %v", path, err))
				return nil
			}
			issues = append(issues, markerIssues...)
			found, err := checkFile(path, allowed)
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: parse/read: %v", path, err))
			}
			issues = append(issues, found...)
			return nil
		})
		if err != nil {
			failures = append(failures, err.Error())
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.path != b.path {
			return a.path < b.path
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.rule < b.rule
	})
	for _, issue := range issues {
		_, _ = fmt.Fprintf(out, "%s:%d: %s: %s\n", issue.path, issue.line, issue.rule, issue.message)
	}
	sort.Strings(failures)
	for _, failure := range failures {
		_, _ = fmt.Fprintln(out, failure)
	}
	if len(issues)+len(failures) > 0 {
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(".", os.Stderr))
}
