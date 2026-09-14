// mutationreport はGremlinsの結果を、mutation huntが扱うmanifestへ変換する。
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

const (
	manifestSchemaVersion = 1
	defaultExclusionsFile = "mutation-exclusions.txt"
)

var errSurvivors = errors.New("unexcluded mutation survivors found")

type gremlinsResult struct {
	GoModule          string         `json:"go_module"`
	Files             []gremlinsFile `json:"files"`
	MutantsTotal      int            `json:"mutants_total"`
	MutantsKilled     int            `json:"mutants_killed"`
	MutantsLived      int            `json:"mutants_lived"`
	MutantsNotViable  int            `json:"mutants_not_viable"`
	MutantsNotCovered int            `json:"mutants_not_covered"`
}

type gremlinsFile struct {
	Filename  string             `json:"file_name"`
	Mutations []gremlinsMutation `json:"mutations"`
}

type gremlinsMutation struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

type declaration struct {
	Path     string `json:"path"`
	Function string `json:"function"`
	Line     int    `json:"line"`
}

type survivor struct {
	Declaration declaration `json:"declaration"`
	Mutator     string      `json:"mutator"`
	Line        int         `json:"line"`
	Column      int         `json:"column"`
	Original    string      `json:"original"`
	Mutated     string      `json:"mutated"`
}

type excluded struct {
	Path     string `json:"path"`
	Function string `json:"function"`
	Reason   string `json:"reason"`
}

type totals struct {
	Mutants    int `json:"mutants"`
	Killed     int `json:"killed"`
	Lived      int `json:"lived"`
	NotCovered int `json:"not_covered"`
	NotViable  int `json:"not_viable"`
	TimedOut   int `json:"timed_out"`
}

type manifest struct {
	SchemaVersion int        `json:"schema_version"`
	Profile       string     `json:"profile"`
	RunID         string     `json:"run_id,omitempty"`
	RunAttempt    string     `json:"run_attempt,omitempty"`
	TestSHA       string     `json:"test_sha,omitempty"`
	Command       []string   `json:"command"`
	Totals        totals     `json:"totals"`
	Survivors     []survivor `json:"survivors"`
	Excluded      []excluded `json:"excluded"`
}

type exclusion struct {
	Path     string
	Function string
	Reason   string
}

type convertOptions struct {
	Root       string
	PackageDir string
	Profile    string
	Input      string
	Exclusions string
	RunID      string
	RunAttempt string
	TestSHA    string
	Command    []string
}

type sourceFile struct {
	path         string
	repository   string
	fileSet      *token.FileSet
	file         *ast.File
	declarations []declarationRange
}

type declarationRange struct {
	declaration declaration
	start       token.Pos
	end         token.Pos
}

var tokenMutations = map[string]map[string]string{
	"ARITHMETIC_BASE": {
		"+": "-", "*": "/", "/": "*", "%": "*", "-": "+",
	},
	"CONDITIONALS_BOUNDARY": {
		">=": ">", ">": ">=", "<=": "<", "<": "<=",
	},
	"CONDITIONALS_NEGATION": {
		"==": "!=", ">=": "<", ">": "<=", "<=": ">", "<": ">=", "!=": "==",
	},
	"INCREMENT_DECREMENT": {"--": "++", "++": "--"},
	"INVERT_ASSIGNMENTS": {
		"+=": "-=", "*=": "/=", "/=": "*=", "%=": "%=", "-=": "+=",
	},
	"INVERT_BITWISE": {
		"&": "|", "|": "&", "^": "&", "&^": "&", "<<": ">>", ">>": "<<",
	},
	"INVERT_BWASSIGN": {
		"&=": "|=", "|=": "&=", "^=": "&=", "&^=": "&=", "<<=": ">>=", ">>=": "<<=",
	},
	"INVERT_LOGICAL":   {"&&": "||", "||": "&&"},
	"INVERT_LOOPCTRL":  {"break": "continue", "continue": "break"},
	"INVERT_NEGATIVES": {"-": "+"},
	"REMOVE_SELF_ASSIGNMENTS": {
		"+=": "=", "&=": "=", "&^=": "=", "*=": "=", "|=": "=", "/=": "=", "%=": "=",
		"<<=": "=", ">>=": "=", "-=": "=", "^=": "=",
	},
}

