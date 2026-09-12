// testshard は go test の対象を実測時間の重みで安定したbucketへ分ける。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const listCommandName = "go test -list"

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(ctx context.Context, args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("testshard", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	goCommand := flags.String("go", "go", "Go executable used to list tests")
	packageName := flags.String("package", "", "package passed to go test")
	packageList := flags.String("packages", "", "whitespace-separated packages for package mode")
	flags.StringVar(packageList, "package-list", "", "whitespace-separated packages for package mode")
	bucketCount := flags.Int("count", 0, "number of buckets")
	bucketIndex := flags.Int("index", -1, "zero-based bucket index")
	mode := flags.String("mode", "test", "split mode: test or package")
	flags.StringVar(mode, "kind", "test", "split mode: test or package")
	weightPath := flags.String("weights", "", "JSON file containing measured test and package weights")
	flags.StringVar(weightPath, "weight-file", "", "JSON file containing measured test and package weights")
	flags.StringVar(weightPath, "weight", "", "JSON file containing measured test and package weights")
	writeWeightsPath := flags.String("write-weights", "", "write weights collected from report JSONL files")
	flags.StringVar(writeWeightsPath, "generate-weights", "", "write weights collected from report JSONL files")
	reportsPath := flags.String("reports", "", "root containing citest report directories")
	flags.StringVar(reportsPath, "reports-dir", "", "root containing citest report directories")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *writeWeightsPath != "" {
		if flags.NArg() != 0 || strings.TrimSpace(*reportsPath) == "" {
			_, _ = fmt.Fprintln(errorOutput, "testshard: -reports is required and positional arguments are not allowed when writing weights")
			return 2
		}
		if err := writeWeights(*reportsPath, *writeWeightsPath); err != nil {
			_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
			return 1
		}
		return 0
	}
	*mode = canonicalMode(*mode)
	if flags.NArg() != 0 && *mode != "package" {
		_, _ = fmt.Fprintln(errorOutput, "testshard: unexpected positional arguments")
		return 2
	}
	if err := validateMode(*mode); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 2
	}
	if err := validateConfig(*goCommand, *packageName, *bucketCount, *bucketIndex); err != nil && *mode == "test" {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 2
	}
	if err := validateBuckets(*bucketCount, *bucketIndex); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 2
	}
	table, err := loadWeights(*weightPath)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 2
	}
	var selected []string
	switch *mode {
	case "test":
		names, listErr := listNames(ctx, *goCommand, *packageName)
		if listErr != nil {
			_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", listErr)
			return 1
		}
		if strings.TrimSpace(*weightPath) == "" {
			selected, err = selectBucket(names, *bucketCount, *bucketIndex)
		} else {
			selected, err = selectWeightedTestBucket(names, *packageName, *bucketCount, *bucketIndex, table)
		}
	case "package":
		packages, listErr := parsePackageInput(*packageList)
		if listErr != nil {
			_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", listErr)
			return 2
		}
		if len(packages) == 0 {
			packages, listErr = parsePackageInput(*packageName)
			if listErr != nil {
				_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", listErr)
				return 2
			}
		}
		for _, argument := range flags.Args() {
			argumentPackages, parseErr := parsePackageInput(argument)
			if parseErr != nil {
				_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", parseErr)
				return 2
			}
			packages = append(packages, argumentPackages...)
		}
		selected, err = selectWeightedPackageBucket(packages, *bucketCount, *bucketIndex, table)
	}
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 1
	}
	if *mode == "package" {
		_, _ = fmt.Fprintln(output, strings.Join(selected, "\n"))
	} else {
		_, _ = fmt.Fprintln(output, runPattern(selected))
	}
	return 0
}

func parsePackageInput(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if info, err := os.Stat(value); err == nil {
		if info.Mode().IsRegular() {
			data, readErr := os.ReadFile(value)
			if readErr != nil {
				return nil, fmt.Errorf("read package list %q: %w", value, readErr)
			}
			value = string(data)
		}
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == ',' })
	return fields, nil
}

func validateMode(mode string) error {
	switch canonicalMode(mode) {
	case "test", "package":
		return nil
	default:
		return fmt.Errorf("unknown split mode %q (want test or package)", mode)
	}
}

func canonicalMode(mode string) string {
	switch mode {
	case "tests":
		return "test"
	case "packages":
		return "package"
	default:
		return mode
	}
}

func validateConfig(goCommand, packageName string, count, index int) error {
	if strings.TrimSpace(goCommand) == "" || strings.IndexFunc(goCommand, unicode.IsControl) >= 0 {
		return errors.New("Go executable is required and must not contain control characters")
	}
	if strings.TrimSpace(packageName) == "" || strings.IndexFunc(packageName, unicode.IsControl) >= 0 {
		return errors.New("package is required and must not contain control characters")
	}
	return validateBuckets(count, index)
}

func validateBuckets(count, index int) error {
	if count <= 0 {
		return fmt.Errorf("bucket count must be positive, got %d", count)
	}
	if index < 0 || index >= count {
		return fmt.Errorf("bucket index %d is outside 0..%d", index, count-1)
	}
	return nil
}

func listNames(ctx context.Context, goCommand, packageName string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, goCommand, "test", "-list", ".", packageName)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", listCommandName, err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", listCommandName, err)
	}
	return parseListOutput(stdout.String())
}

func parseListOutput(output string) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for lineNumber, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if packageResultLine(line) {
			continue
		}
		if benchmarkName(line) {
			continue
		}
		if !testName(line) {
			return nil, fmt.Errorf("go test -list output line %d is not a top-level Test, Fuzz, or Example name: %q", lineNumber+1, line)
		}
		if seen[line] {
			return nil, fmt.Errorf("go test -list output contains duplicate test name %q", line)
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(names) == 0 {
		return nil, errors.New("go test -list returned no top-level Test, Fuzz, or Example names")
	}
	sort.Strings(names)
	return names, nil
}

