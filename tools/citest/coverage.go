package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func preserveCoverage(cfg config, source, name string) error {
	profile, err := readCoverage(source)
	if err != nil {
		return fmt.Errorf("invalid coverage profile %s: %w", source, err)
	}
	return writeCoverage(filepath.Join(cfg.ReportDir, name), profile)
}

func ensureCoverage(path string) error {
	_, err := readCoverage(path)
	if err != nil {
		return fmt.Errorf("invalid coverage profile %s: %w", path, err)
	}
	return nil
}

func readCoverage(path string) (coverageProfile, error) {
	file, err := os.Open(path)
	if err != nil {
		return coverageProfile{}, err
	}
	defer func() { _ = file.Close() }()
	var profile coverageProfile
	seenMode := false
	blocks := make(map[string]int)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "mode:") {
			if seenMode || profile.Mode != "" {
				return coverageProfile{}, fmt.Errorf("duplicate coverage mode")
			}
			profile.Mode = strings.TrimSpace(strings.TrimPrefix(line, "mode:"))
			if profile.Mode == "" {
				return coverageProfile{}, fmt.Errorf("empty coverage mode")
			}
			seenMode = true
			continue
		}
		if !seenMode {
			return coverageProfile{}, fmt.Errorf("coverage mode must be first")
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return coverageProfile{}, fmt.Errorf("malformed line %q", line)
		}
		statements, err := strconv.Atoi(fields[1])
		if err != nil || statements < 0 {
			return coverageProfile{}, fmt.Errorf("invalid statement count %q", fields[1])
		}
		count, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || count < 0 {
			return coverageProfile{}, fmt.Errorf("invalid counter %q", fields[2])
		}
		location := fields[0]
		if index, ok := blocks[location]; ok {
			if profile.Blocks[index].Statements != statements {
				return coverageProfile{}, fmt.Errorf("statement count differs for %s", location)
			}
			profile.Blocks[index].Count += count
			continue
		}
		blocks[location] = len(profile.Blocks)
		profile.Blocks = append(profile.Blocks, coverageBlock{Location: location, Statements: statements, Count: count})
	}
	if err := scanner.Err(); err != nil {
		return coverageProfile{}, err
	}
	if !seenMode {
		return coverageProfile{}, fmt.Errorf("coverage mode is missing")
	}
	return profile, nil
}

func mergeCoverage(basePath, retryPath string) error {
	base, err := readCoverage(basePath)
	if err != nil {
		return err
	}
	retry, err := readCoverage(retryPath)
	if err != nil {
		return err
	}
	if base.Mode != retry.Mode {
		return fmt.Errorf("coverage modes differ: %s and %s", base.Mode, retry.Mode)
	}
	indices := make(map[string]int, len(base.Blocks))
	for index, block := range base.Blocks {
		indices[block.Location] = index
	}
	for _, block := range retry.Blocks {
		index, ok := indices[block.Location]
		if !ok {
			indices[block.Location] = len(base.Blocks)
			base.Blocks = append(base.Blocks, block)
			continue
		}
		if base.Blocks[index].Statements != block.Statements {
			return fmt.Errorf("statement count differs for %s", block.Location)
		}
		base.Blocks[index].Count += block.Count
	}
	return writeCoverage(basePath, base)
}

func writeCoverage(path string, profile coverageProfile) error {
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "mode: %s\n", profile.Mode)
	for _, block := range profile.Blocks {
		_, _ = fmt.Fprintf(&builder, "%s %d %d\n", block.Location, block.Statements, block.Count)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(builder.String()), 0o600)
}
