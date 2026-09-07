// checkgitexec は本番Goコードからos/exec経由でGitを直接起動する箇所をASTで検出する。
// Gitはinternal/gitx経由という不変条件をレビューではなくこの検査で強制し、違反をmake ciで落とす。
// 構文だけで分かる起動を対象とし、所有権証明やCASの正しさは判定しない。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// execImportPath はGitを起動し得る標準パッケージ。
// scannedRootsは本番コードの探索範囲で、allowedPackageは実行アダプタとしてパス単位で許可する唯一の例外である。
const (
	execImportPath = "os/exec"
	allowedPackage = "internal/gitx"
)

var scannedRoots = []string{"cmd", "internal"}

// gitProgram は検出対象のプログラム名。絶対パスはbasenameがこれと一致するかで判定する。
const gitProgram = "git"

// commandArgIndex は関数名ごとの、起動するプログラムを渡す引数の位置。
// LookPathは探索のみで起動しないため、ここに載せず違反にしない。
var commandArgIndex = map[string]int{
	"Command":        0,
	"CommandContext": 1,
}

// constDepthLimit は const 同士の参照をたどる上限。循環参照でも停止させる。
const constDepthLimit = 16

type violation struct {
	path    string
	line    int
	message string
}

func guidance() []string {
	return []string{
		"git-exec guidance:",
		"- Start Git through internal/gitx; its Runner strips the inherited GIT_DIR, GIT_WORK_TREE, and GIT_INDEX_FILE that would redirect the command to another repository.",
		`- Replace exec.Command("git", ...) with runner.Run(ctx, dir, args...), or runner.RunAt(ctx, dirFile, ...) when the CWD must stay bound to a descriptor.`,
		fmt.Sprintf("- %s is the only package allowed to launch Git directly; this check exempts it by path.", allowedPackage),
	}
}

func main() {
	if err := run(os.DirFS("."), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root fs.FS, out io.Writer) error {
	violations, scanned, err := inspect(root)
	if err != nil {
		return err
	}
	if len(violations) == 0 {
		_, _ = fmt.Fprintf(out, "checkgitexec: %d production file(s) launch Git only through %s\n", scanned, allowedPackage)
		return nil
	}
	for _, item := range violations {
		_, _ = fmt.Fprintf(out, "%s:%d: %s\n", item.path, item.line, item.message)
	}
	for _, line := range guidance() {
		_, _ = fmt.Fprintln(out, line)
	}
	return fmt.Errorf("checkgitexec: %d direct Git launch(es) outside %s", len(violations), allowedPackage)
}

// inspect は探索範囲の本番ソースを構文解析し、違反とファイル数を返す。
// build tagで無効な組み合わせも含めて解析し、GOOS別ファイルの適用漏れを防ぐ。
func inspect(root fs.FS) ([]violation, int, error) {
	var violations []violation
	scanned := 0
	err := fs.WalkDir(root, ".", func(target string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if skipDir(target, entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !production(target) {
			return nil
		}
		source, err := fs.ReadFile(root, target)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, target, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", target, err)
		}
		scanned++
		violations = append(violations, checkFile(target, fset, file)...)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].path != violations[j].path {
			return violations[i].path < violations[j].path
		}
		return violations[i].line < violations[j].line
	})
	return violations, scanned, nil
}

func skipDir(target, name string) bool {
	if target == "." {
		return false
	}
	if strings.HasPrefix(name, ".") || name == "tmp" || name == "testdata" {
		return true
	}
	if target == allowedPackage {
		return true
	}
	// 探索範囲の外側は根から刈り、cmd/internal以外の走査を避ける。
	return !strings.Contains(target, "/") && !inScannedRoots(target)
}

func inScannedRoots(name string) bool {
	for _, root := range scannedRoots {
		if name == root {
			return true
		}
	}
	return false
}

// production はテストを除いた探索範囲内のGoソースかを判定する。
// テストには独立したGit状態の確認があるため、初版の対象は本番ソースに限る。
func production(target string) bool {
	if !strings.HasSuffix(target, ".go") || strings.HasSuffix(target, "_test.go") {
		return false
	}
	root, _, found := strings.Cut(target, "/")
	return found && inScannedRoots(root)
}

// checkFile は1ファイル分の違反を返す。os/exec を import しないファイルは対象外とする。
func checkFile(target string, fset *token.FileSet, file *ast.File) []violation {
	lineOf := func(position token.Pos) int { return fset.Position(position).Line }
	name, dotImported, importPos := execImportName(file)
	if dotImported {
		return []violation{{
			path: target, line: lineOf(importPos),
			message: fmt.Sprintf("%s is dot-imported, which hides direct Git launches from this check; import it under a name such as exec", execImportPath),
		}}
	}
	if name == "" {
		return nil
	}
	constants := stringConstants(file)
	var violations []violation
	for _, decl := range file.Decls {
		// 局所的な同名の識別子はimportを覆うため、その宣言ごと対象外にして誤検知を避ける。
		if declaresLocal(decl, name) {
			continue
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if program, position, found := gitLaunch(call, name, constants); found {
				violations = append(violations, violation{
					path: target, line: lineOf(position),
					message: fmt.Sprintf("%s.%s starts %q directly; Git must run through %s", name, program.function, program.value, allowedPackage),
				})
			}
			return true
		})
	}
	return violations
}

