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

// validateReferences は静的文字列として書かれた i18n.T / Localize の ID を
// カタログと照合する。可変 ID は機械的に判定できないため対象外にし、未知 ID
// の追加だけを catalog-check で CI に止めさせる。
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
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			argumentIndex := 0
			switch selector.Sel.Name {
			case "T":
				if ident, ok := selector.X.(*ast.Ident); !ok || ident.Name != "i18n" {
					return true
				}
				argumentIndex = 1
			case "Localize":
				argumentIndex = 0
			default:
				return true
			}
			if argumentIndex >= len(call.Args) {
				return true
			}
			literal, ok := call.Args[argumentIndex].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
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
