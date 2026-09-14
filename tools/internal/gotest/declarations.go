package gotest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// Declaration はテスト関数の宣言位置を指す。
// JSON タグは manifest の契約であり、report-flaky-tests.cjs が同じ名前で読む。
type Declaration struct {
	Package  string `json:"package"`
	Path     string `json:"path"`
	Function string `json:"function"`
	Line     int    `json:"line"`
}

// listedPackage は go list -json のうち、テストファイルの列挙に必要な部分だけを読む。
type listedPackage struct {
	ImportPath   string   `json:"ImportPath"`
	Dir          string   `json:"Dir"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
}

// Resolver はパッケージ名からテスト関数の宣言を引く。
// 解決済みのパッケージを保持するので、同じパッケージを繰り返し指定しても go list は1回で済む。
type Resolver struct {
	GoCommand string
	RepoRoot  string

	cache map[string]map[string]Declaration
}

// Declarations は指定したパッケージのテスト関数を、関数名から宣言へ引ける形で返す。
// 複数のパッケージを一度に渡せる。内部テストと外部テストに同名の関数があるパッケージは、
// どちらを指すか決められないためパッケージ全体をエラーにする。
func (r *Resolver) Declarations(ctx context.Context, packageNames ...string) (map[string]map[string]Declaration, error) {
	if r.cache == nil {
		r.cache = make(map[string]map[string]Declaration)
	}
	pending := make([]string, 0, len(packageNames))
	seen := make(map[string]bool, len(packageNames))
	for _, name := range packageNames {
		if seen[name] || r.cache[name] != nil {
			continue
		}
		seen[name] = true
		pending = append(pending, name)
	}
	if len(pending) > 0 {
		if err := r.load(ctx, pending); err != nil {
			return nil, err
		}
	}
	result := make(map[string]map[string]Declaration, len(packageNames))
	for _, name := range packageNames {
		if found := r.cache[name]; found != nil {
			result[name] = found
		}
	}
	return result, nil
}

func (r *Resolver) load(ctx context.Context, packageNames []string) error {
	goCommand := r.GoCommand
	if goCommand == "" {
		goCommand = "go"
	}
	command := exec.CommandContext(ctx, goCommand, append([]string{"list", "-json"}, packageNames...)...)
	command.Dir = r.RepoRoot
	output, err := command.Output()
	if err != nil {
		return err
	}
	// go list -json は要求したパッケージの数だけ JSON オブジェクトを続けて書くので、
	// Decode を繰り返して全て読む。1回だけ読むと黙って先頭のパッケージしか返らない。
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		found, err := r.parsePackage(listed)
		if err != nil {
			return err
		}
		r.cache[listed.ImportPath] = found
	}
	return nil
}

func (r *Resolver) parsePackage(listed listedPackage) (map[string]Declaration, error) {
	files := append([]string(nil), listed.TestGoFiles...)
	files = append(files, listed.XTestGoFiles...)
	result := make(map[string]Declaration)
	for _, fileName := range files {
		path := filepath.Join(listed.Dir, fileName)
		found, err := ParseDeclarations(path, listed.ImportPath, r.RepoRoot)
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

// ParseDeclarations は1つのテストファイルからテスト関数の宣言を取り出す。
// Path はリポジトリ相対のスラッシュ区切りにそろえ、issue のタイトルが実行環境で変わらないようにする。
func ParseDeclarations(path, packageName, root string) (map[string]Declaration, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]Declaration)
	for _, item := range file.Decls {
		function, ok := item.(*ast.FuncDecl)
		if !ok || function.Recv != nil || !isTestDeclaration(function.Name.Name) {
			continue
		}
		position := fileSet.Position(function.Pos())
		result[function.Name.Name] = Declaration{
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

// RootTestName はサブテスト名から、宣言を持つ最上位のテスト関数名を取り出す。
func RootTestName(name string) string {
	if index := strings.IndexByte(name, '/'); index >= 0 {
		return name[:index]
	}
	return name
}
