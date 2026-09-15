package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, root, name string, value mutationManifest) {
	t.Helper()
	path := filepath.Join(root, name, manifestFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func duration(value float64) *float64 { return &value }

func TestAggregateWeightsSumsProfilesAndDropsIncompleteMeasurements(t *testing.T) {
	records := []manifestRecord{
		{Path: "one/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "./internal/config", RunID: "run-1", DurationSeconds: duration(2.5)}},
		{Path: "two/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "internal/config", RunID: "run-1", DurationSeconds: duration(1.5)}},
		{Path: "three/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "internal/state", RunID: "run-1"}},
		{Path: "four/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "internal/zero", RunID: "run-1", DurationSeconds: duration(0)}},
	}
	value, err := aggregateWeights(records)
	if err != nil {
		t.Fatal(err)
	}
	if value.Version != 1 || value.RunID != "run-1" {
		t.Fatalf("header=%+v", value)
	}
	if !reflect.DeepEqual(value.Packages, map[string]float64{"internal/config": 4}) {
		t.Fatalf("packages=%v", value.Packages)
	}
}

func TestCollectManifestsReadsNestedArtifactsAndWritesStableJSON(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "mutation-group-1", mutationManifest{
		SchemaVersion: 3, Profile: "internal/z", RunID: "42", DurationSeconds: duration(3),
	})
	writeManifest(t, root, "mutation-group-2", mutationManifest{
		SchemaVersion: 3, Profile: "internal/a", RunID: "42", DurationSeconds: duration(1),
	})
	records, err := collectManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || !strings.HasSuffix(records[0].Path, "mutation-group-1/manifest.json") {
		t.Fatalf("records=%+v", records)
	}
	out := filepath.Join(t.TempDir(), "nested", "weights.json")
	if err := writeWeights(root, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("output lacks trailing newline: %q", data)
	}
	var value weightFile
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value.Packages, map[string]float64{"internal/a": 1, "internal/z": 3}) {
		t.Fatalf("packages=%v", value.Packages)
	}
}

func TestCollectManifestsRejectsEmptyAndInvalidInputs(t *testing.T) {
	empty := t.TempDir()
	if _, err := collectManifests(empty); err == nil || !strings.Contains(err.Error(), "no mutation manifests") {
		t.Fatalf("empty error=%v", err)
	}
	file := filepath.Join(t.TempDir(), "not-manifest.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectManifests(file); err == nil || !strings.Contains(err.Error(), "not manifest.json") {
		t.Fatalf("file error=%v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := collectManifests(missing); err == nil {
		t.Fatal("missing artifact input accepted")
	}
}

func TestCollectManifestsValidatesSchemaRunProfileAndDuration(t *testing.T) {
	cases := []struct {
		name  string
		value mutationManifest
		want  string
	}{
		{"schema", mutationManifest{SchemaVersion: 2, Profile: "internal/config", RunID: "1"}, "schema_version"},
		{"run", mutationManifest{SchemaVersion: 3, Profile: "internal/config"}, "run_id"},
		{"profile", mutationManifest{SchemaVersion: 3, Profile: "../outside", RunID: "1"}, "profile"},
		{"duration", mutationManifest{SchemaVersion: 3, Profile: "internal/config", RunID: "1", DurationSeconds: duration(-1)}, "duration_seconds"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			writeManifest(t, root, "artifact", testCase.value)
			_, err := collectManifests(root)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error=%v, want %s", err, testCase.want)
			}
		})
	}
}

func TestAggregateWeightsRejectsMixedRunIDsAndNoRecords(t *testing.T) {
	if _, err := aggregateWeights(nil); err == nil {
		t.Fatal("empty records accepted")
	}
	first := manifestRecord{Path: "one/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "internal/config", RunID: "1", DurationSeconds: duration(1)}}
	second := manifestRecord{Path: "two/manifest.json", Value: mutationManifest{SchemaVersion: 3, Profile: "internal/state", RunID: "2", DurationSeconds: duration(1)}}
	if _, err := aggregateWeights([]manifestRecord{first, second}); err == nil || !strings.Contains(err.Error(), "run_id") {
		t.Fatalf("mixed run IDs error=%v", err)
	}
}

func TestCommandMainWritesWeights(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "artifact", mutationManifest{
		SchemaVersion: 3, Profile: "internal/config", RunID: "9", DurationSeconds: duration(2),
	})
	out := filepath.Join(t.TempDir(), "weights.json")
	var stdout strings.Builder
	if err := commandMain([]string{"-artifacts", root, "-output", out}, &stdout, &stdout); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), out) {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatal(err)
	}
	if err := commandMain([]string{"unexpected"}, &stdout, &stdout); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
}
