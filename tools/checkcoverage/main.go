package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type block struct {
	statements int
	covered    bool
}

// exclusion は coverage gate から除外する repository-relative path と、
// exclusions file に記録する reviewer 向け理由を表す。
type exclusion struct {
	path string
}

func loadExclusions(fileName string) ([]exclusion, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var exclusions []exclusion
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("invalid coverage exclusion %q: expected path and reason separated by a tab", line)
		}
		exclusions = append(exclusions, exclusion{path: filepath.ToSlash(filepath.Clean(fields[0]))})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return exclusions, nil
}

func excluded(fileName string, exclusions []exclusion) bool {
	fileName = filepath.ToSlash(filepath.Clean(fileName))
	for _, item := range exclusions {
		if fileName == item.path || strings.HasSuffix(fileName, "/"+item.path) {
			return true
		}
	}
	return false
}

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(ctx context.Context, arguments []string, output, errorOutput io.Writer) int {
	if err := run(ctx, arguments, output); err != nil {
		_, _ = fmt.Fprintln(errorOutput, "coverage gate:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("checkcoverage", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "", "Go coverage profile")
	exclusionsFile := flags.String("exclusions", "coverage-exclusions.txt", "coverage exclusion file with a reason for every exact path")
	overallMin := flags.Float64("overall", 85, "minimum whole-project line coverage")
	coreMin := flags.Float64("core", 90, "minimum core-package line coverage")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *profile == "" {
		return fmt.Errorf("-profile is required")
	}
	blocks, err := readProfile(*profile)
	if err != nil {
		return err
	}
	exclusions, err := loadExclusions(*exclusionsFile)
	if err != nil {
		return err
	}
	overall := percentage(blocks, "", exclusions)
	core := percentage(blocks, "/internal/config/|/internal/state/|/internal/pool/|/internal/gitx/|/internal/workspace/|/internal/archive/|/internal/rpc/", exclusions)
	if _, err := fmt.Fprintf(output, "coverage: overall %.1f%% (minimum %.1f%%), core %.1f%% (minimum %.1f%%)\n", overall, *overallMin, core, *coreMin); err != nil {
		return err
	}
	if overall < *overallMin || core < *coreMin {
		return fmt.Errorf("coverage thresholds were not met")
	}
	if err := rejectZeroCoreFunctions(ctx, *profile, exclusions); err != nil {
		return err
	}
	return nil
}

func readProfile(path string) (map[string]block, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	blocks := map[string]block{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid coverage line %q", line)
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, err
		}
		key := fields[0]
		old := blocks[key]
		blocks[key] = block{statements: statements, covered: old.covered || count > 0}
	}
	return blocks, scanner.Err()
}

func percentage(blocks map[string]block, corePatterns string, exclusions []exclusion) float64 {
	var total, covered int
	patterns := strings.Split(corePatterns, "|")
	for location, item := range blocks {
		file := strings.SplitN(location, ":", 2)[0]
		if strings.HasSuffix(file, "/migrations/embed.go") || excluded(file, exclusions) {
			continue
		}
		if corePatterns != "" {
			matched := false
			for _, pattern := range patterns {
				matched = matched || strings.Contains(file, pattern)
			}
			if !matched {
				continue
			}
		}
		total += item.statements
		if item.covered {
			covered += item.statements
		}
	}
	if total == 0 {
		return 0
	}
	return float64(covered) * 100 / float64(total)
}

func rejectZeroCoreFunctions(ctx context.Context, profile string, exclusions []exclusion) error {
	command := exec.CommandContext(ctx, "go", "tool", "cover", "-func="+profile)
	output, err := command.Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[len(fields)-1] != "0.0%" {
			continue
		}
		fileName := strings.SplitN(line, ":", 2)[0]
		if excluded(fileName, exclusions) {
			continue
		}
		if strings.Contains(line, "/internal/config/") || strings.Contains(line, "/internal/state/") || strings.Contains(line, "/internal/pool/") || strings.Contains(line, "/internal/gitx/") || strings.Contains(line, "/internal/workspace/") || strings.Contains(line, "/internal/archive/") || strings.Contains(line, "/internal/rpc/") {
			return fmt.Errorf("core function has zero coverage: %s", strings.TrimSpace(line))
		}
	}
	return nil
}
