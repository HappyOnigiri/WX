package config

import (
	"path/filepath"
	"testing"
)

func TestSetRepositoryOnboardingRoundTripsThroughRepositoryFor(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspace")
	raw := Config{Version: 2}
	if err := SetRepositoryOnboarding(&raw, root, "frontend", filepath.Join(root, "frontend"), "", "2026-09-17T00:01:00Z"); err != nil {
		t.Fatal(err)
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	record := effective.RepositoryFor(root, "frontend", filepath.Join(root, "frontend")).Onboarding
	if record.CheckedAt != "" || record.DeclinedAt != "2026-09-17T00:01:00Z" {
		t.Fatalf("onboarding=%+v", record)
	}
	if err := SetRepositoryOnboarding(&raw, root, "frontend", filepath.Join(root, "frontend"), "2026-09-17T00:02:00Z", ""); err != nil {
		t.Fatal(err)
	}
	workspace := raw.Workspaces[root]
	record = workspace.Repositories["frontend"].Onboarding
	if record.CheckedAt != "2026-09-17T00:02:00Z" || record.DeclinedAt != "" {
		t.Fatalf("checked onboarding=%+v", record)
	}
}
