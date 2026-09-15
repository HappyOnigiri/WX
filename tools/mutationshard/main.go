// mutationshard はGremlinsのdry-run結果を、ファイル単位のLPT shardへ分ける。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

type dryRunResult struct {
	Files []dryRunFile `json:"files"`
}

type dryRunFile struct {
	Filename  string           `json:"file_name"`
	Mutations []dryRunMutation `json:"mutations"`
}

type dryRunMutation struct {
	Status string `json:"status"`
}

// shardPlan はworkflowがそのままjqで読み取れる、選択ファイルと除外正規表現の組である。
type shardPlan struct {
	Files        []string `json:"files"`
	ExcludeFiles []string `json:"exclude_files"`
}

type weightedFile struct {
	Name   string
	Weight int
}

type weightedBucket struct {
	Names  []string
	Weight int
}

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(_ context.Context, args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("mutationshard", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	inputPath := flags.String("input", "", "Gremlins dry-run JSON result")
	flags.StringVar(inputPath, "result", "", "alias for -input")
	flags.StringVar(inputPath, "dry-run", "", "alias for -input")
	bucketCount := flags.Int("count", 0, "number of shards")
	bucketIndex := flags.Int("index", -1, "zero-based shard index")
	outputPath := flags.String("output", "-", "JSON output path, or - for stdout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if strings.TrimSpace(*inputPath) == "" {
		_, _ = fmt.Fprintln(errorOutput, "mutationshard: -input is required")
		return 2
	}
	if err := validateBuckets(*bucketCount, *bucketIndex); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: %v\n", err)
		return 2
	}
	data, err := os.ReadFile(*inputPath)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: read dry-run result: %v\n", err)
		return 1
	}
	var result dryRunResult
	if err := json.Unmarshal(data, &result); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: decode dry-run result: %v\n", err)
		return 1
	}
	value, err := buildPlan(result, *bucketCount, *bucketIndex)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: %v\n", err)
		return 1
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: encode plan: %v\n", err)
		return 1
	}
	encoded = append(encoded, '\n')
	if *outputPath == "-" {
		if _, err := output.Write(encoded); err != nil {
			_, _ = fmt.Fprintf(errorOutput, "mutationshard: write plan: %v\n", err)
			return 1
		}
		return 0
	}
	if err := os.MkdirAll(filepath.Dir(*outputPath), 0o755); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: create output directory: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*outputPath, encoded, 0o600); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "mutationshard: write plan: %v\n", err)
		return 1
	}
	return 0
}

func validateBuckets(count, index int) error {
	if count <= 0 {
		return fmt.Errorf("shard count must be positive, got %d", count)
	}
	if index < 0 || index >= count {
		return fmt.Errorf("shard index %d is outside 0..%d", index, count-1)
	}
	return nil
}

func buildPlan(result dryRunResult, count, index int) (shardPlan, error) {
	if err := validateBuckets(count, index); err != nil {
		return shardPlan{}, err
	}
	if len(result.Files) == 0 {
		return shardPlan{}, errors.New("dry-run result contains no files")
	}
	seen := make(map[string]bool, len(result.Files))
	items := make([]weightedFile, 0, len(result.Files))
	allFiles := make([]string, 0, len(result.Files))
	for _, file := range result.Files {
		name, err := packageRelativePath(file.Filename)
		if err != nil {
			return shardPlan{}, fmt.Errorf("invalid file %q: %w", file.Filename, err)
		}
		if seen[name] {
			return shardPlan{}, fmt.Errorf("file %s appears more than once", name)
		}
		seen[name] = true
		allFiles = append(allFiles, name)
		weight := 0
		for _, mutation := range file.Mutations {
			if mutation.Status == "RUNNABLE" {
				weight++
			}
		}
		if weight > 0 {
			items = append(items, weightedFile{Name: name, Weight: weight})
		}
	}
	if len(items) == 0 {
		return shardPlan{}, errors.New("dry-run result contains no RUNNABLE mutations")
	}
	sort.Strings(allFiles)
	buckets := assignBuckets(items, count)
	for bucketIndex, bucket := range buckets {
		if len(bucket.Names) == 0 {
			return shardPlan{}, fmt.Errorf("shard %d of %d has no files", bucketIndex, count)
		}
	}
	selected := append([]string(nil), buckets[index].Names...)
	sort.Strings(selected)
	selectedSet := make(map[string]bool, len(selected))
	for _, name := range selected {
		selectedSet[name] = true
	}
	excluded := make([]string, 0, len(allFiles)-len(selected))
	for _, name := range allFiles {
		if !selectedSet[name] {
			excluded = append(excluded, filepathRegexp(name))
		}
	}
	return shardPlan{Files: selected, ExcludeFiles: excluded}, nil
}

func assignBuckets(items []weightedFile, count int) []weightedBucket {
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
			if buckets[candidate].Weight < buckets[bucketIndex].Weight {
				bucketIndex = candidate
			}
		}
		buckets[bucketIndex].Names = append(buckets[bucketIndex].Names, item.Name)
		buckets[bucketIndex].Weight += item.Weight
	}
	return buckets
}

func packageRelativePath(value string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || strings.IndexFunc(trimmed, unicode.IsControl) >= 0 {
		return "", errors.New("path must be a non-empty relative path")
	}
	parts := strings.Split(trimmed, "/")
	for _, part := range parts {
		if part == ".." {
			return "", errors.New("path must not contain ..")
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(trimmed)))
	if clean == "." || clean == "" || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must be a relative file path")
	}
	return clean, nil
}

func filepathRegexp(name string) string {
	return `(^|/)` + regexp.QuoteMeta(name) + `$`
}
