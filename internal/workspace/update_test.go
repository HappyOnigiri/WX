package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestRejectChangedAttributesDetectsRootAndNestedChanges(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		change     func(t *testing.T, repository string)
		ineligible bool
	}{
		{name: "root added", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, ".gitattributes"), "*.txt text eol=crlf\n")
		}, ineligible: true},
		{name: "nested changed", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, "sub", ".gitattributes"), "*.txt -text\n")
		}, ineligible: true},
		{name: "nested removed", change: func(t *testing.T, repository string) {
			if err := os.Remove(filepath.Join(repository, "sub", ".gitattributes")); err != nil {
				t.Fatal(err)
			}
		}, ineligible: true},
		{name: "unrelated file only", change: func(t *testing.T, repository string) {
			writeTestFile(t, filepath.Join(repository, "sub", "b.txt"), "changed\n")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := t.TempDir()
			gitCommand(t, repository, "init", "-b", "main")
			writeTestFile(t, filepath.Join(repository, "a.txt"), "a\n")
			writeTestFile(t, filepath.Join(repository, "sub", "b.txt"), "b\n")
			writeTestFile(t, filepath.Join(repository, "sub", ".gitattributes"), "*.txt text\n")
			gitCommand(t, repository, "add", "-A")
			gitCommand(t, repository, "commit", "-m", "base")
			oldOID := gitOutput(t, repository, "rev-parse", "HEAD")
			testCase.change(t, repository)
			gitCommand(t, repository, "add", "-A")
			gitCommand(t, repository, "commit", "-m", "change")
			newOID := gitOutput(t, repository, "rev-parse", "HEAD")
			preparer := Preparer{Git: &gitx.Runner{Timeout: 30 * time.Second}}
			repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
			err := preparer.rejectChangedAttributes(context.Background(), repo, oldOID, newOID)
			if testCase.ineligible != errors.Is(err, ErrUpdateIneligible) {
				t.Fatalf("ineligible=%v err=%v", testCase.ineligible, err)
			}
			if !testCase.ineligible && err != nil {
				t.Fatalf("unexpected failure: %v", err)
			}
		})
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPathsConflictAnyIncludesAncestors(t *testing.T) {
	if !pathsConflictAny("cache", map[string]bool{"cache/file": true}) {
		t.Fatal("ancestor collision was missed")
	}
	if pathsConflictAny("cache-a", map[string]bool{"cache-b": true}) {
		t.Fatal("unrelated paths collided")
	}
}

func TestValidateAndSyncRootPlacementsUpdatesRecordedCopyAndPreservesGeneratedFile(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	sourcePath := filepath.Join(source, "config", "local.cfg")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "local.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(target, "config", "generated.log")
	if err := os.WriteFile(generated, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	previous := []state.Placement{{RelativePath: "config/local.cfg", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("old\n")}}
	desired := []state.Placement{{RelativePath: "config/local.cfg", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("new\n")}}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateAndSyncRootPlacements(root, previous, desired); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "config", "local.cfg")); err != nil || string(got) != "new\n" {
		t.Fatalf("updated copy=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(generated); err != nil || string(got) != "keep\n" {
		t.Fatalf("generated file=%q err=%v", got, err)
	}
}

func TestValidateAndSyncRootPlacementsReplacesRecordedDirectoryWithFile(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	sourcePath := filepath.Join(source, "config")
	if err := os.WriteFile(sourcePath, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config", "old.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:])
	}
	previous := []state.Placement{{RelativePath: "config/old.cfg", Kind: "copy", SourcePath: filepath.Join(source, "old.cfg"), ContentSHA256: hash("old\n")}}
	desired := []state.Placement{{RelativePath: "config", Kind: "copy", SourcePath: sourcePath, ContentSHA256: hash("new\n")}}
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := ValidateAndSyncRootPlacements(root, previous, desired); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "config")); err != nil || string(got) != "new\n" {
		t.Fatalf("replacement=%q err=%v", got, err)
	}
}