func main() {
	if err := commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func commandMain(_ context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("mutationreport", flag.ContinueOnError)
	flags.SetOutput(errOut)
	root := flags.String("root", ".", "repository root")
	packageDir := flags.String("package-dir", "", "package directory relative to the repository root")
	profile := flags.String("profile", "", "package profile")
	input := flags.String("input", "", "Gremlins JSON result")
	output := flags.String("output", "-", "manifest output path, or - for stdout")
	exclusions := flags.String("exclusions", defaultExclusionsFile, "mutation exclusions file")
	runID := flags.String("run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run ID")
	runAttempt := flags.String("run-attempt", os.Getenv("GITHUB_RUN_ATTEMPT"), "workflow run attempt")
	testSHA := flags.String("test-sha", "", "source commit SHA; empty reads HEAD through internal/gitx")
	command := flags.String("command", "", "command recorded in the manifest")
	failOnSurvivors := flags.Bool("fail-on-survivors", false, "return an error after writing a manifest with survivors")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *input == "" {
		return errors.New("mutationreport: -input is required")
	}
	if *profile == "" {
		return errors.New("mutationreport: -profile is required")
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		return fmt.Errorf("read Gremlins result: %w", err)
	}
	var result gremlinsResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode Gremlins result: %w", err)
	}
	commands := strings.Fields(*command)
	value, err := buildManifest(convertOptions{
		Root: *root, PackageDir: *packageDir, Profile: *profile, Input: *input,
		Exclusions: *exclusions, RunID: *runID, RunAttempt: *runAttempt,
		TestSHA: *testSHA, Command: commands,
	}, result)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	encoded = append(encoded, '\n')
	if *output == "-" {
		if _, err := out.Write(encoded); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
			return fmt.Errorf("create manifest directory: %w", err)
		}
		if err := os.WriteFile(*output, encoded, 0o600); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
	}
	if *failOnSurvivors && len(value.Survivors) > 0 {
		return fmt.Errorf("%w: %d", errSurvivors, len(value.Survivors))
	}
	return nil
}

func buildManifest(options convertOptions, result gremlinsResult) (manifest, error) {
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return manifest{}, fmt.Errorf("resolve repository root: %w", err)
	}
	profile := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(options.Profile)), "./")
	if profile == "" || profile == "." || strings.HasPrefix(profile, "../") || profile == ".." || filepath.IsAbs(profile) {
		return manifest{}, fmt.Errorf("invalid profile %q", options.Profile)
	}
	packageDir, err := packageDirectory(root, options.PackageDir, profile)
	if err != nil {
		return manifest{}, err
	}
	exclusionsPath := options.Exclusions
	if exclusionsPath == "" {
		exclusionsPath = defaultExclusionsFile
	}
	if !filepath.IsAbs(exclusionsPath) {
		exclusionsPath = filepath.Join(root, exclusionsPath)
	}
	exclusions, err := loadExclusions(exclusionsPath)
	if err != nil {
		return manifest{}, err
	}
	activeExclusions := make(map[string]exclusion)
	for _, item := range exclusions {
		if exclusionApplies(item.Path, root, packageDir) {
			activeExclusions[item.Path+":"+item.Function] = item
		}
	}
	sources := make(map[string]*sourceFile)
	var survivors []survivor
	excludedByKey := make(map[string]excluded)
	timedOut := 0
	for _, listed := range result.Files {
		for _, mutation := range listed.Mutations {
			if mutation.Status == "TIMED OUT" {
				timedOut++
			}
			if mutation.Status != "LIVED" {
				continue
			}
			source, err := sourceFor(sources, root, packageDir, listed.Filename)
			if err != nil {
				return manifest{}, err
			}
			declaration, ok := source.declarationAtLine(mutation.Line)
			if !ok {
				return manifest{}, fmt.Errorf("%s:%d: no enclosing function declaration", source.repository, mutation.Line)
			}
			original, mutated, err := source.mutationTokens(mutation)
			if err != nil {
				return manifest{}, fmt.Errorf("%s:%d:%d: %w", source.repository, mutation.Line, mutation.Column, err)
			}
			key := declaration.Path + ":" + declaration.Function
			if item, ok := activeExclusions[key]; ok {
				excludedByKey[key] = excluded{Path: item.Path, Function: item.Function, Reason: item.Reason}
				continue
			}
			survivors = append(survivors, survivor{
				Declaration: declaration,
				Mutator:     mutation.Type,
				Line:        mutation.Line, Column: mutation.Column,
				Original: original, Mutated: mutated,
			})
		}
	}
	for key := range activeExclusions {
		path, function, ok := strings.Cut(key, ":")
		if !ok {
			return manifest{}, fmt.Errorf("invalid exclusion key %q", key)
		}
		source, err := sourceForRepositoryPath(sources, root, path)
		if err != nil {
			return manifest{}, fmt.Errorf("%s: stale exclusion for %s: %w", options.Exclusions, key, err)
		}
		if !source.hasFunction(function) {
			return manifest{}, fmt.Errorf("%s: stale exclusion for %s", options.Exclusions, key)
		}
	}
	survivors = sortSurvivors(survivors)
	if survivors == nil {
		survivors = []survivor{}
	}
	ignored := make([]excluded, 0, len(excludedByKey))
	for _, item := range excludedByKey {
		ignored = append(ignored, item)
	}
	sort.Slice(ignored, func(i, j int) bool {
		if ignored[i].Path != ignored[j].Path {
			return ignored[i].Path < ignored[j].Path
		}
		return ignored[i].Function < ignored[j].Function
	})
	sha := options.TestSHA
	if sha == "" {
		sha = gitSHA(root)
	}
	command := append([]string(nil), options.Command...)
	if len(command) == 0 {
		command = []string{"gremlins", "unleash", "./" + profile}
	}
	return manifest{
		SchemaVersion: manifestSchemaVersion,
		Profile:       profile,
		RunID:         options.RunID, RunAttempt: options.RunAttempt, TestSHA: sha,
		Command: command,
		Totals: totals{
			Mutants: result.MutantsTotal, Killed: result.MutantsKilled,
			Lived: result.MutantsLived, NotCovered: result.MutantsNotCovered,
			NotViable: result.MutantsNotViable, TimedOut: timedOut,
		},
		Survivors: survivors, Excluded: ignored,
	}, nil
}

