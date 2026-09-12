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

// 免除コメントの識別子。規則ごとに分け、ある規則の免除が別の規則へ効かないようにする。
const (
	tempDirMarker           = "socketlint:allow-tempdir"
	busyTimeoutMarker       = "sqlitelint:allow-no-busy-timeout"
	stateParallelMarker     = "statelint:allow-serial"
	stateParallelRule       = "state-test-parallel"
	stateParallelMarkerRule = "state-test-parallel-marker"
)

// markers は検査を免除するコメントで、理由の記載を必須とする。
// 出力順を固定するため、map ではなく宣言順の slice で持つ。
var markers = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{tempDirMarker, regexp.MustCompile(tempDirMarker + ` -- (.+)$`)},
	{busyTimeoutMarker, regexp.MustCompile(busyTimeoutMarker + ` -- (.+)$`)},
	{stateParallelMarker, regexp.MustCompile(stateParallelMarker + ` -- (.+)$`)},
}

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

// sqliteOpenCall は sql.Open 呼び出しならDSNの引数を返す。
func sqliteOpenCall(call *ast.CallExpr) (ast.Expr, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Open" {
		return nil, false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != "sql" || len(call.Args) < 2 {
		return nil, false
	}
	return call.Args[1], true
}

// calleeName は呼び出し先の関数名を返す。package修飾は落とし、末尾の識別子だけを見る。
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// busyTimeoutDSN はDSNの式が busy_timeout を指定していると読み取れるかを返す。
// 文字列リテラルへの直書きと、DSNを組み立てるhelper（名前が DSN で終わる関数）の呼び出しを認める。
func busyTimeoutDSN(dsn ast.Expr) bool {
	found := false
	ast.Inspect(dsn, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.BasicLit:
			if typed.Kind == token.STRING && strings.Contains(typed.Value, "_busy_timeout") {
				found = true
			}
		case *ast.CallExpr:
			if strings.HasSuffix(calleeName(typed), "DSN") {
				found = true
			}
		}
		return !found
	})
	return found
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

// exemptions は免除コメントの行番号を規則ごとに持つ。
type exemptions map[string]map[int]bool

// allows は違反行、またはその直前の行に該当する免除コメントがあるかを返す。
func (e exemptions) allows(marker string, line int) bool {
	lines := e[marker]
	return lines[line] || lines[line-1]
}

// checkFile は socket-tempdir と sqlite-busy-timeout の違反を返す。
// t.TempDir() 由来の socket path はテスト名が伸びるとsun_pathの上限を超えてbindが失敗し、
// busy_timeout のない sqlite 接続は他の書き手とlockが重なった瞬間に SQLITE_BUSY で落ちる。
func checkFile(path string, allowed exemptions) ([]violation, error) {
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
	issues = append(issues, checkStateParallel(path, fset, file, allowed)...)
	return issues, nil
}

// isStateTestPath は state package のテストだけを並列化規則の対象にする。
func isStateTestPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains("/"+clean+"/", "/internal/state/") && strings.HasSuffix(clean, "_test.go")
}

// isTestingT は関数引数が *testing.T かを返す。TestMain や補助関数を誤って対象にしないために使う。
func isTestingT(function *ast.FuncDecl) bool {
	if function.Type.Params == nil || len(function.Type.Params.List) == 0 {
		return false
	}
	parameter := function.Type.Params.List[0].Type
	star, ok := parameter.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := star.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "T" {
		return false
	}
	packageName, ok := selector.X.(*ast.Ident)
	return ok && packageName.Name == "testing"
}

// isStateTopLevelTest は state package の通常の TestXxx だけを判定対象にする。
func isStateTopLevelTest(function *ast.FuncDecl) bool {
	return function.Recv == nil && function.Name != nil && function.Name.Name != "TestMain" && strings.HasPrefix(function.Name.Name, "Test") && isTestingT(function)
}

