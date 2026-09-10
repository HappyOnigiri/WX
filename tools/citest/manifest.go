package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

func writeManifest(reportDir string, value manifest) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFile(filepath.Join(reportDir, "manifest.json"), data)
}

func runRecordFromResult(result testResult, cfg config, command []string, label, coverage string) runRecord {
	coverageName := ""
	if coverage != "" {
		coverageName = filepath.Base(coverage)
	}
	return runRecord{
		Command:          command,
		Exit:             result.Exit,
		Status:           result.Status,
		Signal:           result.Signal,
		JSONL:            filepath.Base(filepath.Join(cfg.ReportDir, label+".jsonl")),
		Log:              filepath.Base(filepath.Join(cfg.ReportDir, label+".log")),
		Stderr:           filepath.Base(filepath.Join(cfg.ReportDir, label+".stderr")),
		Coverage:         coverageName,
		Shuffle:          result.Shuffle,
		ShuffleByPackage: result.ShuffleByPackage,
		StartedAt:        result.StartedAt,
		FinishedAt:       result.FinishedAt,
		DurationMS:       durationMS(result.StartedAt, result.FinishedAt),
		LogExcerpt:       result.LogExcerpt,
	}
}

func gitSHA(root string) string {
	result, err := (&gitx.Runner{}).Run(context.Background(), root, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(result.Stdout)
}

func testSHA(root string) string {
	return gitSHA(root)
}