func benchmarkName(name string) bool {
	return testPrefixName(name, "Benchmark")
}

func packageResultLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "ok", "?", "FAIL":
		return true
	default:
		return false
	}
}

func testName(name string) bool {
	return testPrefixName(name, "Test") || testPrefixName(name, "Fuzz") || testPrefixName(name, "Example")
}

func testPrefixName(name, prefix string) bool {
	if !token.IsIdentifier(name) || !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := name[len(prefix):]
	if suffix == "" {
		return true
	}
	runeValue, _ := utf8.DecodeRuneInString(suffix)
	return !unicode.IsLower(runeValue)
}

func selectBucket(names []string, count, index int, weights ...map[string]float64) ([]string, error) {
	if len(weights) > 1 {
		return nil, errors.New("at most one weight map is allowed")
	}
	if len(weights) == 1 {
		return selectWeightedBucket(names, count, index, func(name string) (float64, bool) {
			weight, ok := weights[0][name]
			return weight, ok
		}, medianWeightValues(weights[0]))
	}
	return selectHashBucket(names, count, index)
}

func medianWeightValues(weights map[string]float64) float64 {
	values := make([]float64, 0, len(weights))
	for _, weight := range weights {
		if validWeight(weight) {
			values = append(values, weight)
		}
	}
	return medianWeight(values)
}

// selectHashBucket は旧呼び出し側との互換用に残す。CLIの既定経路は重み付き分割である。
func selectHashBucket(names []string, count, index int) ([]string, error) {
	if err := validateBuckets(count, index); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("test name list is empty")
	}
	seen := make(map[string]bool, len(names))
	selected := make([]string, 0, (len(names)+count-1)/count)
	for _, name := range names {
		if !testName(name) {
			return nil, fmt.Errorf("invalid top-level test name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate test name %q", name)
		}
		seen[name] = true
		if bucketForName(name, count) == index {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("bucket %d of %d has no tests", index, count)
	}
	sort.Strings(selected)
	return selected, nil
}

// selectWeightedTestBucket は単一パッケージのトップレベルテストを分割する。
func selectWeightedTestBucket(names []string, packageName string, count, index int, table weightTable) ([]string, error) {
	return selectWeightedBucket(names, count, index, func(name string) (float64, bool) {
		return table.testWeight(packageName, name)
	}, table.testMedian())
}

// selectWeightedPackageBucket はパッケージ名を分割する。package modeでは
// go listの結果をそのまま入力とし、go test -listを実行しない。
func selectWeightedPackageBucket(packages []string, count, index int, table weightTable) ([]string, error) {
	if len(packages) == 0 {
		return nil, errors.New("package list is empty")
	}
	for _, packageName := range packages {
		if strings.TrimSpace(packageName) == "" || strings.IndexFunc(packageName, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid package name %q", packageName)
		}
	}
	return selectWeightedBucket(packages, count, index, table.packageWeight, table.packageMedian())
}

// selectWeightedBucket は重みの降順（同値なら名前順）で最も軽いbucketへ詰めるLPT。
// lookupに無い項目にはmedianを与え、入力順によらず同じ所属を返す。
func selectWeightedBucket(names []string, count, index int, lookup func(string) (float64, bool), median float64) ([]string, error) {
	if err := validateBuckets(count, index); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("item list is empty")
	}
	if median <= 0 || !validWeight(median) {
		median = 1
	}
	seen := make(map[string]bool, len(names))
	items := make([]weightedItem, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid item name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate item name %q", name)
		}
		seen[name] = true
		weight, ok := lookup(name)
		if !ok {
			weight = median
		}
		if !validWeight(weight) {
			return nil, fmt.Errorf("invalid weight for %q: %v", name, weight)
		}
		items = append(items, weightedItem{Name: name, Weight: weight})
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Weight != items[right].Weight {
			return items[left].Weight > items[right].Weight
		}
		return items[left].Name < items[right].Name
	})
	buckets := make([]weightedBucket, count)
	for _, item := range items {
		bucketIndex := 0
		for candidate := 1; candidate < len(buckets); candidate++ {
			if buckets[candidate].Total < buckets[bucketIndex].Total {
				bucketIndex = candidate
			}
		}
		buckets[bucketIndex].Names = append(buckets[bucketIndex].Names, item.Name)
		buckets[bucketIndex].Total += item.Weight
	}
	if len(buckets[index].Names) == 0 {
		return nil, fmt.Errorf("bucket %d of %d has no items", index, count)
	}
	selected := append([]string(nil), buckets[index].Names...)
	sort.Strings(selected)
	return selected, nil
}

type weightedItem struct {
	Name   string
	Weight float64
}

type weightedBucket struct {
	Names []string
	Total float64
}

func validWeight(weight float64) bool {
	return !math.IsNaN(weight) && !math.IsInf(weight, 0) && weight >= 0
}

// bucketForName はSHA-256の先頭8byteを名前のUTF-8列へ適用し、countで剰余を取る。
// ハッシュの入力と読み出し順を固定し、CPUアーキテクチャによる所属差を避ける。
func bucketForName(name string, count int) int {
	if count <= 0 {
		return -1
	}
	digest := sha256.Sum256([]byte(name))
	value := binary.BigEndian.Uint64(digest[:8])
	return int(value % uint64(count))
}

func runPattern(names []string) string {
	parts := make([]string, len(names))
	for index, name := range names {
		parts[index] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}
