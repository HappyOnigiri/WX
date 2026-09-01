package coverageconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndMatch(t *testing.T) {
	fileName := filepath.Join(t.TempDir(), "exclusions.txt")
	content := "# comment\n\ncmd/wx/main.go\tprocess adapter\ninternal/launchd/launchd.go\tDarwin adapter\n"
	if err := os.WriteFile(fileName, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	exclusions, err := Load(fileName)
	if err != nil || len(exclusions) != 2 {
		t.Fatalf("exclusions=%+v err=%v", exclusions, err)
	}
	if !Matches("github.com/example/project/cmd/wx/main.go", exclusions) || !Matches("internal/launchd/launchd.go", exclusions) {
		t.Fatal("configured files did not match")
	}
	if Matches("internal/daemon/manager.go", exclusions) {
		t.Fatal("unconfigured file matched")
	}
}

func TestLoadRejectsMissingAndUnexplainedEntries(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing exclusion file succeeded")
	}
	fileName := filepath.Join(t.TempDir(), "invalid.txt")
	for _, content := range []string{"path-without-reason\n", "\treason\n", "path\t\n"} {
		if err := os.WriteFile(fileName, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(fileName); err == nil {
			t.Fatalf("invalid exclusion succeeded: %q", content)
		}
	}
}
