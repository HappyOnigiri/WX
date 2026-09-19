// mutationreport はGremlinsの結果を、mutation huntが扱うmanifestへ変換する。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

const (
	manifestSchemaVersion   = 3
	mutationIDVersion       = "wx-mutation-id-v1"
	exclusionKeyVersion     = "ast-v1"
	defaultExclusionsFile   = "mutation-exclusions.txt"
	packageScopeDeclaration = "<package>"
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
	ID          string      `json:"id"`
	Declaration declaration `json:"declaration"`
	Mutator     string      `json:"mutator"`
	Line        int         `json:"line"`
	Column      int         `json:"column"`
	Original    string      `json:"original"`
	Mutated     string      `json:"mutated"`
}

type excluded struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Function string `json:"function"`
	Mutator  string `json:"mutator"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	Status   string `json:"status"`
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
	SchemaVersion   int        `json:"schema_version"`
	Profile         string     `json:"profile"`
	RunID           string     `json:"run_id,omitempty"`
	RunAttempt      string     `json:"run_attempt,omitempty"`
	TestSHA         string     `json:"test_sha,omitempty"`
	DurationSeconds float64    `json:"duration_seconds,omitempty"`
	Command         []string   `json:"command"`
	Totals          totals     `json:"totals"`
	Survivors       []survivor `json:"survivors"`
	Excluded        []excluded `json:"excluded"`
}

type convertOptions struct {
	Root            string
	PackageDir      string
	Profile         string
	Input           string
	Exclusions      string
	ShardFiles      []string
	RunID           string
	RunAttempt      string
	TestSHA         string
	DurationSeconds float64
	Command         []string
	TargetFile      string
	MutationID      string
	MutatorSet      string
}

type stringListFlag []string

func (value *stringListFlag) String() string {
	return strings.Join(*value, " ")
}

func (value *stringListFlag) Set(item string) error {
	*value = append(*value, item)
	return nil
}

type mutationRecord struct {
	ID           string
	ExclusionKey string
	Declaration  declaration
	Mutator      string
	Line         int
	Column       int
	Original     string
	Mutated      string
	Status       string
}

type sourceFile struct {
	path         string
	repository   string
	data         []byte
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
	emptyResult := flags.Bool("empty-result", false, "generate an empty manifest without a Gremlins result (mutually exclusive with -input)")
	output := flags.String("output", "-", "manifest output path, or - for stdout")
	exclusions := flags.String("exclusions", defaultExclusionsFile, "mutation exclusions file")
	validateExclusions := flags.Bool("validate-exclusions", false, "validate all exclusion keys against source and exit")
	var shardFiles stringListFlag
	flags.Var(&shardFiles, "shard-files", "repository-relative files assigned to this shard (repeatable)")
	flags.Var(&shardFiles, "files", "alias for -shard-files")
	runID := flags.String("run-id", os.Getenv("GITHUB_RUN_ID"), "workflow run ID")
	runAttempt := flags.String("run-attempt", os.Getenv("GITHUB_RUN_ATTEMPT"), "workflow run attempt")
	testSHA := flags.String("test-sha", "", "source commit SHA; empty reads HEAD through internal/gitx")
	durationSeconds := flags.Float64("duration-seconds", 0, "Gremlins execution duration in seconds; zero means unmeasured")
	command := flags.String("command", "", "command recorded in the manifest")
	targetFile := flags.String("file", "", "repository-relative file to validate")
	targetFileAlias := flags.String("target-file", "", "alias for -file")
	mutationID := flags.String("mutation-id", "", "mutation ID to validate")
	mutationIDAlias := flags.String("id", "", "alias for -mutation-id")
	failOnSurvivors := flags.Bool("fail-on-survivors", false, "return an error after writing a manifest with survivors")
	mutatorSet := flags.String("mutators", "default", "selected mutator set: boundary or default")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *validateExclusions {
		if *input != "" || *emptyResult || *profile != "" {
			return errors.New("mutationreport: -validate-exclusions cannot be combined with result conversion")
		}
		rootPath, err := filepath.Abs(*root)
		if err != nil {
			return fmt.Errorf("resolve repository root: %w", err)
		}
		exclusionsPath := *exclusions
		if !filepath.IsAbs(exclusionsPath) {
			exclusionsPath = filepath.Join(rootPath, exclusionsPath)
		}
		items, err := loadExclusions(exclusionsPath)
		if err != nil {
			return err
		}
		if _, err := resolveExclusions(rootPath, items); err != nil {
			return fmt.Errorf("validate exclusions: %w", err)
		}
		_, err = fmt.Fprintf(out, "validated %d mutation exclusion(s)\n", len(items))
		return err
	}
	if (*input == "") != *emptyResult {
		return errors.New("mutationreport: exactly one of -input and -empty-result is required")
	}
	if *profile == "" {
		return errors.New("mutationreport: -profile is required")
	}
	if !validDurationSeconds(*durationSeconds) {
		return fmt.Errorf("mutationreport: -duration-seconds must be a finite non-negative number, got %v", *durationSeconds)
	}
	if *targetFile != "" && *targetFileAlias != "" && *targetFile != *targetFileAlias {
		return errors.New("mutationreport: -file and -target-file disagree")
	}
	if *mutationID != "" && *mutationIDAlias != "" && *mutationID != *mutationIDAlias {
		return errors.New("mutationreport: -mutation-id and -id disagree")
	}
	if *targetFile == "" {
		*targetFile = *targetFileAlias
	}
	if *mutationID == "" {
		*mutationID = *mutationIDAlias
	}
	if *targetFile != "" && *mutationID != "" {
		return errors.New("mutationreport: -file and -mutation-id cannot be used together")
	}
	var result gremlinsResult
	if !*emptyResult {
		data, err := os.ReadFile(*input)
		if err != nil {
			return fmt.Errorf("read Gremlins result: %w", err)
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode Gremlins result: %w", err)
		}
	}
	commands := strings.Fields(*command)
	value, err := buildManifest(convertOptions{
		Root: *root, PackageDir: *packageDir, Profile: *profile, Input: *input,
		Exclusions: *exclusions, ShardFiles: shardFiles, RunID: *runID, RunAttempt: *runAttempt,
		TestSHA: *testSHA, DurationSeconds: *durationSeconds, Command: commands, TargetFile: *targetFile, MutationID: *mutationID,
		MutatorSet: *mutatorSet,
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
	if !validDurationSeconds(options.DurationSeconds) {
		return manifest{}, fmt.Errorf("invalid duration_seconds %v", options.DurationSeconds)
	}
	if options.MutatorSet != "" && options.MutatorSet != "default" && options.MutatorSet != "boundary" {
		return manifest{}, fmt.Errorf("invalid mutator set %q", options.MutatorSet)
	}
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
	shardFiles, err := normalizeShardFiles(root, packageDir, options.ShardFiles)
	if err != nil {
		return manifest{}, err
	}
	targetFile := ""
	if options.TargetFile != "" {
		targetFile, err = repositoryTargetPath(root, options.TargetFile)
		if err != nil {
			return manifest{}, err
		}
	}
	targetID := strings.TrimSpace(options.MutationID)
	if targetID != "" && !validMutationID(targetID) {
		return manifest{}, fmt.Errorf("invalid mutation ID %q", options.MutationID)
	}
	if targetFile != "" && targetID != "" {
		return manifest{}, errors.New("file and mutation ID cannot be used together")
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
	resolvedExclusions, err := resolveExclusions(root, exclusions)
	if err != nil {
		return manifest{}, fmt.Errorf("validate exclusions: %w", err)
	}
	activeExclusions := make(map[string]exclusion)
	for _, item := range exclusions {
		resolved := resolvedExclusions[item.Key]
		if exclusionApplies(item.Path, root, packageDir) && (len(shardFiles) == 0 || shardFiles[item.Path]) && mutatorSetIncludes(options.MutatorSet, resolved.Mutator) {
			activeExclusions[resolved.ID] = item
		}
	}
	collection, err := collectMutationRecords(root, packageDir, result)
	if err != nil {
		return manifest{}, err
	}
	if err := validateShardResults(collection, shardFiles); err != nil {
		return manifest{}, err
	}
	if err := validateActiveExclusions(exclusionsPath, activeExclusions, collection.byID); err != nil {
		return manifest{}, err
	}
	selected, err := selectRecords(collection, targetFile, targetID)
	if err != nil {
		return manifest{}, err
	}
	survivors, ignored := splitRecords(selected, activeExclusions)
	survivors = sortSurvivors(survivors)
	if survivors == nil {
		survivors = []survivor{}
	}
	if ignored == nil {
		ignored = []excluded{}
	}
	sort.Slice(ignored, func(i, j int) bool {
		if ignored[i].Path != ignored[j].Path {
			return ignored[i].Path < ignored[j].Path
		}
		return ignored[i].ID < ignored[j].ID
	})
	selectedTotals := countRecords(selected)
	allTotals := countRecords(collection.records)
	if targetFile == "" && targetID == "" {
		selectedTotals.Mutants = result.MutantsTotal
		selectedTotals.Killed = result.MutantsKilled
		selectedTotals.Lived = result.MutantsLived
		selectedTotals.NotCovered = result.MutantsNotCovered
		selectedTotals.NotViable = result.MutantsNotViable
		selectedTotals.TimedOut = allTotals.TimedOut
	}
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
		DurationSeconds: options.DurationSeconds,
		Command:         command,
		Totals: totals{
			Mutants: selectedTotals.Mutants, Killed: selectedTotals.Killed,
			Lived: selectedTotals.Lived, NotCovered: selectedTotals.NotCovered,
			NotViable: selectedTotals.NotViable, TimedOut: selectedTotals.TimedOut,
		},
		Survivors: survivors, Excluded: ignored,
	}, nil
}

func validDurationSeconds(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

type mutationCollection struct {
	records     []mutationRecord
	byID        map[string]mutationRecord
	listedPaths map[string]bool
}

func parseShardFiles(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == ',' })
}

func normalizeShardFiles(root, packageDir string, values []string) (map[string]bool, error) {
	if len(values) == 0 {
		return nil, nil
	}
	relativePackage, err := repositoryRelative(root, packageDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(values))
	for _, raw := range values {
		parts := parseShardFiles(raw)
		if len(parts) == 0 {
			return nil, fmt.Errorf("invalid shard file %q: path is required", raw)
		}
		for _, value := range parts {
			path, err := repositoryPath(value)
			if err != nil {
				return nil, fmt.Errorf("invalid shard file %q: %w", value, err)
			}
			if relativePackage != "." && path != relativePackage && !strings.HasPrefix(path, relativePackage+"/") {
				return nil, fmt.Errorf("shard file %s is outside package %s", path, relativePackage)
			}
			if result[path] {
				return nil, fmt.Errorf("shard file %s is listed more than once", path)
			}
			result[path] = true
		}
	}
	return result, nil
}

func validateShardResults(collection mutationCollection, shardFiles map[string]bool) error {
	if len(shardFiles) == 0 {
		return nil
	}
	paths := make([]string, 0, len(collection.listedPaths))
	for path := range collection.listedPaths {
		if !shardFiles[path] {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	return fmt.Errorf("Gremlins result contains files outside shard: %s", strings.Join(paths, ", "))
}

func collectMutationRecords(root, packageDir string, result gremlinsResult) (mutationCollection, error) {
	collection := mutationCollection{
		byID:        make(map[string]mutationRecord),
		listedPaths: make(map[string]bool),
	}
	sources := make(map[string]*sourceFile)
	for _, listed := range result.Files {
		source, err := sourceFor(sources, root, packageDir, listed.Filename)
		if err != nil {
			return mutationCollection{}, err
		}
		collection.listedPaths[source.repository] = true
		for _, mutation := range listed.Mutations {
			record, err := mutationRecordFor(source, mutation)
			if err != nil {
				return mutationCollection{}, err
			}
			if previous, exists := collection.byID[record.ID]; exists {
				if !sameMutationRecord(previous, record) {
					return mutationCollection{}, fmt.Errorf("duplicate mutation ID %s has conflicting mutation details", record.ID)
				}
				if previous.Status != record.Status {
					return mutationCollection{}, fmt.Errorf("duplicate mutation ID %s has conflicting statuses", record.ID)
				}
				return mutationCollection{}, fmt.Errorf("duplicate mutation ID %s appears more than once", record.ID)
			}
			collection.byID[record.ID] = record
			collection.records = append(collection.records, record)
		}
	}
	return collection, nil
}

func mutationRecordFor(source *sourceFile, mutation gremlinsMutation) (mutationRecord, error) {
	if !knownMutationStatus(mutation.Status) {
		return mutationRecord{}, fmt.Errorf("unsupported mutation status %q", mutation.Status)
	}
	declaration, ok := source.declarationAtLine(mutation.Line)
	if !ok {
		return mutationRecord{}, fmt.Errorf("%s:%d: no enclosing function declaration", source.repository, mutation.Line)
	}
	original, mutated, err := source.mutationTokens(mutation)
	if err != nil {
		return mutationRecord{}, fmt.Errorf("%s:%d:%d: %w", source.repository, mutation.Line, mutation.Column, err)
	}
	exclusionKey, err := source.exclusionKey(mutation.Line, mutation.Column, mutation.Type, original, mutated)
	if err != nil {
		return mutationRecord{}, fmt.Errorf("%s:%d:%d: %w", source.repository, mutation.Line, mutation.Column, err)
	}
	return mutationRecord{
		ID:           mutationID(declaration.Path, declaration.Function, mutation.Type, mutation.Line, mutation.Column, original, mutated),
		ExclusionKey: exclusionKey,
		Declaration:  declaration, Mutator: mutation.Type, Line: mutation.Line, Column: mutation.Column,
		Original: original, Mutated: mutated, Status: mutation.Status,
	}, nil
}

func validateActiveExclusions(path string, active map[string]exclusion, records map[string]mutationRecord) error {
	for id, item := range active {
		record, ok := records[id]
		if !ok {
			return fmt.Errorf("%s: exclusion %s resolved to mutation %s, which is absent from the selected result", path, item.Key, id)
		}
		if record.Declaration.Path != item.Path || record.ExclusionKey != item.Key {
			return fmt.Errorf("%s: exclusion %s does not match path %s", path, id, item.Path)
		}
	}
	return nil
}

func selectRecords(collection mutationCollection, targetFile, targetID string) ([]mutationRecord, error) {
	selected := collection.records
	if targetFile != "" {
		if !collection.listedPaths[targetFile] {
			return nil, fmt.Errorf("target file %s was not present in Gremlins results", targetFile)
		}
		selected = filterRecords(collection.records, func(record mutationRecord) bool { return record.Declaration.Path == targetFile })
	}
	if targetID == "" {
		return selected, nil
	}
	matches := filterRecords(collection.records, func(record mutationRecord) bool { return record.ID == targetID })
	if len(matches) == 0 {
		return nil, fmt.Errorf("mutation ID %s was not present in Gremlins results", targetID)
	}
	if len(matches) != 1 {
		return nil, fmt.Errorf("mutation ID %s matched %d results", targetID, len(matches))
	}
	if matches[0].Status != "KILLED" {
		return nil, fmt.Errorf("mutation ID %s has status %s; only KILLED is successful", targetID, matches[0].Status)
	}
	return matches, nil
}

func splitRecords(records []mutationRecord, exclusions map[string]exclusion) ([]survivor, []excluded) {
	var survivors []survivor
	var ignored []excluded
	for _, record := range records {
		if item, ok := exclusions[record.ID]; ok {
			ignored = append(ignored, excluded{
				ID: record.ID, Path: record.Declaration.Path, Function: record.Declaration.Function,
				Mutator: record.Mutator, Line: record.Line, Column: record.Column,
				Original: record.Original, Mutated: record.Mutated, Status: record.Status, Reason: item.Reason,
			})
			continue
		}
		if record.Status == "LIVED" {
			survivors = append(survivors, survivor{
				ID: record.ID, Declaration: record.Declaration, Mutator: record.Mutator,
				Line: record.Line, Column: record.Column, Original: record.Original, Mutated: record.Mutated,
			})
		}
	}
	return survivors, ignored
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
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", repository, err)
	}
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, abs, data, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", repository, err)
	}
	source := &sourceFile{path: abs, repository: repository, data: data, fileSet: fileSet, file: file}
	packagePosition := fileSet.Position(file.Pos())
	source.declarations = append(source.declarations, declarationRange{
		declaration: declaration{Path: repository, Function: packageScopeDeclaration, Line: packagePosition.Line},
		start:       file.Pos(), end: file.End(),
	})
	for _, declarationNode := range file.Decls {
		function, ok := declarationNode.(*ast.FuncDecl)
		if !ok || function.Name == nil {
			position := fileSet.Position(declarationNode.Pos())
			source.declarations = append(source.declarations, declarationRange{
				declaration: declaration{Path: repository, Function: packageScopeDeclaration, Line: position.Line},
				start:       declarationNode.Pos(), end: declarationNode.End(),
			})
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

func repositoryTargetPath(root, value string) (string, error) {
	clean, err := repositoryPath(value)
	if err != nil {
		return "", fmt.Errorf("invalid target file %q: %w", value, err)
	}
	return repositoryRelative(root, filepath.Join(root, filepath.FromSlash(clean)))
}

func repositoryPath(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", errors.New("path is required")
	}
	normalized := strings.ReplaceAll(trimmed, "\\", "/")
	if strings.HasPrefix(normalized, "/") || filepath.IsAbs(filepath.FromSlash(normalized)) {
		return "", errors.New("path must be repository-relative")
	}
	parts := strings.Split(normalized, "/")
	for _, part := range parts {
		if part == ".." {
			return "", errors.New("path must be repository-relative")
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(normalized)))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must be repository-relative")
	}
	return clean, nil
}

func (source *sourceFile) declarationAtLine(line int) (declaration, bool) {
	rangeValue, ok := source.declarationRangeAtLine(line)
	return rangeValue.declaration, ok
}

func (source *sourceFile) declarationRangeAtLine(line int) (declarationRange, bool) {
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
		return declarationRange{}, false
	}
	return found, true
}

func mutationID(path, function, mutator string, line, column int, original, mutated string) string {
	canonical := strings.Join([]string{
		mutationIDVersion, path, function, mutator,
		strconv.Itoa(line), strconv.Itoa(column), original, mutated,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func validMutationID(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func sameMutationRecord(a, b mutationRecord) bool {
	return a.ID == b.ID && a.Declaration == b.Declaration && a.Mutator == b.Mutator &&
		a.Line == b.Line && a.Column == b.Column && a.Original == b.Original && a.Mutated == b.Mutated
}

func filterRecords(records []mutationRecord, keep func(mutationRecord) bool) []mutationRecord {
	filtered := make([]mutationRecord, 0, len(records))
	for _, record := range records {
		if keep(record) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func countRecords(records []mutationRecord) totals {
	var result totals
	for _, record := range records {
		switch record.Status {
		case "KILLED":
			result.Mutants++
			result.Killed++
		case "LIVED":
			result.Mutants++
			result.Lived++
		case "NOT COVERED":
			result.NotCovered++
		case "NOT VIABLE":
			result.Mutants++
			result.NotViable++
		case "TIMED OUT":
			result.TimedOut++
		}
	}
	return result
}

func knownMutationStatus(status string) bool {
	switch status {
	case "KILLED", "LIVED", "NOT COVERED", "NOT VIABLE", "TIMED OUT":
		return true
	default:
		return false
	}
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
		if a.Mutator != b.Mutator {
			return a.Mutator < b.Mutator
		}
		return a.ID < b.ID
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
