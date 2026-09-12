package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// weightTableはデータだけを保持し、全shardが走査順に依存せず同じsnapshotを読む。
type weightTable struct {
	tests    map[string]map[string]float64
	packages map[string]float64
	flat     map[string]float64
}

type weightFile struct {
	Version  int                           `json:"version"`
	Tests    map[string]map[string]float64 `json:"tests,omitempty"`
	Packages map[string]float64            `json:"packages,omitempty"`
}

type weightRecord struct {
	Package string  `json:"package"`
	Test    string  `json:"test"`
	Name    string  `json:"name"`
	Elapsed float64 `json:"elapsed"`
	Weight  float64 `json:"weight"`
}

type reportEvent struct {
	Action  string  `json:"Action"`
	Package string  `json:"Package"`
	Test    string  `json:"Test"`
	Elapsed float64 `json:"Elapsed"`
}

func loadWeights(path string) (weightTable, error) {
	table := weightTable{
		tests:    make(map[string]map[string]float64),
		packages: make(map[string]float64),
		flat:     make(map[string]float64),
	}
	if strings.TrimSpace(path) == "" {
		return table, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return table, fmt.Errorf("read weights %q: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return table, errors.New("weights file is empty")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		if recordErr := decodeWeightRecords(data, &table); recordErr != nil {
			return table, fmt.Errorf("parse weights %q: %w", path, err)
		}
		return table, nil
	}
	if value, ok := raw["tests"]; ok {
		if err := decodeTestWeights(value, &table); err != nil {
			return table, fmt.Errorf("parse test weights: %w", err)
		}
	}
	if value, ok := raw["packages"]; ok {
		if err := decodePackageWeights(value, &table); err != nil {
			return table, fmt.Errorf("parse package weights: %w", err)
		}
	}
	// 手書きや旧生成物のflat形式も受け付ける。tests/packages節があればそちらを優先する。
	for key, value := range raw {
		if key == "version" || key == "tests" || key == "packages" {
			continue
		}
		var weight float64
		if err := json.Unmarshal(value, &weight); err == nil {
			if !validWeight(weight) {
				return table, fmt.Errorf("invalid weight for %q: %v", key, weight)
			}
			table.flat[key] = weight
		}
	}
	return table, nil
}

