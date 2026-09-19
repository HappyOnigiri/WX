package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestSetRepositoryOnboardingAcceptsWorkspaceRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspace")
	raw := Config{Version: 2}
	if err := SetRepositoryOnboarding(&raw, root, ".", root, "2026-09-17T00:02:00Z", ""); err != nil {
		t.Fatal(err)
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	record := effective.RepositoryFor(root, ".", root).Onboarding
	if record.CheckedAt != "2026-09-17T00:02:00Z" || record.DeclinedAt != "" {
		t.Fatalf("root onboarding=%+v", record)
	}
	if raw.Workspaces[root].Onboarding != record {
		t.Fatalf("raw root onboarding=%+v", raw.Workspaces[root].Onboarding)
	}
}

func TestLoadRawMigratesLegacyWorkspaceOnboardingAndSavesCurrentShape(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `version: 2
workspaces:
  /workspace:
    worktree: hot
    onboarding:
      ".":
        checked_at: "2026-09-17T00:02:00Z"
      frontend:
        declined_at: "2026-09-17T00:03:00Z"
    repositories:
      frontend:
        readiness:
          mode: full
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	workspace := raw.Workspaces["/workspace"]
	if workspace.Onboarding.CheckedAt != "2026-09-17T00:02:00Z" {
		t.Fatalf("root onboarding=%+v", workspace.Onboarding)
	}
	if got := workspace.Repositories["frontend"].Onboarding.DeclinedAt; got != "2026-09-17T00:03:00Z" {
		t.Fatalf("frontend declined_at=%q", got)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(saved)
	if strings.Contains(text, `".":`) || strings.Contains(text, "\n            .:") {
		t.Fatalf("saved config did not use the current onboarding shape:\n%s", text)
	}
	reloaded, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	workspace = reloaded.Workspaces["/workspace"]
	if workspace.Onboarding.CheckedAt == "" || workspace.Repositories["frontend"].Onboarding.DeclinedAt == "" {
		t.Fatalf("reloaded workspace=%+v", workspace)
	}
}

func TestLoadRawRejectsLegacyAndCurrentMemberOnboardingCollision(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := `version: 2
workspaces:
  /workspace:
    onboarding:
      frontend:
        checked_at: old
    repositories:
      frontend:
        onboarding:
          checked_at: current
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRaw(); err == nil || !strings.Contains(err.Error(), "both legacy and current locations") {
		t.Fatalf("LoadRaw error=%v", err)
	}
}

func TestLoadRawKeepsUnknownRootOnboardingFieldForDoctor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := "version: 2\nworkspaces:\n  /workspace:\n    onboarding:\n      checkd_at: now\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadRaw()
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Workspaces["/workspace"].Repositories) != 0 {
		t.Fatalf("unknown field was migrated as a repository: %+v", raw.Workspaces["/workspace"])
	}
	unknown := raw.UnknownKeys()
	if len(unknown) != 1 || !strings.HasSuffix(unknown[0].Key, ".onboarding.checkd_at") {
		t.Fatalf("unknown keys=%+v", unknown)
	}
}

func TestSetRepositoryOnboardingRejectsInvalidRelativePath(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "workspace")
	for _, relativePath := range []string{"", "../outside", filepath.Join(root, "absolute")} {
		raw := Config{Version: 2}
		if err := SetRepositoryOnboarding(&raw, root, relativePath, root, "now", ""); err == nil || !strings.Contains(err.Error(), "relative") {
			t.Fatalf("relativePath=%q error=%v", relativePath, err)
		}
	}
}
