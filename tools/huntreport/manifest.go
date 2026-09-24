package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
	"github.com/HappyOnigiri/WorktreeX/tools/internal/gotest"
)

// countPattern と runPattern は、issue本文へ残すハント条件をコマンドから拾う。
var (
	countPattern = regexp.MustCompile(`^-?-count=(.+)$`)
	runPattern   = regexp.MustCompile(`^-?-run=(.+)$`)
)

// manifest は集計をreporting側が読む形へ落とす。
// 宣言を解決できなかったテストは診断へ落とし、他のテストの報告を止めない。
func (s *huntState) manifest(cfg config) huntManifest {
	command, err := commandWithJSON(cfg.Command)
	if err != nil {
		command = append([]string(nil), cfg.Command...)
	}
	man := huntManifest{
		SchemaVersion: huntSchemaVersion,
		Kind:          huntKind,
		HuntID:        cfg.HuntID,
		Command:       command,
		RunRegexp:     matchArgument(command, runPattern),
		Count:         matchArgument(command, countPattern),
		Rounds:        len(s.rounds),
		FailedRounds:  s.failedRounds,
		AnomalyRounds: s.anomalyRounds,
		Repository:    os.Getenv("GITHUB_REPOSITORY"),
		RunID:         os.Getenv("GITHUB_RUN_ID"),
		RunAttempt:    os.Getenv("GITHUB_RUN_ATTEMPT"),
		Event:         os.Getenv("GITHUB_EVENT_NAME"),
		Ref:           os.Getenv("GITHUB_REF"),
		APISHA:        os.Getenv("GITHUB_SHA"),
		TestSHA:       gitSHA(cfg.RepoRoot),
		GoVersion:     goVersion(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		RoundRecords:  s.rounds,
		CreatedAt:     s.startedAt,
		FinishedAt:    s.finishedAt,
	}
	man.Tests, man.Diagnostics = s.testTallies()
	man.Diagnostics = append(man.Diagnostics, s.diagnostics...)
	sort.Strings(man.Diagnostics)
	return man
}

// testTallies は失敗を1回以上観測したテストだけを並べる。
// 全ラウンド成功したテストは証拠にならないので載せない。
func (s *huntState) testTallies() ([]testTally, []string) {
	var tallies []testTally
	var diagnostics []string
	ids := make([]gotest.TestID, 0, len(s.failingNames))
	for id := range s.failingNames {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if ids[a].Package != ids[b].Package {
			return ids[a].Package < ids[b].Package
		}
		return ids[a].Test < ids[b].Test
	})
	for _, id := range ids {
		decl, ok := s.declarations[id.Package][id.Test]
		if !ok {
			diagnostics = append(diagnostics, fmt.Sprintf("%s: no declaration for %s", id.Package, id.Test))
			continue
		}
		names := append([]string(nil), s.failingNames[id]...)
		sort.Strings(names)
		tally := s.tallies[id]
		tallies = append(tallies, testTally{
			Package:     id.Package,
			Declaration: decl,
			Subtests:    names,
			PassCount:   tally.Pass,
			FailCount:   tally.Fail,
			SkipCount:   tally.Skip,
			LogExcerpt:  s.excerpts[id],
		})
	}
	return tallies, diagnostics
}

func matchArgument(command []string, pattern *regexp.Regexp) string {
	for _, arg := range command {
		if match := pattern.FindStringSubmatch(arg); len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

func writeManifest(reportDir string, value huntManifest) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(reportDir, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// writeSummary は従来のstep summaryと同じ見え方を、集計から組み立てる。
func writeSummary(man huntManifest) error {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if path == "" {
		return nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.WriteString(summaryText(man))
	return err
}

func summaryText(man huntManifest) string {
	lines := []string{
		fmt.Sprintf("## Flake hunt: `%s`", man.HuntID),
		"",
		fmt.Sprintf("- Profile: `%s`", strings.Join(man.Command, " ")),
		fmt.Sprintf("- Rounds: %d", man.Rounds),
		fmt.Sprintf("- Failed rounds: %d", man.FailedRounds),
		fmt.Sprintf("- Anomalous rounds: %d", man.AnomalyRounds),
		"",
	}
	if len(man.Tests) > 0 {
		lines = append(lines, "### Failing tests", "", "```")
		for _, tally := range man.Tests {
			mark := ""
			// 全ラウンド失敗するテストは決定的な失敗で、issueへは起票しない。
			if tally.PassCount == 0 {
				mark = " (never passed; not reported as flaky)"
			}
			lines = append(lines, fmt.Sprintf("%6d fail %6d pass  %s: %s%s",
				tally.FailCount, tally.PassCount, tally.Declaration.Path, tally.Declaration.Function, mark))
			for _, name := range tally.Subtests {
				if name != tally.Declaration.Function {
					lines = append(lines, "             subtest  "+name)
				}
			}
		}
		lines = append(lines, "```", "")
	}
	if len(man.Diagnostics) > 0 {
		lines = append(lines, "### Diagnostics", "", "```")
		lines = append(lines, man.Diagnostics...)
		lines = append(lines, "```", "")
	}
	if man.FailedRounds > 0 {
		lines = append(lines, fmt.Sprintf("Full logs of the failed rounds are attached as `hunt-logs-%s`.", man.HuntID), "")
	}
	return strings.Join(lines, "\n")
}

func gitSHA(root string) string {
	result, err := (&gitx.Runner{}).Run(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func goVersion() string {
	output, err := exec.CommandContext(context.Background(), "go", "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}