func decodeWeightRecords(data []byte, table *weightTable) error {
	var records []weightRecord
	if err := json.Unmarshal(data, &records); err == nil {
		for _, record := range records {
			if err := addWeightRecord(record, table); err != nil {
				return err
			}
		}
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineCount := 0
	for scanner.Scan() {
		lineCount++
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		var record weightRecord
		if err := json.Unmarshal([]byte(scanner.Text()), &record); err == nil &&
			(record.Package != "" || record.Test != "" || record.Name != "") {
			if err := addWeightRecord(record, table); err != nil {
				return fmt.Errorf("weight line %d: %w", lineCount, err)
			}
			continue
		}
		if len(fields) != 2 {
			return fmt.Errorf("weight line %d must contain a name and a number", lineCount)
		}
		weight, parseErr := strconv.ParseFloat(fields[1], 64)
		if parseErr != nil || !validWeight(weight) {
			return fmt.Errorf("invalid weight on line %d", lineCount)
		}
		table.flat[fields[0]] = weight
	}
	return scanner.Err()
}

func addWeightRecord(record weightRecord, table *weightTable) error {
	weight := record.Elapsed
	if record.Weight != 0 {
		weight = record.Weight
	}
	if !validWeight(weight) {
		return fmt.Errorf("invalid weight: %v", weight)
	}
	switch {
	case record.Package != "" && record.Test != "":
		if table.tests[record.Package] == nil {
			table.tests[record.Package] = make(map[string]float64)
		}
		table.tests[record.Package][record.Test] = weight
	case record.Package != "":
		table.packages[record.Package] = weight
	case record.Name != "":
		table.flat[record.Name] = weight
	default:
		return errors.New("weight record has no name")
	}
	return nil
}

func decodeTestWeights(data []byte, table *weightTable) error {
	var nested map[string]map[string]float64
	if err := json.Unmarshal(data, &nested); err == nil {
		for packageName, values := range nested {
			if table.tests[packageName] == nil {
				table.tests[packageName] = make(map[string]float64)
			}
			for name, weight := range values {
				if !validWeight(weight) {
					return fmt.Errorf("invalid weight for %q/%q: %v", packageName, name, weight)
				}
				table.tests[packageName][name] = weight
			}
		}
		return nil
	}
	var flat map[string]float64
	if err := json.Unmarshal(data, &flat); err == nil {
		for name, weight := range flat {
			if !validWeight(weight) {
				return fmt.Errorf("invalid weight for %q: %v", name, weight)
			}
			table.flat[name] = weight
		}
		return nil
	}
	var records []weightRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for _, record := range records {
		weight := record.Elapsed
		if record.Weight != 0 {
			weight = record.Weight
		}
		name := record.Name
		if name == "" {
			name = record.Test
		}
		if name == "" {
			return errors.New("test weight record has no name")
		}
		if !validWeight(weight) {
			return fmt.Errorf("invalid weight for %q: %v", name, weight)
		}
		if record.Package != "" {
			if table.tests[record.Package] == nil {
				table.tests[record.Package] = make(map[string]float64)
			}
			table.tests[record.Package][name] = weight
		} else {
			table.flat[name] = weight
		}
	}
	return nil
}

func decodePackageWeights(data []byte, table *weightTable) error {
	var values map[string]float64
	if err := json.Unmarshal(data, &values); err == nil {
		for packageName, weight := range values {
			if !validWeight(weight) {
				return fmt.Errorf("invalid weight for %q: %v", packageName, weight)
			}
			table.packages[packageName] = weight
		}
		return nil
	}
	var records []weightRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	for _, record := range records {
		packageName := record.Package
		if packageName == "" {
			packageName = record.Name
		}
		weight := record.Elapsed
		if record.Weight != 0 {
			weight = record.Weight
		}
		if packageName == "" {
			return errors.New("package weight record has no name")
		}
		if !validWeight(weight) {
			return fmt.Errorf("invalid weight for %q: %v", packageName, weight)
		}
		table.packages[packageName] = weight
	}
	return nil
}

func (table weightTable) testWeight(packageName, testName string) (float64, bool) {
	for _, candidate := range packageCandidates(packageName) {
		if values, ok := table.tests[candidate]; ok {
			if weight, ok := values[testName]; ok {
				return weight, true
			}
		}
	}
	// Makefileのgo test -listは相対pathを受け取り、citestはmodule import pathを記録する。
	// mapの走査順に依存せず、この表記差を解決する。
	keys := make([]string, 0, len(table.tests))
	for key := range table.tests {
		if packageMatches(packageName, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if weight, ok := table.tests[key][testName]; ok {
			return weight, true
		}
	}
	for _, candidate := range []string{
		packageName + "\x00" + testName,
		packageName + "::" + testName,
		packageName + ":" + testName,
	} {
		if weight, ok := table.flat[candidate]; ok {
			return weight, true
		}
	}
	if weight, ok := table.flat[testName]; ok {
		return weight, true
	}
	return 0, false
}

func (table weightTable) packageWeight(packageName string) (float64, bool) {
	for _, candidate := range packageCandidates(packageName) {
		if weight, ok := table.packages[candidate]; ok {
			return weight, true
		}
		if weight, ok := table.flat[candidate]; ok {
			return weight, true
		}
	}
	keys := make([]string, 0, len(table.packages))
	for key := range table.packages {
		if packageMatches(packageName, key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return table.packages[keys[0]], true
	}
	return 0, false
}

func packageMatches(left, right string) bool {
	left = strings.TrimPrefix(left, "./")
	right = strings.TrimPrefix(right, "./")
	return left == right || strings.HasSuffix(right, "/"+left) || strings.HasSuffix(left, "/"+right)
}

func packageCandidates(packageName string) []string {
	trimmed := strings.TrimPrefix(packageName, "./")
	if trimmed == packageName {
		return []string{packageName, "./" + packageName}
	}
	return []string{packageName, trimmed}
}

func (table weightTable) testMedian() float64 {
	values := make([]float64, 0, len(table.flat))
	for _, packageWeights := range table.tests {
		for _, weight := range packageWeights {
			if validWeight(weight) {
				values = append(values, weight)
			}
		}
	}
	for _, weight := range table.flat {
		if validWeight(weight) {
			values = append(values, weight)
		}
	}
	return medianWeight(values)
}

func (table weightTable) packageMedian() float64 {
	values := make([]float64, 0, len(table.packages)+len(table.flat))
	for _, weight := range table.packages {
		if validWeight(weight) {
			values = append(values, weight)
		}
	}
	for _, weight := range table.flat {
		if validWeight(weight) {
			values = append(values, weight)
		}
	}
	return medianWeight(values)
}

func medianWeight(values []float64) float64 {
	if len(values) == 0 {
		return 1
	}
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func writeWeights(reportsRoot, outputPath string) error {
	stats := collectedWeights{
		tests:    make(map[string]map[string]weightSamples),
		packages: make(map[string]weightSamples),
	}
	files := 0
	err := filepath.Walk(reportsRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() || info.Name() != "initial.jsonl" {
			return nil
		}
		// coverageはraceと固定費・実行対象が異なるため重みに混ぜない。
		if filepath.Base(filepath.Dir(path)) == "coverage" {
			return nil
		}
		files++
		return collectReport(path, &stats)
	})
	if err != nil {
		return fmt.Errorf("scan reports %q: %w", reportsRoot, err)
	}
	if files == 0 {
		return fmt.Errorf("no initial.jsonl reports found under %q", reportsRoot)
	}
	result := weightFile{Version: 1, Tests: make(map[string]map[string]float64), Packages: make(map[string]float64)}
	for packageName, values := range stats.tests {
		result.Tests[packageName] = make(map[string]float64)
		var testTotal float64
		for testName, samples := range values {
			weight := samples.average()
			result.Tests[packageName][testName] = weight
			testTotal += weight
		}
		if packageSamples := stats.packages[packageName]; packageSamples.count == 0 && testTotal > 0 {
			result.Packages[packageName] = testTotal
		}
	}
	for packageName, samples := range stats.packages {
		if samples.count > 0 {
			result.Packages[packageName] = samples.average()
		} else if _, ok := result.Packages[packageName]; !ok {
			result.Packages[packageName] = samples.average()
		}
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode weights: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("create weights directory: %w", err)
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		return fmt.Errorf("write weights %q: %w", outputPath, err)
	}
	return nil
}

type collectedWeights struct {
	tests    map[string]map[string]weightSamples
	packages map[string]weightSamples
}

type weightSamples struct {
	total float64
	count int
}

func (samples *weightSamples) add(value float64) {
	if validWeight(value) && value > 0 {
		samples.total += value
		samples.count++
	}
}

func (samples weightSamples) average() float64 {
	if samples.count == 0 {
		return 1
	}
	return samples.total / float64(samples.count)
}

func collectReport(path string, stats *collectedWeights) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	for scanner.Scan() {
		var event reportEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Package == "" || !validWeight(event.Elapsed) || event.Elapsed <= 0 {
			continue
		}
		switch {
		case event.Test != "" && isTopLevelTest(event.Test) && isTerminalAction(event.Action):
			if stats.tests[event.Package] == nil {
				stats.tests[event.Package] = make(map[string]weightSamples)
			}
			samples := stats.tests[event.Package][event.Test]
			samples.add(event.Elapsed)
			stats.tests[event.Package][event.Test] = samples
		case event.Test == "" && isTerminalAction(event.Action):
			samples := stats.packages[event.Package]
			samples.add(event.Elapsed)
			stats.packages[event.Package] = samples
		}
	}
	return scanner.Err()
}

func isTerminalAction(action string) bool {
	switch action {
	case "pass", "fail", "skip":
		return true
	default:
		return false
	}
}

func isTopLevelTest(name string) bool {
	return !strings.Contains(name, "/") && testName(name)
}