type launch struct {
	function string
	value    string
}

// gitLaunch は呼び出しがGitの直接起動かを判定する。動的な値は解釈できないため対象外とする。
func gitLaunch(call *ast.CallExpr, execName string, constants map[string]ast.Expr) (launch, token.Pos, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return launch{}, 0, false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok || pkg.Name != execName {
		return launch{}, 0, false
	}
	index, ok := commandArgIndex[selector.Sel.Name]
	if !ok || len(call.Args) <= index {
		return launch{}, 0, false
	}
	value, ok := staticString(call.Args[index], constants, 0)
	if !ok || !isGit(value) {
		return launch{}, 0, false
	}
	return launch{function: selector.Sel.Name, value: value}, call.Lparen, true
}

// isGit は git そのものと、basenameが git の絶対パスを対象にする。
// 相対パスの ./git などは配置に依存して意味が変わるため、この検査では扱わない。
func isGit(value string) bool {
	if value == gitProgram {
		return true
	}
	return path.IsAbs(value) && path.Base(value) == gitProgram
}

// execImportName は os/exec の参照名を返す。dot importは別途扱い、blank importは参照できないため空を返す。
func execImportName(file *ast.File) (name string, dotImported bool, position token.Pos) {
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil || value != execImportPath {
			continue
		}
		if spec.Name == nil {
			return "exec", false, spec.Pos()
		}
		switch spec.Name.Name {
		case ".":
			return "", true, spec.Pos()
		case "_":
			return "", false, spec.Pos()
		default:
			return spec.Name.Name, false, spec.Pos()
		}
	}
	return "", false, 0
}

// stringConstants は同一ファイル内のconst名と値の式を集める。パッケージ跨ぎの定数は解決しない。
func stringConstants(file *ast.File) map[string]ast.Expr {
	constants := map[string]ast.Expr{}
	ast.Inspect(file, func(node ast.Node) bool {
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != len(value.Values) {
				continue
			}
			for index, ident := range value.Names {
				constants[ident.Name] = value.Values[index]
			}
		}
		return true
	})
	return constants
}

// staticString は文字列リテラルと、リテラル・同一ファイルのconstだけで組まれた連結を解決する。
func staticString(expr ast.Expr, constants map[string]ast.Expr, depth int) (string, bool) {
	if depth > constDepthLimit {
		return "", false
	}
	switch node := expr.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(node.Value)
		return value, err == nil
	case *ast.ParenExpr:
		return staticString(node.X, constants, depth+1)
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, leftOK := staticString(node.X, constants, depth+1)
		right, rightOK := staticString(node.Y, constants, depth+1)
		return left + right, leftOK && rightOK
	case *ast.Ident:
		value, ok := constants[node.Name]
		if !ok {
			return "", false
		}
		return staticString(value, constants, depth+1)
	default:
		return "", false
	}
}

// declaresLocal は宣言の中で name を局所的に宣言しているかを返す。
// 宣言位置より前の参照も一括して対象外にする粗い判定で、見逃す側へ倒して誤検知を出さない。
func declaresLocal(decl ast.Decl, name string) bool {
	found := false
	ast.Inspect(decl, func(node ast.Node) bool {
		if found {
			return false
		}
		switch typed := node.(type) {
		case *ast.FuncDecl:
			found = fieldsDeclare(typed.Recv, name) || fieldsDeclare(typed.Type.Params, name) || fieldsDeclare(typed.Type.Results, name)
		case *ast.FuncLit:
			found = fieldsDeclare(typed.Type.Params, name) || fieldsDeclare(typed.Type.Results, name)
		case *ast.AssignStmt:
			if typed.Tok == token.DEFINE {
				found = identsDeclare(typed.Lhs, name)
			}
		case *ast.RangeStmt:
			if typed.Tok == token.DEFINE {
				found = identsDeclare([]ast.Expr{typed.Key, typed.Value}, name)
			}
		case *ast.ValueSpec:
			for _, ident := range typed.Names {
				if ident.Name == name {
					found = true
				}
			}
		case *ast.TypeSpec:
			found = typed.Name != nil && typed.Name.Name == name
		}
		return !found
	})
	return found
}

func fieldsDeclare(fields *ast.FieldList, name string) bool {
	if fields == nil {
		return false
	}
	for _, field := range fields.List {
		for _, ident := range field.Names {
			if ident.Name == name {
				return true
			}
		}
	}
	return false
}

func identsDeclare(exprs []ast.Expr, name string) bool {
	for _, expr := range exprs {
		if ident, ok := expr.(*ast.Ident); ok && ident.Name == name {
			return true
		}
	}
	return false
}
