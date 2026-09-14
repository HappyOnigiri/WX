package workspace

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 適格外の子を含む一括準備でも、post-checkout hook がその子を再初期化しない。
func TestPrepareBulkSubmodulesKeepsIneligibleChildEmpty(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	secondChild := filepath.Join(filepath.Dir(f.child), "second-child")
	if err := os.Mkdir(secondChild, 0o700); err != nil {
		t.Fatal(err)
	}
	initTestRepository(t, secondChild)
	if err := os.WriteFile(filepath.Join(secondChild, "second.txt"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, secondChild, "add", ".")
	gitCommand(t, secondChild, "commit", "-m", "second initial")
	gitCommand(t, f.repository, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "modules/second", "../second-child", "sub/second")
	gitCommand(t, f.repository, "add", ".")
	gitCommand(t, f.repository, "commit", "-m", "add second submodule")
	f.head = gitOutput(t, f.repository, "rev-parse", "HEAD")
	gitCommand(t, f.repository, "config", "--remove-section", "submodule."+submoduleName)
	gitCommand(t, f.repository, "config", "--remove-section", "submodule.modules/second")
	if err := os.RemoveAll(filepath.Join(string(f.repo.CommonDir), "modules", "modules", "second")); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(string(f.repo.CommonDir), "config")
	beforeConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	f.preparer.submoduleWorkerCount = 1
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.target, submodulePath, "kid.txt")); err != nil {
		t.Fatalf("eligible submodule content: %v", err)
	}
	assertEmptyGitlinkDirectory(t, filepath.Join(f.target, "sub", "second"))
	afterConfig, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeConfig, afterConfig) {
		t.Fatalf("source repository config changed:\nbefore:\n%s\nafter:\n%s", beforeConfig, afterConfig)
	}
}