func packageDirectory(root, configured, profile string) (string, error) {
	value := configured
	if value == "" {
		value = profile
	}
	if filepath.IsAbs(value) {
		return containedDirectory(root, value)
	}
	value = filepath.FromSlash(strings.TrimPrefix(value, "./"))
	if value == "" || value == "." {
		return root, nil
	}
	return containedDirectory(root, filepath.Join(root, value))
}

func containedDirectory(root, target string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, targetAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("package directory %q is outside repository root", target)
	}
	return targetAbs, nil
}

func sourceFor(cache map[string]*sourceFile, root, packageDir, fileName string) (*sourceFile, error) {
	clean := filepath.Clean(filepath.FromSlash(fileName))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is not a package-relative source path", fileName)
	}
	return sourceForPath(cache, root, filepath.Join(packageDir, clean))
}

func sourceForRepositoryPath(cache map[string]*sourceFile, root, repository string) (*sourceFile, error) {
	clean := filepath.Clean(filepath.FromSlash(repository))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s is not a repository-relative source path", repository)
	}
	return sourceForPath(cache, root, filepath.Join(root, clean))
}

func sourceForPath(cache map[string]*sourceFile, root, path string) (*sourceFile, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	repository, err := repositoryRelative(root, abs)
	if err != nil {
		return nil, err
	}
	if source := cache[repository]; source != nil {
		return source, nil
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, abs, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", repository, err)
	}
	source := &sourceFile{path: abs, repository: repository, fileSet: fileSet, file: file}
	for _, declarationNode := range file.Decls {
		function, ok := declarationNode.(*ast.FuncDecl)
		if !ok || function.Name == nil {
			continue
		}
		name := function.Name.Name
		if function.Recv != nil && len(function.Recv.List) > 0 {
			receiver, err := formatExpression(fileSet, function.Recv.List[0].Type)
			if err != nil {
				return nil, fmt.Errorf("format receiver in %s: %w", repository, err)
			}
			name = receiver + "." + name
		}
		position := fileSet.Position(function.Pos())
		source.declarations = append(source.declarations, declarationRange{
			declaration: declaration{Path: repository, Function: name, Line: position.Line},
			start:       function.Pos(), end: function.End(),
		})
	}
	cache[repository] = source
	return source, nil
}

