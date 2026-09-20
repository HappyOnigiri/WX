package workspace

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func writeRootFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func containsRule(rules []string, want string) bool {
	for _, rule := range rules {
		if rule == filepath.Clean(want) {
			return true
		}
	}
	return false
}

func TestResolveRootRulesAddsAgentAssetsAndManifestEntries(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, filepath.Join(".claude", "skills", "local-design-memo", "SKILL.md"), "skill\n")
	writeRootFile(t, source, ".envrc", "export A=1\n")
	writeRootFile(t, source, filepath.Join("bin", "tool"), "#!/bin/sh\n")
	writeRootFile(t, source, filepath.Join("shared", "value"), "shared\n")
	writeRootFile(t, source, ".worktreeinclude", "# comment\n.envrc\nbin\nabsent*\n")
	writeRootFile(t, source, ".worktreelink", "shared\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(".claude", "skills"), ".envrc", "bin"} {
		if !containsRule(rules.OptionalCopy, want) {
			t.Fatalf("optional copies=%v missing %q", rules.OptionalCopy, want)
		}
	}
	if !containsRule(rules.Link, "shared") {
		t.Fatalf("links=%v missing the manifest link", rules.Link)
	}
	if len(rules.Copy) != 0 {
		t.Fatalf("manifest entries must not become required copies: %v", rules.Copy)
	}

	target := t.TempDir()
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".claude", "skills", "local-design-memo", "SKILL.md")); err != nil {
		t.Fatalf("root skill was not materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".envrc")); err != nil {
		t.Fatalf("manifest include was not materialized: %v", err)
	}
	info, err := os.Lstat(filepath.Join(target, "shared"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("manifest link was not materialized as a symlink: mode=%v err=%v", info, err)
	}
	if _, err := os.Stat(filepath.Join(target, ".claude", "settings.local.json")); !os.IsNotExist(err) {
		t.Fatalf("only the declared agent asset subpaths belong in the slot: %v", err)
	}
}

func TestResolveRootRulesSkipsAgentAssetsOwnedByLinks(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, filepath.Join(".claude", "skills", "SKILL.md"), "skill\n")
	writeRootFile(t, source, ".worktreelink", ".claude\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules.OptionalCopy {
		if strings.HasPrefix(rule, ".claude") {
			t.Fatalf("optional copies=%v still claim a linked path", rules.OptionalCopy)
		}
	}
	target := t.TempDir()
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatalf("linked agent directory should not conflict with the defaults: %v", err)
	}
}

func TestResolveRootRulesSkipsDefaultCopyNamesOwnedByLinks(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, "AGENTS.md", "agents\n")
	writeRootFile(t, source, ".worktreelink", "AGENTS.md\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatalf("default copy name claimed by a link must not conflict: %v", err)
	}
	if slices.Contains(rules.OptionalCopy, "AGENTS.md") {
		t.Fatalf("optional copies=%v still claim the linked default name", rules.OptionalCopy)
	}
	if !slices.Contains(rules.Link, "AGENTS.md") {
		t.Fatalf("manifest link was dropped: %+v", rules)
	}
	if !slices.Contains(rules.OptionalCopy, "CLAUDE.md") {
		t.Fatalf("unrelated default copy names were dropped: %+v", rules)
	}
	target := t.TempDir()
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatalf("linked default name should not conflict with the defaults: %v", err)
	}
	info, err := os.Lstat(filepath.Join(target, "AGENTS.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("linked default name was not materialized as a symlink: mode=%v err=%v", info, err)
	}
}

// RootRulesFromConfig は repository の main worktree と同じ root で使い、manifest も既定名の除去も効かせない。
func TestRootRulesFromConfigKeepsDefaultCopyNamesUnderLinks(t *testing.T) {
	t.Parallel()
	rules := RootRulesFromConfig(config.Workspace{Link: []string{"AGENTS.md"}})
	if !slices.Contains(rules.OptionalCopy, "AGENTS.md") {
		t.Fatalf("single-repository defaults changed: %+v", rules)
	}
}

func TestResolveRootRulesRejectsCopyAndLinkOverlap(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, filepath.Join("shared", "value"), "shared\n")
	writeRootFile(t, source, ".worktreeinclude", "shared\n")
	writeRootFile(t, source, ".worktreelink", "shared\n")

	_, err := ResolveRootRules(source, config.Workspace{})
	if err == nil {
		t.Fatal("copy and link overlap resolved without an error")
	}
	if !strings.Contains(err.Error(), ".worktreeinclude") || !strings.Contains(err.Error(), source) {
		t.Fatalf("conflict error must name the workspace root and the manifests: %v", err)
	}
}

