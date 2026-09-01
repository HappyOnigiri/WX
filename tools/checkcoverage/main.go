package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type block struct {
	statements int
	covered    bool
}

func main() {
	profile := flag.String("profile", "", "Go coverage profile")
	overallMin := flag.Float64("overall", 85, "minimum whole-project line coverage")
	coreMin := flag.Float64("core", 90, "minimum core-package line coverage")
	flag.Parse()
	if *profile == "" {
		fatal("-profile is required")
	}
	blocks, err := readProfile(*profile)
	if err != nil {
		fatal(err.Error())
	}
	overall := percentage(blocks, "")
	core := percentage(blocks, "/internal/config/|/internal/state/|/internal/pool/|/internal/gitx/|/internal/workspace/|/internal/archive/|/internal/rpc/")
	fmt.Printf("coverage: overall %.1f%% (minimum %.1f%%), core %.1f%% (minimum %.1f%%)\n", overall, *overallMin, core, *coreMin)
	if overall < *overallMin || core < *coreMin {
		os.Exit(1)
	}
	if err := rejectZeroCoreFunctions(*profile); err != nil {
		fatal(err.Error())
	}
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

func percentage(blocks map[string]block, corePatterns string) float64 {
	var total, covered int
	patterns := strings.Split(corePatterns, "|")
	for location, item := range blocks {
		file := strings.SplitN(location, ":", 2)[0]
		if strings.HasSuffix(file, "/migrations/embed.go") {
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

func rejectZeroCoreFunctions(profile string) error {
	command := exec.Command("go", "tool", "cover", "-func="+profile)
	output, err := command.Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[len(fields)-1] != "0.0%" {
			continue
		}
		if strings.Contains(line, "/internal/config/") || strings.Contains(line, "/internal/state/") || strings.Contains(line, "/internal/pool/") || strings.Contains(line, "/internal/gitx/") || strings.Contains(line, "/internal/workspace/") || strings.Contains(line, "/internal/archive/") || strings.Contains(line, "/internal/rpc/") {
			return fmt.Errorf("core function has zero coverage: %s", strings.TrimSpace(line))
		}
	}
	return nil
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "coverage gate:", message)
	os.Exit(1)
}