func repositoryRelative(root, path string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside repository root", path)
	}
	return filepath.ToSlash(relative), nil
}

func (source *sourceFile) declarationAtLine(line int) (declaration, bool) {
	var found declarationRange
	foundRange := false
	for _, candidate := range source.declarations {
		start := source.fileSet.Position(candidate.start).Line
		end := source.fileSet.Position(candidate.end).Line
		if line < start || line > end {
			continue
		}
		if !foundRange || candidate.end-candidate.start < found.end-found.start {
			found, foundRange = candidate, true
		}
	}
	if !foundRange {
		return declaration{}, false
	}
	return found.declaration, true
}

func (source *sourceFile) hasFunction(name string) bool {
	for _, item := range source.declarations {
		if item.declaration.Function == name {
			return true
		}
	}
	return false
}

func (source *sourceFile) mutationTokens(mutation gremlinsMutation) (string, string, error) {
	var original string
	ast.Inspect(source.file, func(node ast.Node) bool {
		if original != "" || node == nil {
			return original == ""
		}
		var position token.Pos
		var value string
		switch typed := node.(type) {
		case *ast.AssignStmt:
			position, value = typed.TokPos, typed.Tok.String()
		case *ast.BinaryExpr:
			position, value = typed.OpPos, typed.Op.String()
		case *ast.BranchStmt:
			position, value = typed.TokPos, typed.Tok.String()
		case *ast.IncDecStmt:
			position, value = typed.TokPos, typed.Tok.String()
		case *ast.UnaryExpr:
			position, value = typed.OpPos, typed.Op.String()
		default:
			return true
		}
		location := source.fileSet.Position(position)
		if location.Line == mutation.Line && location.Column == mutation.Column {
			original = value
		}
		return original == ""
	})
	if original == "" {
		return "", "", errors.New("mutation token was not found at the reported position")
	}
	mutated := tokenMutations[mutation.Type][original]
	if mutated == "" {
		return "", "", fmt.Errorf("no token mapping for %s %q", mutation.Type, original)
	}
	return original, mutated, nil
}

func formatExpression(fileSet *token.FileSet, expression ast.Expr) (string, error) {
	var buffer bytes.Buffer
	if err := format.Node(&buffer, fileSet, expression); err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(buffer.String()), ""), nil
}

func loadExclusions(path string) ([]exclusion, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var result []exclusion
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("%s:%d: every entry needs path:function and a reason separated by one tab", path, lineNumber)
		}
		key := strings.TrimSpace(fields[0])
		separator := strings.LastIndexByte(key, ':')
		if separator <= 0 || separator == len(key)-1 {
			return nil, fmt.Errorf("%s:%d: target must be <repository path>:<function>", path, lineNumber)
		}
		relative := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(key[:separator]))))
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") || filepath.IsAbs(relative) {
			return nil, fmt.Errorf("%s:%d: target path must be repository-relative", path, lineNumber)
		}
		function := strings.TrimSpace(key[separator+1:])
		if function == "" {
			return nil, fmt.Errorf("%s:%d: function is required", path, lineNumber)
		}
		exclusion := exclusion{Path: relative, Function: function, Reason: strings.TrimSpace(fields[1])}
		unique := exclusion.Path + ":" + exclusion.Function
		if seen[unique] {
			return nil, fmt.Errorf("%s:%d: duplicate exclusion %s", path, lineNumber, unique)
		}
		seen[unique] = true
		result = append(result, exclusion)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func exclusionApplies(path, root, packageDir string) bool {
	relativePackage, err := repositoryRelative(root, packageDir)
	if err != nil || relativePackage == "." {
		return err == nil
	}
	return path == relativePackage || strings.HasPrefix(path, relativePackage+"/")
}

func sortSurvivors(value []survivor) []survivor {
	sort.Slice(value, func(i, j int) bool {
		a, b := value[i], value[j]
		if a.Declaration.Path != b.Declaration.Path {
			return a.Declaration.Path < b.Declaration.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Mutator < b.Mutator
	})
	return value
}

func gitSHA(root string) string {
	result, err := (&gitx.Runner{}).Run(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}
