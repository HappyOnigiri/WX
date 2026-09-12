package main

import (
	"strings"
	"testing"
)

func TestValidateConfigAcceptsAllProfiles(t *testing.T) {
	base := config{
		ReportDir: "reports",
		RepoRoot:  "repo",
		Command:   []string{"go", "test", "./..."},
	}
	for _, profile := range []string{
		"coverage", "race-daemon", "race-daemon-0", "race-daemon-1",
		"race-state", "race-state-0", "race-state-1", "race-rest",
	} {
		t.Run(profile, func(t *testing.T) {
			cfg := base
			cfg.Profile = profile
			if err := validateConfig(cfg); err != nil {
				t.Fatalf("validateConfig(%q) = %v", profile, err)
			}
		})
	}
}

func TestValidateConfigRejectsUnknownProfile(t *testing.T) {
	err := validateConfig(config{
		Profile:   "race-unknown",
		ReportDir: "reports",
		RepoRoot:  "repo",
		Command:   []string{"go", "test", "./..."},
	})
	if err == nil || !strings.Contains(err.Error(), `unsupported profile "race-unknown"`) {
		t.Fatalf("validateConfig error = %v", err)
	}
}
