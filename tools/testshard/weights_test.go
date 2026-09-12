package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadWeightsReadsNestedAndRelativePackageEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "weights.json")
	data := []byte(`{
  "version": 1,
  "tests": {"github.com/example/pkg": {"TestSlow": 4.5}},
  "packages": {"github.com/example/pkg": 7.25}
}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := loadWeights(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := table.testWeight("./github.com/example/pkg", "TestSlow"); !ok || got != 4.5 {
		t.Fatalf("test weight=%v, present=%v", got, ok)
	}
	if got, ok := table.packageWeight("./github.com/example/pkg"); !ok || got != 7.25 {
		t.Fatalf("package weight=%v, present=%v", got, ok)
	}
}

func TestWriteWeightsCollectsTerminalTopLevelEvents(t *testing.T) {
	reports := filepath.Join(t.TempDir(), "reports")
	if err := os.MkdirAll(filepath.Join(reports, "race-daemon-0"), 0o750); err != nil {
		t.Fatal(err)
	}
	jsonl := `{"Action":"pass","Package":"github.com/example/pkg","Test":"TestSlow","Elapsed":4.5}
{"Action":"pass","Package":"github.com/example/pkg","Test":"TestSlow/sub","Elapsed":4.5}
{"Action":"pass","Package":"github.com/example/pkg","Test":"TestFast","Elapsed":1.5}
{"Action":"pass","Package":"github.com/example/pkg","Elapsed":6.25}
{"Action":"run","Package":"github.com/example/pkg","Test":"TestIgnored","Elapsed":99}
`
	if err := os.WriteFile(filepath.Join(reports, "race-daemon-0", "initial.jsonl"), []byte(jsonl), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "weights.json")
	if err := writeWeights(reports, output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var value weightFile
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	wantTests := map[string]map[string]float64{
		"github.com/example/pkg": {"TestSlow": 4.5, "TestFast": 1.5},
	}
	if !reflect.DeepEqual(value.Tests, wantTests) {
		t.Fatalf("tests=%v, want %v", value.Tests, wantTests)
	}
	if value.Packages["github.com/example/pkg"] != 6.25 {
		t.Fatalf("package weight=%v, want 6.25", value.Packages["github.com/example/pkg"])
	}
}

func TestWriteWeightsRejectsMissingReports(t *testing.T) {
	err := writeWeights(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "weights.json"))
	if err == nil {
		t.Fatal("writeWeights succeeded without reports")
	}
}
