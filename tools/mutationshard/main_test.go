package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildPlanUsesRunnableWeightsAndStableLPT(t *testing.T) {
	result := dryRunResult{Files: []dryRunFile{
		{Filename: "delta.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}, {Status: "RUNNABLE"}}},
		{Filename: "alpha.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}}},
		{Filename: "zero.go", Mutations: []dryRunMutation{{Status: "NOT COVERED"}}},
		{Filename: "beta.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}}},
		{Filename: "gamma.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}, {Status: "RUNNABLE"}, {Status: "RUNNABLE"}}},
	}}

	first, err := buildPlan(result, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildPlan(result, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := first.Files, []string{"alpha.go", "delta.go"}; !equalStrings(got, want) {
		t.Fatalf("first files=%v, want %v", got, want)
	}
	if got, want := second.Files, []string{"beta.go", "gamma.go"}; !equalStrings(got, want) {
		t.Fatalf("second files=%v, want %v", got, want)
	}
	if got, want := first.ExcludeFiles, []string{`(^|/)beta\.go$`, `(^|/)gamma\.go$`, `(^|/)zero\.go$`}; !equalStrings(got, want) {
		t.Fatalf("first exclusions=%v, want %v", got, want)
	}
	if got, want := second.ExcludeFiles, []string{`(^|/)alpha\.go$`, `(^|/)delta\.go$`, `(^|/)zero\.go$`}; !equalStrings(got, want) {
		t.Fatalf("second exclusions=%v, want %v", got, want)
	}
}

func TestBuildPlanRejectsEmptyBucketAndInvalidInput(t *testing.T) {
	result := dryRunResult{Files: []dryRunFile{{Filename: "only.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}}}}
	if _, err := buildPlan(result, 2, 1); err == nil || !strings.Contains(err.Error(), "has no files") {
		t.Fatalf("empty bucket error=%v", err)
	}
	for _, value := range []dryRunResult{
		{Files: []dryRunFile{{Filename: "../outside.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}}}},
		{Files: []dryRunFile{{Filename: "same.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}}, {Filename: "same.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}}}},
		{Files: []dryRunFile{{Filename: "covered.go", Mutations: []dryRunMutation{{Status: "NOT COVERED"}}}}},
	} {
		if _, err := buildPlan(value, 1, 0); err == nil {
			t.Fatalf("invalid result accepted: %#v", value)
		}
	}
}

func TestCommandMainReadsAndWritesJSONPlan(t *testing.T) {
	input := filepath.Join(t.TempDir(), "dry-run.json")
	data, err := json.Marshal(dryRunResult{Files: []dryRunFile{
		{Filename: "a.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}},
		{Filename: "b.go", Mutations: []dryRunMutation{{Status: "RUNNABLE"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output, errorOutput bytes.Buffer
	code := commandMain(context.Background(), []string{"-input", input, "-count", "2", "-index", "0"}, &output, &errorOutput)
	if code != 0 {
		t.Fatalf("code=%d stderr=%q", code, errorOutput.String())
	}
	var plan shardPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Files) != 1 || len(plan.ExcludeFiles) != 1 {
		t.Fatalf("plan=%#v", plan)
	}
}

func TestPackageRelativePathNormalizesSeparators(t *testing.T) {
	if got, err := packageRelativePath(`dir\\file.go`); err != nil || got != "dir/file.go" {
		t.Fatalf("normalized path=%q err=%v", got, err)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
