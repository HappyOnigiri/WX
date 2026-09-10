package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"strings"
)

func declarations(ctx context.Context, cfg config, packageName string) (map[string]declaration, error) {
	command := exec.CommandContext(ctx, cfg.GoCommand, "list", "-json", packageName)
	command.Dir = cfg.RepoRoot
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	var listed packageDeclaration
	if err := decoder.Decode(&listed); err != nil {
		return nil, err
	}
	files := append([]string(nil), listed.TestGoFiles...)
	files = append(files, listed.XTestGoFiles...)
	result := make(map[string]declaration)
	for _, fileName := range files {
		path := filepath.Join(listed.Dir, fileName)
		found, err := parseDeclarations(path, packageName, cfg.RepoRoot)
		if err != nil {
			return nil, err
		}
		for name, item := range found {
			if _, exists := result[name]; exists {
				return nil, fmt.Errorf("ambiguous declaration %s", name)
			}
			result[name] = item
		}
	}
	return result, nil
}

func parseDeclarations(path, packageName, root string) (map[string]declaration, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]declaration)
	for _, item := range file.Decls {
		function, ok := item.(*ast.FuncDecl)
		if !ok || function.Recv != nil || !isTestDeclaration(function.Name.Name) {
			continue
		}
		position := fileSet.Position(function.Pos())
		result[function.Name.Name] = declaration{
			Package:  packageName,
			Path:     filepath.ToSlash(relative),
			Function: function.Name.Name,
			Line:     position.Line,
		}
	}
	return result, nil
}

func isTestDeclaration(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}
