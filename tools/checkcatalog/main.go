// i18n カタログの英語・日本語の対応と template を make ci の検査へ接続する。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func main() {
	if err := i18n.ValidateCatalog(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := validateReferences(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// validateReferences は literal として書かれた ID をカタログと照合する。可変 ID は機械的に
// 判定できないため対象外にし、未知 ID の追加だけを catalog-check で CI に止めさせる。
// ただし field と indentLine はラベル位置にデータが流れる経路なので、literal 以外を拒否する。
func validateReferences(root string) error {
	known := i18n.Catalog()
	var problems []string
	fset := token.NewFileSet()
	// 同 package の宣言を見て判定するため、ディレクトリ単位で 1 度だけ集める。
	helpersByDirectory := map[string]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		directory := filepath.Dir(path)
		helpers, collected := helpersByDirectory[directory]
		if !collected {
			helpers, err = messageHelpers(fset, directory)
			if err != nil {
				return err
			}
			helpersByDirectory[directory] = helpers
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if composite, ok := node.(*ast.CompositeLit); ok {
				problems = append(problems, messageLiteralProblems(fset, known, composite)...)
				return true
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				if helpers[ident.Name] && len(call.Args) > 0 {
					problems = append(problems, unknownID(fset, known, call.Args[0])...)
				}
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var argumentIndex int
			// field はラベルを message ID で解決する描画ヘルパである。ID を literal に
			// 限ることで、payload のキーや path をラベル位置へ流す経路を dataField だけに残す。
			requireLiteral := false
			switch selector.Sel.Name {
			case "T":
				if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "i18n" {
					return true
				}
				argumentIndex = 1
			case "Localize", "LocalizeOr", "line":
				argumentIndex = 0
			case "NewError":
				if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "i18n" {
					return true
				}
				argumentIndex = 0
			case "WrapError":
				if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "i18n" {
					return true
				}
				argumentIndex = 1
			case "field", "indentLine":
				argumentIndex, requireLiteral = 1, true
			default:
				return true
			}
			if argumentIndex >= len(call.Args) {
				return true
			}
			literal, ok := call.Args[argumentIndex].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				if requireLiteral {
					position := fset.Position(call.Args[argumentIndex].Pos())
					problems = append(problems, fmt.Sprintf("%s:%d: %s needs a literal message ID; use dataField when the label comes from the payload",
						position.Filename, position.Line, selector.Sel.Name))
				}
				return true
			}
			id, err := strconv.Unquote(literal.Value)
			if err != nil || strings.TrimSpace(id) == "" || known[id].EN != "" {
				return true
			}
			position := fset.Position(literal.Pos())
			problems = append(problems, fmt.Sprintf("%s:%d: unknown message ID %q", position.Filename, position.Line, id))
			return true
		})
		return nil
	})
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("catalog references:\n%s", strings.Join(problems, "\n"))
	}
	return nil
}

// messageHelpers は message ID を第 1 引数 `id string` に取る package 内ヘルパの名前を集める。
// 名前だけで判定すると、別 package の同名関数（message(format string, ...)）を
// 未知 ID として誤検知する。宣言の形まで見て、同じ package のものだけを対象にする。
func messageHelpers(fset *token.FileSet, directory string) (map[string]bool, error) {
	packages, err := parser.ParseDir(fset, directory, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", directory, err)
	}
	helpers := map[string]bool{}
	for _, pkg := range packages {
		for _, file := range pkg.Files {
			for _, declaration := range file.Decls {
				function, ok := declaration.(*ast.FuncDecl)
				if !ok || function.Recv != nil || function.Type.Params == nil || len(function.Type.Params.List) == 0 {
					continue
				}
				switch function.Name.Name {
				case "message", "messageError":
				default:
					continue
				}
				first := function.Type.Params.List[0]
				if len(first.Names) != 1 || first.Names[0].Name != "id" {
					continue
				}
				if ident, ok := first.Type.(*ast.Ident); !ok || ident.Name != "string" {
					continue
				}
				if !returnsMessageOrError(function) {
					continue
				}
				helpers[function.Name.Name] = true
			}
		}
	}
	return helpers, nil
}

// messageLiteralProblems は i18n.Message{ID: "..."} の ID をカタログと照合する。
// 生成側が message ID を直接書く経路はこの複合リテラルが最も多い。
// []i18n.Message{{ID: ...}} のように要素の型が省略される形も同じ規則で照合する。
func messageLiteralProblems(fset *token.FileSet, known map[string]i18n.Entry, composite *ast.CompositeLit) []string {
	if elementType, ok := messageContainer(composite.Type); ok {
		var problems []string
		for _, element := range composite.Elts {
			inner, ok := element.(*ast.CompositeLit)
			if !ok {
				continue
			}
			if inner.Type == nil {
				inner = &ast.CompositeLit{Type: elementType, Elts: inner.Elts}
			}
			problems = append(problems, messageLiteralProblems(fset, known, inner)...)
		}
		return problems
	}
	if !isMessageType(composite.Type) {
		return nil
	}
	var problems []string
	for _, element := range composite.Elts {
		pair, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := pair.Key.(*ast.Ident); !ok || key.Name != "ID" {
			continue
		}
		problems = append(problems, unknownID(fset, known, pair.Value)...)
	}
	return problems
}

// returnsMessageOrError は宣言の最初の戻り値が i18n.Message か error かを返す。
// 第 1 引数の名前だけで判定すると、message(id string) string のような別用途の
// ヘルパまで message ID の生成経路として扱ってしまう。
func returnsMessageOrError(function *ast.FuncDecl) bool {
	if function.Type.Results == nil || len(function.Type.Results.List) == 0 {
		return false
	}
	result := function.Type.Results.List[0].Type
	if isMessageType(result) {
		return true
	}
	ident, ok := result.(*ast.Ident)
	return ok && ident.Name == "error"
}

// isMessageType は式が i18n.Message を指すかを返す。
func isMessageType(expression ast.Expr) bool {
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Message" {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == "i18n"
}

// messageContainer は i18n.Message を要素に持つ slice・array・map なら、その要素型を返す。
// 要素の型は複合リテラルで省略できるため、要素へ降りるときに補う必要がある。
func messageContainer(expression ast.Expr) (ast.Expr, bool) {
	switch container := expression.(type) {
	case *ast.ArrayType:
		if isMessageType(container.Elt) {
			return container.Elt, true
		}
	case *ast.MapType:
		if isMessageType(container.Value) {
			return container.Value, true
		}
	}
	return nil, false
}

// unknownID は式が literal の message ID なら、カタログに無いことを問題として返す。
func unknownID(fset *token.FileSet, known map[string]i18n.Entry, expression ast.Expr) []string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return nil
	}
	id, err := strconv.Unquote(literal.Value)
	if err != nil || strings.TrimSpace(id) == "" || known[id].EN != "" {
		return nil
	}
	position := fset.Position(literal.Pos())
	return []string{fmt.Sprintf("%s:%d: unknown message ID %q", position.Filename, position.Line, id)}
}
