package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestMaterializeRootCopiesLinksAndIsIdempotent(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "docs", "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "docs", "nested", "note"), []byte("note\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	rules := RootRulesFromConfig(config.Workspace{Copy: []string{"docs", "docs"}, Link: []string{"shared"}})
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatalf("idempotent materialization: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "docs", "nested", "note"))
	if err != nil || string(data) != "note\n" {
		t.Fatalf("copied data=%q err=%v", data, err)
	}
	link, err := os.Readlink(filepath.Join(target, "shared"))
	if err != nil || link != filepath.Join(source, "shared") {
		t.Fatalf("link=%q err=%v", link, err)
	}
}

func TestWorkspaceRootDefaultSymlinkRuleIsSkipped(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repository := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	before, err := Fingerprint(1, "oid", repository, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(source, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	after, err := Fingerprint(1, "oid", repository, config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("default symlink changed fingerprint before=%s after=%s", before, after)
	}
	if err := MaterializeRoot(nil, source, target, RootRulesFromConfig(config.Workspace{})); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(target, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default symlink materialized: %v", err)
	}
	// 明示指定した名前も symlink なら既定名と同じく skip し、prepare を失敗させない。
	explicitTarget := t.TempDir()
	if err := MaterializeRoot(nil, source, explicitTarget, RootRulesFromConfig(config.Workspace{Copy: []string{"AGENTS.md"}})); err != nil {
		t.Fatalf("explicit symlink copy error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(explicitTarget, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit symlink materialized: %v", err)
	}
}

func TestMaterializeRootRejectsMissingExplicitCopyBeforeWriting(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "AGENTS.md"), []byte("rules\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rules := RootRulesFromConfig(config.Workspace{Copy: []string{"required.json"}})
	err := MaterializeRoot(nil, source, target, rules)
	if err == nil {
		t.Fatal("missing explicit workspace copy succeeded")
	}
	missing := filepath.Join(source, "required.json")
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), source) {
		t.Fatalf("missing explicit copy error=%v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(target, "AGENTS.md")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("materialization wrote a default copy before validation: %v", statErr)
	}
}

func TestMaterializeRootAtUsesPinnedDestination(t *testing.T) {
	t.Parallel()
	source, target := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "copied.txt"), []byte("pinned copy\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := MaterializeRootAt(nil, source, owner, RootRulesFromConfig(config.Workspace{Copy: []string{"copied.txt"}, Link: []string{"shared"}})); err != nil {
		t.Fatalf("pinned materialization: %v", err)
	}
	data, err := owner.ReadFile("copied.txt")
	if err != nil || string(data) != "pinned copy\n" {
		t.Fatalf("pinned copy=%q err=%v", data, err)
	}
	link, err := owner.Readlink("shared")
	if err != nil || link != filepath.Join(source, "shared") {
		t.Fatalf("pinned link=%q err=%v", link, err)
	}
	if err := MaterializeRootAt(nil, source, nil, RootRulesFromConfig(config.Workspace{})); err == nil {
		t.Fatal("nil destination root was accepted")
	}
}

// TestMaterializeRootAtRejectsSymlinkAncestorInCopyRuleは、copy ruleの存在検査でErrNotExist以外を扱う分岐を確認する。
// symlink祖先を通るcopy ruleは単なる「欠落」とせず拒否する。
func TestMaterializeRootAtRejectsSymlinkAncestorInCopyRule(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "linked")); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	destinationRoot, err := OpenPhysicalRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destinationRoot.Close() }()
	rules := RootRulesFromConfig(config.Workspace{Copy: []string{filepath.Join("linked", "value")}})
	if err := MaterializeRootAt(nil, source, destinationRoot, rules); err == nil {
		t.Fatal("workspace copy rule through a symlink ancestor was accepted")
	}
}
