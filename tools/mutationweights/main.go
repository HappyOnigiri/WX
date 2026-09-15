// mutationweights はMutation Huntのmanifestからパッケージ所要時間の重みを生成する。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	weightFileVersion     = 1
	manifestSchemaVersion = 3
	defaultArtifactDir    = "artifacts/mutation"
	defaultWeightFile     = ".github/scripts/mutation-weights.json"
	manifestFileName      = "manifest.json"
)

type mutationManifest struct {
	SchemaVersion   int      `json:"schema_version"`
	Profile         string   `json:"profile"`
	RunID           string   `json:"run_id"`
	DurationSeconds *float64 `json:"duration_seconds"`
}

type weightFile struct {
	Version  int                `json:"version"`
	RunID    string             `json:"run_id"`
	Packages map[string]float64 `json:"packages"`
}

type manifestRecord struct {
	Path  string
	Value mutationManifest
}

type profileTotal struct {
	total    float64
	complete bool
}

func main() {
	if err := commandMain(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func commandMain(args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("mutationweights", flag.ContinueOnError)
	flags.SetOutput(errOut)
	artifactDir := flags.String("artifacts", defaultArtifactDir, "directory containing Mutation Hunt artifacts")
	flags.StringVar(artifactDir, "artifact-dir", defaultArtifactDir, "alias for -artifacts")
	flags.StringVar(artifactDir, "input", defaultArtifactDir, "alias for -artifacts")
	flags.StringVar(artifactDir, "reports", defaultArtifactDir, "alias for -artifacts")
	flags.StringVar(artifactDir, "reports-dir", defaultArtifactDir, "alias for -artifacts")
	output := flags.String("output", defaultWeightFile, "generated mutation weight JSON path")
	flags.StringVar(output, "weights", defaultWeightFile, "alias for -output")
	flags.StringVar(output, "weight-file", defaultWeightFile, "alias for -output")
	flags.StringVar(output, "write-weights", defaultWeightFile, "alias for -output")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("mutationweights: unexpected argument %q", flags.Arg(0))
	}
	if err := writeWeights(*artifactDir, *output); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "wrote mutation weights to %s\n", *output)
	return nil
}

func writeWeights(artifactDir, outputPath string) error {
	records, err := collectManifests(artifactDir)
	if err != nil {
		return err
	}
	value, err := aggregateWeights(records)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode mutation weights: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create mutation weight directory: %w", err)
	}
	if err := os.WriteFile(outputPath, encoded, 0o600); err != nil {
		return fmt.Errorf("write mutation weights: %w", err)
	}
	return nil
}

func collectManifests(root string) ([]manifestRecord, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("read mutation artifact directory: %w", err)
	}
	var paths []string
	switch {
	case info.Mode().IsRegular():
		if filepath.Base(root) != manifestFileName {
			return nil, fmt.Errorf("mutation artifact input %q is not %s", root, manifestFileName)
		}
		paths = append(paths, root)
	case info.IsDir():
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if entry.Name() == manifestFileName {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan mutation artifacts: %w", err)
		}
	default:
		return nil, fmt.Errorf("mutation artifact input %q is not a file or directory", root)
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, errors.New("no mutation manifests found")
	}
	records := make([]manifestRecord, 0, len(paths))
	for _, path := range paths {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read mutation manifest %s: %w", path, readErr)
		}
		var value mutationManifest
		if decodeErr := json.Unmarshal(data, &value); decodeErr != nil {
			return nil, fmt.Errorf("decode mutation manifest %s: %w", path, decodeErr)
		}
		if err := validateManifest(value, path); err != nil {
			return nil, err
		}
		records = append(records, manifestRecord{Path: path, Value: value})
	}
	return records, nil
}

func validateManifest(value mutationManifest, name string) error {
	if value.SchemaVersion != manifestSchemaVersion {
		return fmt.Errorf("%s has unsupported schema_version %d (want %d)", name, value.SchemaVersion, manifestSchemaVersion)
	}
	profile := normalizeProfile(value.Profile)
	if profile == "" {
		return fmt.Errorf("%s has an invalid profile", name)
	}
	if strings.TrimSpace(value.RunID) == "" {
		return fmt.Errorf("%s has no run_id", name)
	}
	if value.DurationSeconds != nil &&
		(math.IsNaN(*value.DurationSeconds) || math.IsInf(*value.DurationSeconds, 0) || *value.DurationSeconds < 0) {
		return fmt.Errorf("%s has an invalid duration_seconds", name)
	}
	return nil
}

func aggregateWeights(records []manifestRecord) (weightFile, error) {
	if len(records) == 0 {
		return weightFile{}, errors.New("no mutation manifests found")
	}
	runID := strings.TrimSpace(records[0].Value.RunID)
	profiles := make(map[string]profileTotal)
	for _, record := range records {
		value := record.Value
		if strings.TrimSpace(value.RunID) != runID {
			return weightFile{}, fmt.Errorf("%s run_id %q does not match %q", record.Path, value.RunID, runID)
		}
		profile := normalizeProfile(value.Profile)
		entry, exists := profiles[profile]
		if !exists {
			entry.complete = true
		}
		switch {
		case value.DurationSeconds == nil:
			entry.complete = false
		case *value.DurationSeconds <= 0:
			entry.complete = false
		default:
			entry.total += *value.DurationSeconds
		}
		profiles[profile] = entry
	}
	packages := make(map[string]float64)
	for profile, entry := range profiles {
		if entry.complete && entry.total > 0 && !math.IsNaN(entry.total) && !math.IsInf(entry.total, 0) {
			packages[profile] = entry.total
		}
	}
	return weightFile{Version: weightFileVersion, RunID: runID, Packages: packages}, nil
}

func normalizeProfile(value string) string {
	profile := strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	profile = strings.TrimPrefix(profile, "./")
	if profile == "" || profile == "." || strings.HasPrefix(profile, "/") || strings.Contains(profile, "//") {
		return ""
	}
	parts := strings.Split(profile, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\r\n\t") {
			return ""
		}
		if first := part[0]; !((first >= 'A' && first <= 'Z') || (first >= 'a' && first <= 'z') || (first >= '0' && first <= '9')) {
			return ""
		}
		for _, character := range part {
			if !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') &&
				!(character >= '0' && character <= '9') && character != '_' && character != '-' && character != '.' {
				return ""
			}
		}
	}
	return profile
}
