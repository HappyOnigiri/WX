package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestPrepareStagedPreservesRulesIndexFilterAndHookContract(t *testing.T) {
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	files := map[string]string{
		"CLAUDE.md": "instructions\n", ".codex/config.toml": "config\n", "src/AGENTS.md": "nested\n",
		".gitattributes": "*.filtered filter=wx\n", "rules.filtered": "filter input\n",
		".gitignore": "local\nshared\nAGENTS.local.md\n", ".worktreeinclude": "local\n",
		".worktreelink": "shared\n", "shared/value": "shared\n",
	}
	for path, content := range files {
		path = filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("CLAUDE.md", filepath.Join(source, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "ordinary"), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1500; i++ {
		if err := os.WriteFile(filepath.Join(source, "ordinary", fmt.Sprintf("file-%04d", i)), []byte("ordinary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gitCommand(t, source, "config", "filter.wx.smudge", "sed s/input/output/")
	gitCommand(t, source, "config", "filter.wx.clean", "sed s/output/input/")
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "startup fixtures")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	for path, content := range map[string]string{"AGENTS.local.md": "local rules\n", "local/early": "early\n", "local/late": "late\n"} {
		path = filepath.Join(source, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// tracked な指示の source 側変更が先行展開へ混ざらないことを確かめる。
	if err := os.WriteFile(filepath.Join(source, "CLAUDE.md"), []byte("dirty source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	preparer.Config.Readiness.EarlyPaths = []string{"local/early", "shared", "rules.filtered", "not-planned"}
	hookLog := filepath.Join(t.TempDir(), "hook")
	script := "#!/bin/sh\nset -eu\ntest -f tracked\ntest -f src/AGENTS.md\nprintf '%s %s %s\\n' \"$1\" \"$2\" \"$3\" >> '" + hookLog + "'\n"
	if err := os.WriteFile(filepath.Join(string(repo.CommonDir), "hooks", "post-checkout"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	early := false
	var earlyInfo os.FileInfo
	err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
		early = true
		for _, path := range []string{"AGENTS.md", "CLAUDE.md", "AGENTS.local.md", ".codex/config.toml", "local/early", "shared/value", "rules.filtered"} {
			if _, err := os.Stat(filepath.Join(target, path)); err != nil {
				t.Errorf("early file %s: %v", path, err)
			}
		}
		for _, path := range []string{"tracked", "src/AGENTS.md", "local/late", "not-planned", "ordinary"} {
			if _, err := os.Lstat(filepath.Join(target, path)); !os.IsNotExist(err) {
				t.Errorf("late/unplanned file %s present: %v", path, err)
			}
		}
		if _, err := os.Stat(hookLog); !os.IsNotExist(err) {
			t.Errorf("hook ran before full checkout: %v", err)
		}
		content, err := os.ReadFile(filepath.Join(target, "CLAUDE.md"))
		if err != nil || string(content) != "instructions\n" {
			t.Errorf("rules=%q: %v", content, err)
		}
		earlyInfo, err = os.Stat(filepath.Join(target, "local", "early"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !early {
		t.Fatal("early boundary not reached")
	}
	after, err := os.Stat(filepath.Join(target, "local", "early"))
	if err != nil || !os.SameFile(earlyInfo, after) || !earlyInfo.ModTime().Equal(after.ModTime()) {
		t.Fatalf("early include rewritten: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(target, "rules.filtered"))
	if err != nil || string(content) != "filter output\n" {
		t.Fatalf("filter result=%q: %v", content, err)
	}
	if link, err := os.Readlink(filepath.Join(target, "AGENTS.md")); err != nil || link != "CLAUDE.md" {
		t.Fatalf("symlink=%q: %v", link, err)
	}
	hooks, err := os.ReadFile(hookLog)
	if err != nil || string(hooks) != strings.Repeat("0", len(oid))+" "+oid+" 1\n" {
		t.Fatalf("hook=%q: %v", hooks, err)
	}
	if status := gitOutput(t, target, "status", "--porcelain", "--untracked-files=no"); status != "" {
		t.Fatalf("dirty final tree/index: %s", status)
	}
}

func TestPrepareStagedDefaultsDisabledIncludesAndGitlinks(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		t.Run(name, func(t *testing.T) {
			source, repo, preparer, oid, target := prepareEdgesFixture(t)
			preparer.Config.Includes.DefaultAgentRules = enabled
			preparer.Config.Storage.CopyMode = config.CopyModeCopy
			for _, path := range defaultIncludeNames {
				if err := os.WriteFile(filepath.Join(source, path), []byte(path), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// gitlink は通常の checkout 同様、空ディレクトリだけを作る。
			gitCommand(t, source, "update-index", "--add", "--cacheinfo", "160000,"+oid+",module")
			gitCommand(t, source, "commit", "-m", "gitlink")
			oid = gitOutput(t, source, "rev-parse", "HEAD")
			err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
				for _, path := range defaultIncludeNames {
					_, err := os.Stat(filepath.Join(target, path))
					if enabled && err != nil {
						t.Errorf("default %s missing: %v", path, err)
					}
					if !enabled && !os.IsNotExist(err) {
						t.Errorf("disabled default %s copied: %v", path, err)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(target, "module"))
			if err != nil || !info.IsDir() {
				t.Fatalf("gitlink directory: %v", err)
			}
			if got := gitOutput(t, target, "status", "--porcelain", "--untracked-files=no"); got != "" {
				t.Fatalf("gitlink status: %s", got)
			}
		})
	}
}

func TestPrepareStagedStopsBeforeRemainingWritesWhenEarlyCASFails(t *testing.T) {
	_, repo, preparer, oid, target := prepareEdgesFixture(t)
	failure := errors.New("early readiness CAS failed")
	err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "tracked")); !os.IsNotExist(err) {
		t.Fatalf("remaining checkout ran: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, ".git")); err != nil {
		t.Fatalf("partial preparation discarded: %v", err)
	}
}

func TestPrepareStagedUsesRequestedAttributesAfterEarlyIncludes(t *testing.T) {
	source, repo, preparer, oid, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	gitCommand(t, source, "config", "filter.wx.smudge", "sed s/base/filtered/")
	gitCommand(t, source, "config", "filter.wx.clean", "sed s/filtered/base/")
	if err := os.WriteFile(filepath.Join(source, ".gitattributes"), []byte("tracked filter=wx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".worktreeinclude"), []byte(".gitattributes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
		if _, err := os.Stat(filepath.Join(target, ".gitattributes")); err != nil {
			t.Errorf("early attributes missing: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "tracked")); err != nil || string(data) != "base\n" {
		t.Fatalf("untracked early attributes changed checkout: %q, %v", data, err)
	}
}
