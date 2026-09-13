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
		ast.Inspect(file, func(node ast.Node) bool {
			if composite, ok := node.(*ast.CompositeLit); ok {
				problems = append(problems, messageLiteralProblems(fset, known, composite)...)
				return true
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if name, ok := plainCallName(call); ok {
				problems = append(problems, unknownIDProblem(fset, known, call, 0, name)...)
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

// plainCallName は message ID を第 1 引数に取る package 内ヘルパの呼び出しを見分ける。
// internal/setup の message・messageError は表示文を組み立てる唯一の入口なので、
// 生成側が増えても未知 ID がそこから画面へ出ないようにする。
func plainCallName(call *ast.CallExpr) (string, bool) {
	ident, ok := call.Fun.(*ast.Ident)
	if !ok {
		return "", false
	}
	switch ident.Name {
	case "message", "messageError":
		return ident.Name, true
	default:
		return "", false
	}
}

// messageLiteralProblems は i18n.Message{ID: "..."} の ID をカタログと照合する。
// 生成側が message ID を直接書く経路はこの複合リテラルが最も多い。
func messageLiteralProblems(fset *token.FileSet, known map[string]i18n.Entry, composite *ast.CompositeLit) []string {
	selector, ok := composite.Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Message" {
		return nil
	}
	if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "i18n" {
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

// unknownIDProblem は呼び出しの指定位置の引数を照合する。literal でない ID は可変 ID として見逃す。
func unknownIDProblem(fset *token.FileSet, known map[string]i18n.Entry, call *ast.CallExpr, index int, _ string) []string {
	if index >= len(call.Args) {
		return nil
	}
	return unknownID(fset, known, call.Args[index])
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
