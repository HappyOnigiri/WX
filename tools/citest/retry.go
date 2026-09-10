package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

func resolveFailedDeclarations(ctx context.Context, cfg config, failed map[string][]string) (map[string]map[string]declaration, []string) {
	resolved := make(map[string]map[string]declaration)
	var diagnostics []string
	for packageName, tests := range failed {
		decls, err := declarations(ctx, cfg, packageName)
		if err != nil {
			diagnostics = append(diagnostics, fmt.Sprintf("%s: resolve test declarations: %v", packageName, err))
			continue
		}
		resolved[packageName] = make(map[string]declaration)
		for _, testName := range tests {
			root := rootTestName(testName)
			decl, ok := decls[root]
			if !ok {
				diagnostics = append(diagnostics, fmt.Sprintf("%s: no declaration for %s", packageName, root))
				continue
			}
			resolved[packageName][root] = decl
		}
	}
	sortStrings(diagnostics)
	return resolved, diagnostics
}

func retryFailures(ctx context.Context, cfg config, initial testResult, failed map[string][]string, resolved map[string]map[string]declaration, man *manifest, output interface{ Write([]byte) (int, error) }) string {
	status := "failed"
	recoveredNames := make(map[string]map[string]bool)
	totalNames := 0
	for packageName, names := range failed {
		totalNames += len(names)
		recoveredNames[packageName] = make(map[string]bool)
	}
	for packageName, names := range failed {
		roots := make([]string, 0, len(resolved[packageName]))
		for root := range resolved[packageName] {
			roots = append(roots, root)
		}
		sortStrings(roots)
		if len(roots) == 0 {
			continue
		}
		command, coverage, err := retryCommand(cfg, packageName, roots, initial.ShuffleByPackage[packageName], len(man.Retries))
		retry := retryRecord{
			Package:     packageName,
			Functions:   roots,
			Command:     command,
			StartedAt:   now(),
			Coverage:    coverage,
			FailedTests: append([]string(nil), names...),
		}
		if err != nil {
			retry.Reason = err.Error()
			man.Retries = append(man.Retries, retry)
			continue
		}
		result, executeErr := executeRun(ctx, cfg, command, fmt.Sprintf("retry-%02d-%s", len(man.Retries)+1, safeName(packageName)), coverage, output)
		retry.FinishedAt = now()
		retry.DurationMS = durationMS(retry.StartedAt, retry.FinishedAt)
		retry.Exit = result.Exit
		retry.Status = result.Status
		retry.JSONL = filepath.Base(filepath.Join(cfg.ReportDir, fmt.Sprintf("retry-%02d-%s.jsonl", len(man.Retries)+1, safeName(packageName))))
		retry.Log = filepath.Base(filepath.Join(cfg.ReportDir, fmt.Sprintf("retry-%02d-%s.log", len(man.Retries)+1, safeName(packageName))))
		retry.Stderr = filepath.Base(filepath.Join(cfg.ReportDir, fmt.Sprintf("retry-%02d-%s.stderr", len(man.Retries)+1, safeName(packageName))))
		retry.Shuffle = result.Shuffle
		retry.LogExcerpt = result.LogExcerpt
		if executeErr != nil {
			retry.Reason = executeErr.Error()
		} else if recovered, reason := retryRecovered(result, packageName, names); recovered {
			retry.Recovered = true
			retry.PassedTests = append([]string(nil), names...)
			for _, name := range names {
				recoveredNames[packageName][name] = true
			}
			if coverage != "" {
				if err := mergeCoverage(cfg.CoverageProfile, filepath.Join(cfg.ReportDir, coverage)); err != nil {
					retry.Recovered = false
					retry.Reason = "coverage merge failed: " + err.Error()
				}
			}
			if retry.Recovered {
				for root, declaration := range resolved[packageName] {
					man.Recoveries = append(man.Recoveries, recovery{
						Package:       packageName,
						Declaration:   declaration,
						FailedTests:   failedNamesForRoot(names, root),
						InitialResult: "fail",
						RetryResult:   "pass",
						RetryIndex:    len(man.Retries) + 1,
					})
				}
			}
		} else {
			retry.Reason = reason
		}
		man.Retries = append(man.Retries, retry)
	}
	recoveredCount := 0
	for _, names := range recoveredNames {
		for _, recovered := range names {
			if recovered {
				recoveredCount++
			}
		}
	}
	if totalNames > 0 && recoveredCount == totalNames {
		status = "passed"
	}
	return status
}

