package main

import (
	"context"
	"strings"
	"testing"
)

func TestCommandMainRejectsIncompleteInvocations(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no id", []string{"-minutes", "1", "--", "go", "test", "./..."}},
		{"no minutes", []string{"-id", "hunt-1", "go", "test", "./..."}},
		{"no command", []string{"-id", "hunt-1", "-minutes", "1"}},
		{"unknown flag", []string{"-nope"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			var output, errorOutput strings.Builder
			if code := commandMain(context.Background(), item.args, &output, &errorOutput); code != 2 {
				t.Fatalf("code=%d stderr=%q", code, errorOutput.String())
			}
		})
	}
}

func TestValidateConfigRequiresGoTest(t *testing.T) {
	base := config{ReportDir: "report", LogDir: "logs", RepoRoot: "."}
	base.Command = []string{"go", "test", "./..."}
	if err := validateConfig(base); err != nil {
		t.Fatal(err)
	}
	base.Command = []string{"go", "build", "./..."}
	if err := validateConfig(base); err == nil {
		t.Fatal("a command without go test was accepted")
	}
	base.Command = []string{"go", "test"}
	base.LogDir = ""
	if err := validateConfig(base); err == nil {
		t.Fatal("an empty log directory was accepted")
	}
}

func TestCommandWithJSONInsertsTheFlagOnce(t *testing.T) {
	got, err := commandWithJSON([]string{"go", "test", "-race", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "go test -json -race ./..." {
		t.Fatalf("command=%v", got)
	}
	got, err = commandWithJSON([]string{"go", "test", "-json", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "go test -json ./..." {
		t.Fatalf("command=%v", got)
	}
	if _, err := commandWithJSON([]string{"go", "vet", "./..."}); err == nil {
		t.Fatal("a command without go test was accepted")
	}
}
