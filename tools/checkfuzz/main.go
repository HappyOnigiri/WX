// checkfuzz はfuzzターゲットの実体とCI設定の一致を検査する。
// go test -fuzz は一致するターゲットが無くても通常テストを実行して成功するため、
// 設定だけが残った空回りのjobを検出できない。この検査でmake ciから落とす。
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
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	workflowPath = ".github/workflows/nightly.yml"
	makefilePath = "Makefile"
)

// target はfuzzターゲットの位置。packageはモジュールルートからの相対パスに ./ を付けた形で表す。
type target struct {
	pkg  string
	name string
}

func (t target) String() string { return t.pkg + "." + t.name }

// workflow はnightlyのfuzz matrixだけを読む最小の形。
type workflow struct {
	Jobs struct {
		Fuzz struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Package string `yaml:"package"`
						Target  string `yaml:"target"`
					} `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"fuzz"`
	} `yaml:"jobs"`
}

// makefileFuzzPackages は make fuzz が渡すパッケージ一覧を取り出す。
var makefileFuzzPackages = regexp.MustCompile(`(?m)^fuzz:\n(?:\t.*\n)*?\t.*-fuzztime=[^\s]+\s+(.*)$`)

func main() {
	if err := run(os.DirFS("."), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root fs.FS, out io.Writer) error {
	declared, err := declaredTargets(root)
	if err != nil {
		return err
	}
	configured, err := workflowTargets(root)
	if err != nil {
		return err
	}
	var problems []string
	problems = append(problems, compareTargets(declared, configured)...)
	packages, err := makefilePackages(root)
	if err != nil {
		return err
	}
	problems = append(problems, comparePackages(declared, packages)...)
	if len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(out, problem)
		}
		return fmt.Errorf("checkfuzz: %d problem(s); fuzz configuration and fuzz targets disagree", len(problems))
	}
	_, _ = fmt.Fprintf(out, "checkfuzz: %d fuzz target(s) match %s and make fuzz\n", len(declared), workflowPath)
	return nil
}

// declaredTargets はリポジトリ内の func FuzzX(f *testing.F) を集める。
func declaredTargets(root fs.FS) (map[target]bool, error) {
	found := map[target]bool{}
	err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name := entry.Name(); path != "." && (strings.HasPrefix(name, ".") || name == "tmp") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := fs.ReadFile(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, source, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || !isFuzzTarget(function) {
				continue
			}
			found[target{pkg: "./" + filepath.ToSlash(filepath.Dir(path)), name: function.Name.Name}] = true
		}
		return nil
	})
	return found, err
}

// isFuzzTarget は Fuzz で始まる名前で *testing.F を1つだけ受け、レシーバも戻り値も無い関数かを判定する。
func isFuzzTarget(function *ast.FuncDecl) bool {
	if function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Fuzz") || function.Name.Name == "Fuzz" {
		return false
	}
	if function.Type.Results != nil || function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	pointer, ok := function.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := pointer.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "F" {
		return false
	}
	packageName, ok := selector.X.(*ast.Ident)
	return ok && packageName.Name == "testing"
}

func workflowTargets(root fs.FS) (map[target]bool, error) {
	source, err := fs.ReadFile(root, workflowPath)
	if err != nil {
		return nil, err
	}
	var parsed workflow
	if err := yaml.Unmarshal(source, &parsed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", workflowPath, err)
	}
	entries := parsed.Jobs.Fuzz.Strategy.Matrix.Include
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: jobs.fuzz.strategy.matrix.include is empty or missing", workflowPath)
	}
	configured := map[target]bool{}
	for _, entry := range entries {
		if entry.Package == "" || entry.Target == "" {
			return nil, fmt.Errorf("%s: matrix entry needs both package and target, got %q/%q", workflowPath, entry.Package, entry.Target)
		}
		configured[target{pkg: entry.Package, name: entry.Target}] = true
	}
	return configured, nil
}

func makefilePackages(root fs.FS) ([]string, error) {
	source, err := fs.ReadFile(root, makefilePath)
	if err != nil {
		return nil, err
	}
	match := makefileFuzzPackages.FindSubmatch(source)
	if match == nil {
		return nil, fmt.Errorf("%s: could not find the package list of the fuzz target", makefilePath)
	}
	return strings.Fields(string(match[1])), nil
}

func compareTargets(declared, configured map[target]bool) []string {
	var problems []string
	for _, missing := range sortedDifference(configured, declared) {
		problems = append(problems, fmt.Sprintf(
			"%s runs %s in %s, but no such fuzz target exists; go test -fuzz would silently pass without fuzzing anything",
			workflowPath, missing.name, missing.pkg))
	}
	for _, extra := range sortedDifference(declared, configured) {
		problems = append(problems, fmt.Sprintf(
			"%s declares fuzz target %s, but %s never runs it; add it to jobs.fuzz.strategy.matrix.include",
			extra.pkg, extra.name, workflowPath))
	}
	return problems
}

// comparePackages は make fuzz のパッケージ一覧を検査する。
// -fuzz=. はターゲットが無いパッケージを黙って通し、複数あるパッケージでは実行前に失敗する。
func comparePackages(declared map[target]bool, packages []string) []string {
	counts := map[string]int{}
	for item := range declared {
		counts[item.pkg]++
	}
	var problems []string
	listed := map[string]bool{}
	for _, pkg := range packages {
		listed[pkg] = true
		switch counts[pkg] {
		case 0:
			problems = append(problems, fmt.Sprintf(
				"make fuzz runs %s, but it declares no fuzz target; -fuzz=. would pass without fuzzing anything", pkg))
		case 1:
		default:
			problems = append(problems, fmt.Sprintf(
				"make fuzz runs %s, which declares %d fuzz targets; -fuzz=. refuses to run when it matches more than one", pkg, counts[pkg]))
		}
	}
	for _, pkg := range sortedKeys(counts) {
		if !listed[pkg] {
			problems = append(problems, fmt.Sprintf("%s declares a fuzz target, but make fuzz does not run it", pkg))
		}
	}
	return problems
}

func sortedDifference(from, without map[target]bool) []target {
	var difference []target
	for item := range from {
		if !without[item] {
			difference = append(difference, item)
		}
	}
	sort.Slice(difference, func(i, j int) bool { return difference[i].String() < difference[j].String() })
	return difference
}

func sortedKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