func retryRecovered(result testResult, packageName string, expected []string) (bool, string) {
	if result.Exit != 0 {
		return false, fmt.Sprintf("retry exited with status %d", result.Exit)
	}
	if result.Anomaly != "" {
		return false, result.Anomaly
	}
	failed := failedTests(result)
	if len(failed[packageName]) > 0 {
		return false, "retry reported another named test failure"
	}
	for _, name := range expected {
		events := result.Tests[packageName+"\x00"+name]
		if !hasAction(events, "run") || !hasAction(events, "pass") || hasAction(events, "skip") {
			return false, "retry did not pass every originally failed test"
		}
	}
	return true, ""
}

func retryCommand(cfg config, packageName string, roots []string, shuffle string, index int) ([]string, string, error) {
	command, err := commandWithJSON(cfg.Command)
	if err != nil {
		return nil, "", err
	}
	args := append([]string(nil), command...)
	args = replaceRun(args, roots)
	args = replacePackages(args, packageName)
	if shuffle != "" {
		args = replaceShuffle(args, shuffle)
	}
	coverage := ""
	if cfg.CoverageProfile != "" {
		coverage = fmt.Sprintf("retry-%02d-%s.out", index+1, safeName(packageName))
		args = replaceCoverage(args, filepath.Join(cfg.ReportDir, coverage))
	}
	return args, coverage, nil
}

func replaceRun(command, roots []string) []string {
	patternParts := make([]string, 0, len(roots))
	for _, root := range roots {
		patternParts = append(patternParts, regexp.QuoteMeta(root))
	}
	pattern := "^(" + strings.Join(patternParts, "|") + ")$"
	return replaceFlag(command, "-run", "-run="+pattern)
}

func replaceShuffle(command []string, seed string) []string {
	return replaceFlag(command, "-shuffle", "-shuffle="+seed)
}

func replaceCoverage(command []string, path string) []string {
	return replaceFlag(command, "-coverprofile", "-coverprofile="+path)
}

func replaceFlag(command []string, name, replacement string) []string {
	result := make([]string, 0, len(command)+1)
	replaced := false
	for index := 0; index < len(command); index++ {
		arg := command[index]
		if arg == name {
			if index+1 < len(command) {
				index++
			}
			if !replaced {
				result = append(result, replacement)
				replaced = true
			}
			continue
		}
		if strings.HasPrefix(arg, name+"=") {
			if !replaced {
				result = append(result, replacement)
				replaced = true
			}
			continue
		}
		result = append(result, arg)
	}
	if !replaced {
		result = append(result, replacement)
	}
	return result
}

func replacePackages(command []string, packageName string) []string {
	result := make([]string, 0, len(command)+1)
	seenPackage := false
	valueFlags := map[string]bool{
		"-args": true, "-bench": true, "-benchtime": true, "-blockprofile": true,
		"-blockprofilerate": true, "-count": true, "-covermode": true, "-coverpkg": true,
		"-coverprofile": true, "-cpu": true, "-cpuprofile": true, "-memprofile": true,
		"-memprofilerate": true, "-mutexprofile": true, "-mutexprofilefraction": true,
		"-outputdir": true, "-parallel": true, "-run": true, "-shuffle": true,
		"-test.run": true, "-timeout": true, "-trace": true, "-vet": true,
	}
	for index := 0; index < len(command); index++ {
		arg := command[index]
		if arg == "-args" {
			result = append(result, command[index:]...)
			break
		}
		if index == 0 || arg == "test" || strings.HasPrefix(arg, "-") || arg == "-" {
			result = append(result, arg)
			if valueFlags[arg] && index+1 < len(command) {
				index++
				result = append(result, command[index])
			}
			continue
		}
		if !seenPackage {
			result = append(result, packageName)
			seenPackage = true
		}
	}
	if !seenPackage {
		result = append(result, packageName)
	}
	return result
}

func rootTestName(name string) string {
	if index := strings.IndexByte(name, '/'); index >= 0 {
		return name[:index]
	}
	return name
}

func safeName(packageName string) string {
	name := strings.NewReplacer("/", "_", "\\", "_", ".", "_").Replace(packageName)
	if name == "" {
		return "package"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	digest := sha256.Sum256([]byte(packageName))
	return fmt.Sprintf("%s-%x", name, digest[:6])
}

func failedNamesForRoot(names []string, root string) []string {
	var result []string
	for _, name := range names {
		if rootTestName(name) == root {
			result = append(result, name)
		}
	}
	return result
}

func hasAction(events []testEvent, action string) bool {
	for _, event := range events {
		if event.Action == action {
			return true
		}
	}
	return false
}
