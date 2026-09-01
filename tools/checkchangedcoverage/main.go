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
	"regexp"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/tools/internal/coverageconfig"
)

type coverageBlock struct {
	file       string
	start, end int
	statements int
	covered    bool
}

var hunkPattern = regexp.MustCompile(`^@@ -[0-9]+(?:,[0-9]+)? \+([0-9]+)(?:,([0-9]+))? @@`)

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(ctx context.Context, arguments []string, output, errorOutput io.Writer) int {
	if err := run(ctx, arguments, output); err != nil {
		_, _ = fmt.Fprintln(errorOutput, "changed coverage gate:", err)
		return 1
	}
	return 0
}

func run(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("checkchangedcoverage", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	profile := flags.String("profile", "", "Go coverage profile")
	exclusionsFile := flags.String("exclusions", "coverage-exclusions.txt", "coverage exclusion file with a reason for every exact path")
	base := flags.String("base", "origin/main", "Git base revision")
	minimum := flags.Float64("minimum", 90, "minimum changed executable statement coverage")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *profile == "" {
		return fmt.Errorf("-profile is required")
	}
	blocks, err := readCoverage(ctx, *profile)
	if err != nil {
		return err
	}
	changed, err := changedLines(ctx, *base)
	if err != nil {
		return err
	}
	exclusions, err := coverageconfig.Load(*exclusionsFile)
	if err != nil {
		return err
	}
	var total, covered int
	for _, block := range blocks {
		if coverageconfig.Matches(block.file, exclusions) {
			continue
		}
		lines := changed[filepath.Clean(block.file)]
		if !intersects(lines, block.start, block.end) {
			continue
		}
		total += block.statements
		if block.covered {
			covered += block.statements
		}
	}
	percentage := 100.0
	if total != 0 {
		percentage = float64(covered) * 100 / float64(total)
	}
	if _, err := fmt.Fprintf(output, "changed coverage: %.1f%% (%d/%d statements, minimum %.1f%%)\n", percentage, covered, total, *minimum); err != nil {
		return err
	}
	if percentage < *minimum {
		return fmt.Errorf("changed coverage %.1f%% is below minimum %.1f%%", percentage, *minimum)
	}
	return nil
}

func readCoverage(ctx context.Context, path string) ([]coverageBlock, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	module, err := modulePath(ctx)
	if err != nil {
		return nil, err
	}
	byLocation := map[string]coverageBlock{}
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
		location := fields[0]
		colon := strings.LastIndex(location, ":")
		if colon < 0 {
			return nil, fmt.Errorf("invalid coverage location %q", location)
		}
		fileName := strings.TrimPrefix(location[:colon], module+"/")
		var startLine, startColumn, endLine, endColumn int
		if _, err := fmt.Sscanf(location[colon+1:], "%d.%d,%d.%d", &startLine, &startColumn, &endLine, &endColumn); err != nil {
			return nil, err
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, err
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, err
		}
		key := fileName + ":" + location[colon+1:]
		previous := byLocation[key]
		byLocation[key] = coverageBlock{file: fileName, start: startLine, end: endLine, statements: statements, covered: previous.covered || count > 0}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	blocks := make([]coverageBlock, 0, len(byLocation))
	for _, block := range byLocation {
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func modulePath(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Path}}")
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func changedLines(ctx context.Context, base string) (map[string]map[int]bool, error) {
	rootCommand := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	rootOutput, err := rootCommand.Output()
	if err != nil {
		return nil, fmt.Errorf("locate Git worktree: %w", err)
	}
	command := exec.CommandContext(ctx, "git", "diff", "--unified=0", "--no-color", base+"...HEAD", "--", "*.go")
	command.Dir = strings.TrimSpace(string(rootOutput))
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return parseChangedLines(strings.NewReader(string(output)))
}

func parseChangedLines(input io.Reader) (map[string]map[int]bool, error) {
	changed := map[string]map[int]bool{}
	var file string
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "+++ b/") {
			file = filepath.Clean(strings.TrimPrefix(line, "+++ b/"))
			continue
		}
		match := hunkPattern.FindStringSubmatch(line)
		if match == nil || file == "" {
			continue
		}
		start, _ := strconv.Atoi(match[1])
		count := 1
		if match[2] != "" {
			count, _ = strconv.Atoi(match[2])
		}
		if changed[file] == nil {
			changed[file] = map[int]bool{}
		}
		for lineNumber := start; lineNumber < start+count; lineNumber++ {
			changed[file][lineNumber] = true
		}
	}
	return changed, scanner.Err()
}

func intersects(lines map[int]bool, start, end int) bool {
	for line := start; line <= end; line++ {
		if lines[line] {
			return true
		}
	}
	return false
}