// parallelCall は t.Parallel() の直接呼び出しかを返す。別の scope にある呼び出しは親テストを並列化しない。
func parallelCall(statement ast.Stmt, parameter string) bool {
	expression, ok := statement.(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := expression.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Parallel" || len(call.Args) != 0 {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && receiver.Name == parameter
}

// testingTParameter は *testing.T の引数名を返す。
func testingTParameter(function *ast.FuncDecl) (string, bool) {
	if !isTestingT(function) {
		return "", false
	}
	field := function.Type.Params.List[0]
	if len(field.Names) != 1 || field.Names[0].Name == "_" {
		return "", false
	}
	return field.Names[0].Name, true
}

// hasTopLevelParallel は関数本体直下にある t.Parallel() だけを認める。
func hasTopLevelParallel(function *ast.FuncDecl) bool {
	parameter, ok := testingTParameter(function)
	if !ok || function.Body == nil {
		return false
	}
	for _, statement := range function.Body.List {
		if parallelCall(statement, parameter) {
			return true
		}
	}
	return false
}

func sortedMarkerLines(lines map[int]bool) []int {
	ordered := make([]int, 0, len(lines))
	for line := range lines {
		ordered = append(ordered, line)
	}
	sort.Ints(ordered)
	return ordered
}

// checkStateParallel は state の各トップレベルテストが直接 t.Parallel() を呼ぶことを検査する。
// 直列を保つ必要があるテストだけは、関数宣言の直前に理由付き免除 marker を置ける。
func checkStateParallel(path string, fset *token.FileSet, file *ast.File, allowed exemptions) []violation {
	markerLines := allowed[stateParallelMarker]
	if len(markerLines) == 0 && !isStateTestPath(path) {
		return nil
	}
	if !isStateTestPath(path) {
		issues := make([]violation, 0, len(markerLines))
		for _, line := range sortedMarkerLines(markerLines) {
			issues = append(issues, violation{path: path, line: line, rule: stateParallelMarkerRule, message: fmt.Sprintf("%s is only valid on a state top-level test", stateParallelMarker)})
		}
		return issues
	}

	var issues []violation
	consumedMarkers := map[int]bool{}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || !isStateTopLevelTest(function) {
			continue
		}
		line := fset.PositionFor(function.Pos(), false).Line
		markerLine := 0
		if markerLines[line] {
			markerLine = line
		} else if markerLines[line-1] {
			markerLine = line - 1
		}
		if markerLine != 0 {
			consumedMarkers[markerLine] = true
		}
		if hasTopLevelParallel(function) {
			if markerLine != 0 {
				issues = append(issues, violation{path: path, line: markerLine, rule: stateParallelMarkerRule, message: fmt.Sprintf("%s is only valid for a serial state test", stateParallelMarker)})
			}
			continue
		}
		if markerLine == 0 {
			issues = append(issues, violation{path: path, line: line, rule: stateParallelRule, message: "state top-level test must call t.Parallel() directly, or use a reasoned statelint:allow-serial marker"})
		}
	}
	for _, line := range sortedMarkerLines(markerLines) {
		if !consumedMarkers[line] {
			issues = append(issues, violation{path: path, line: line, rule: stateParallelMarkerRule, message: fmt.Sprintf("%s must immediately precede a serial state top-level test", stateParallelMarker)})
		}
	}
	return issues
}

func checkFunction(path string, fset *token.FileSet, function *ast.FuncDecl, allowed exemptions) []violation {
	var issues []violation
	names := derived{}
	ast.Inspect(function, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.AssignStmt:
			names.record(typed.Lhs, typed.Rhs)
		case *ast.CallExpr:
			line := fset.PositionFor(typed.Lparen, false).Line
			if dsn, ok := sqliteOpenCall(typed); ok && !busyTimeoutDSN(dsn) && !allowed.allows(busyTimeoutMarker, line) {
				issues = append(issues, violation{path, line, "sqlite-busy-timeout", fmt.Sprintf("sql.Open does not set _busy_timeout; use a DSN helper, or exempt with a `%s -- <理由>` comment", busyTimeoutMarker)})
			}
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
			if name == "" || allowed.allows(tempDirMarker, line) {
				return true
			}
			for _, arg := range args {
				if names.expr(arg) {
					issues = append(issues, violation{path, line, "socket-tempdir", fmt.Sprintf("socket %q is built from t.TempDir(); use testsupport.SocketPath, or exempt with a `%s -- <理由>` comment", name, tempDirMarker)})
					break
				}
			}
		}
		return true
	})
	return issues
}

// markerLines は免除コメントの行番号と、理由を欠いた記載の違反を返す。
func markerLines(path string) (exemptions, []violation, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.AllErrors)
	if err != nil {
		return nil, nil, err
	}
	allowed := exemptions{}
	var issues []violation
	for _, group := range file.Comments {
		for _, comment := range group.List {
			line := fset.PositionFor(comment.Slash, false).Line
			for _, marker := range markers {
				if !strings.Contains(comment.Text, marker.name) {
					continue
				}
				match := marker.pattern.FindStringSubmatch(comment.Text)
				if len(match) != 2 || strings.TrimSpace(match[1]) == "" {
					issues = append(issues, violation{path, line, "marker-format", fmt.Sprintf("%s requires ` -- <理由>`", marker.name)})
					continue
				}
				if allowed[marker.name] == nil {
					allowed[marker.name] = map[int]bool{}
				}
				allowed[marker.name][line] = true
			}
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