func TestResolveRootRulesWithoutManifestsKeepsConfigRules(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	rules, err := ResolveRootRules(source, config.Workspace{Copy: []string{"required.json"}, Link: []string{"shared"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Copy) != 1 || rules.Copy[0] != "required.json" || len(rules.Link) != 1 || rules.Link[0] != "shared" {
		t.Fatalf("config rules changed without manifests: %+v", rules)
	}
	missing, err := ResolveRootRules(filepath.Join(source, "absent"), config.Workspace{Link: []string{"shared"}})
	if err != nil {
		t.Fatalf("missing workspace root must not fail rule resolution: %v", err)
	}
	if len(missing.Link) != 1 {
		t.Fatalf("config rules lost for a missing workspace root: %+v", missing)
	}
}

// TestFingerprintIgnoresRootAgentAssetsForRepositoryWorkspace は単一 repository workspace の再利用判定が変わらないことを固定する。
// root が repository の main worktree そのものなら root 直下は Git が checkout し、非 Git root 向けの既定を混ぜてはならない。
func TestFingerprintIgnoresRootAgentAssetsForRepositoryWorkspace(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	cfg := config.Defaults()
	before, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeRootFile(t, source, filepath.Join(".claude", "skills", "SKILL.md"), "skill\n")
	after, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("repository workspace fingerprint changed before=%s after=%s", before, after)
	}
}

func TestFingerprintTracksRootAgentAssetsForMultiRepositoryWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: "repository"}
	cfg := config.Defaults()
	before, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	skill := writeRootFile(t, root, filepath.Join(".claude", "skills", "SKILL.md"), "skill\n")
	added, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || added == before {
		t.Fatalf("root skill fingerprint before=%s added=%s err=%v", before, added, err)
	}
	if err := os.WriteFile(skill, []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || edited == added {
		t.Fatalf("root skill content fingerprint added=%s edited=%s err=%v", added, edited, err)
	}
	writeRootFile(t, root, ".worktreelink", "shared\n")
	writeRootFile(t, root, filepath.Join("shared", "value"), "shared\n")
	linked, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || linked == edited {
		t.Fatalf("root manifest link fingerprint edited=%s linked=%s err=%v", edited, linked, err)
	}
}

// TestPlanRootStagesPlacesRootAgentAssetsEarly は agent 起動前に root の skill が読めることを固定する。
func TestPlanRootStagesPlacesRootAgentAssetsEarly(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, filepath.Join(".claude", "skills", "SKILL.md"), "skill\n")
	writeRootFile(t, source, "late.txt", "late\n")
	writeRootFile(t, source, ".worktreeinclude", "late.txt\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatal(err)
	}
	stage, err := PlanRootStages(nil, source, rules, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	destination, err := domain.EnsurePhysicalDirectoryRoot(target, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close() }()
	if err := stage.Materialize(destination, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, ".claude", "skills", "SKILL.md")); err != nil {
		t.Fatalf("root skill was not placed in the early stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "late.txt")); !os.IsNotExist(err) {
		t.Fatalf("non-early include was placed too soon: %v", err)
	}
	if err := stage.Materialize(destination, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "late.txt")); err != nil {
		t.Fatalf("include was not placed in the late stage: %v", err)
	}
}

// TestResolveRootRulesExpandsLinkGlobs は、非 Git workspace root の `.worktreelink` でも glob が展開され、
// 展開後の path が既定 agent 資産の optional copy を落とすことを確かめる。
// 既定を落とさないと同じ path を copy と link の両方が所有し、rule 衝突で準備が失敗する。
func TestResolveRootRulesExpandsLinkGlobs(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, filepath.Join(".claude", "skills", "local-a", "SKILL.md"), "a\n")
	writeRootFile(t, source, filepath.Join(".claude", "skills", "local-b", "SKILL.md"), "b\n")
	writeRootFile(t, source, filepath.Join("bin", "one"), "#!/bin/sh\n")
	writeRootFile(t, source, filepath.Join("bin", "two"), "#!/bin/sh\n")
	writeRootFile(t, source, ".worktreelink", ".claude/skills/local-*/\nbin/*\nabsent-*\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(".claude", "skills", "local-a"), filepath.Join(".claude", "skills", "local-b"), filepath.Join("bin", "one"), filepath.Join("bin", "two")}
	if !slices.Equal(rules.Link, want) {
		t.Fatalf("links=%v want=%v", rules.Link, want)
	}
	if containsRule(rules.OptionalCopy, filepath.Join(".claude", "skills")) {
		t.Fatalf("expanded links did not drop the default agent asset: %v", rules.OptionalCopy)
	}

	target := t.TempDir()
	if err := MaterializeRoot(nil, source, target, rules); err != nil {
		t.Fatal(err)
	}
	for _, relative := range want {
		info, err := os.Lstat(filepath.Join(target, relative))
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expanded link %s was not a symlink: mode=%v err=%v", relative, info, err)
		}
	}
}

// TestResolveRootRulesKeepsLiteralLinkRequired は、メタ文字を含まない行が展開されずに残り、
// 欠落が準備失敗として扱われる既存の契約を保つことを確かめる。glob 行の 0 件は従来どおり成功させる。
func TestResolveRootRulesKeepsLiteralLinkRequired(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeRootFile(t, source, ".worktreelink", "missing-literal\nabsent-*\n")

	rules, err := ResolveRootRules(source, config.Workspace{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rules.Link, []string{"missing-literal"}) {
		t.Fatalf("links=%v want the literal line only", rules.Link)
	}
	if err := MaterializeRoot(nil, source, t.TempDir(), rules); err == nil || !strings.Contains(err.Error(), "missing-literal") {
		t.Fatalf("a missing literal link must fail the preparation: %v", err)
	}
}
