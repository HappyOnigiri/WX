package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStagedFilesUsesExplicitIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runTestGit(t, root, nil, "init")
	runTestGit(t, root, nil, "config", "user.email", "hookcheck@example.test")
	runTestGit(t, root, nil, "config", "user.name", "hookcheck")
	writeTestFile(t, root, "tracked.txt", "tracked\n")
	runTestGit(t, root, nil, "add", "tracked.txt")
	runTestGit(t, root, nil, "commit", "-m", "initial")
	indexBytes, err := os.ReadFile(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	alternate := filepath.Join(root, "alternate-index")
	if err := os.WriteFile(alternate, indexBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, root, "staged only.txt", "staged\n")
	runTestGit(t, root, []string{"GIT_INDEX_FILE=" + alternate}, "add", "staged only.txt")
	gitDir := strings.TrimSpace(runTestGit(t, root, nil, "rev-parse", "--absolute-git-dir"))
	files, err := stagedFiles(context.Background(), root, gitDir, alternate)
	if err != nil {
		t.Fatalf("stagedFiles: %v", err)
	}
	if len(files) != 1 || files[0].status != "A" || files[0].path != "staged only.txt" {
		t.Fatalf("files=%#v", files)
	}
}

func runTestGit(t *testing.T, directory string, extraEnvironment []string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	command.Env = append(os.Environ(), extraEnvironment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}
