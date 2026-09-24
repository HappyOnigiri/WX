package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

func TestPrepareStagedPreservesRulesIndexFilterAndHookContract(t *testing.T) {
	t.Parallel()
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
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
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
	trackedInfo, err := os.Lstat(filepath.Join(target, "tracked"))
	if err != nil {
		t.Fatal(err)
	}
	if trackedInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("regular tracked file was classified as a symlink")
	}
}

// 通常 blob の内容は symlink の参照先ではなく、early path の閉包へ取り込まない。
func TestPrepareStagedDoesNotTreatRegularBlobAsSymlink(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	if err := os.WriteFile(filepath.Join(source, "early-blob"), []byte("late-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "late-file"), []byte("late\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, source, "add", "early-blob", "late-file")
	gitCommand(t, source, "commit", "-m", "regular blob closure")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	preparer.Config.Readiness.EarlyPaths = []string{"early-blob"}
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
		if _, statErr := os.Stat(filepath.Join(target, "early-blob")); statErr != nil {
			return statErr
		}
		if _, statErr := os.Lstat(filepath.Join(target, "late-file")); !os.IsNotExist(statErr) {
			return fmt.Errorf("regular blob pulled late path into early stage: %w", statErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepareStagedDefaultsDisabledIncludesAndGitlinks(t *testing.T) {
	t.Parallel()
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
			_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
				if _, statErr := os.Stat(filepath.Join(target, "module")); !os.IsNotExist(statErr) {
					t.Errorf("gitlink directory appeared during early stage: %v", statErr)
				}
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

// 複数 repository の early/remaining 区間は、開始順の 1 始まり scope を持つ。
func TestPrepareStagedScopesEachRepositoryFromOne(t *testing.T) {
	t.Parallel()
	firstSource, firstRepo, preparer, firstOID, firstTarget := prepareEdgesFixture(t)
	secondSource := filepath.Join(filepath.Dir(firstSource), "second")
	if err := os.Mkdir(secondSource, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, secondSource, "init", "-b", "main")
	gitCommand(t, secondSource, "config", "user.name", "test")
	gitCommand(t, secondSource, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(secondSource, "tracked"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, secondSource, "add", ".")
	gitCommand(t, secondSource, "commit", "-m", "initial")
	secondOID := gitOutput(t, secondSource, "rev-parse", "HEAD")
	secondCommon := gitOutput(t, secondSource, "rev-parse", "--path-format=absolute", "--git-common-dir")
	secondRepo := discovery.Repository{
		ID:           "second",
		MainPath:     domain.CanonicalPath(secondSource),
		CommonDir:    domain.CanonicalPath(secondCommon),
		RelativePath: "second",
	}
	firstRepo.RelativePath = "first"
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	preparer.Phases = &PhaseTimings{}
	secondTarget := filepath.Join(preparer.SlotPath, "second")
	var registrationScopes, checkoutScopes []PhaseScope
	preparer.Git.SetBeforeRunAtHook(func(args []string) {
		active, ok := preparer.Phases.Active()
		if !ok {
			return
		}
		hasArg := func(want string) bool {
			for _, arg := range args {
				if arg == want {
					return true
				}
			}
			return false
		}
		if active.Name == "git-register" && hasArg("add") && hasArg("worktree") {
			registrationScopes = append(registrationScopes, active.Scope)
		}
		if active.Name == "checkout" && hasArg("checkout-index") {
			checkoutScopes = append(checkoutScopes, active.Scope)
		}
	})
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{
		{Repository: firstRepo, Target: firstTarget, OID: firstOID},
		{Repository: secondRepo, Target: secondTarget, OID: secondOID},
	}, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := PhaseScope{Target: "first", Index: 1, Total: 2}
	wantSecond := PhaseScope{Target: "second", Index: 2, Total: 2}
	if len(registrationScopes) != 2 || registrationScopes[0] != wantFirst || registrationScopes[1] != wantSecond {
		t.Fatalf("registration scopes=%+v, want first=%+v second=%+v", registrationScopes, wantFirst, wantSecond)
	}
	if len(checkoutScopes) != 2 || checkoutScopes[0] != wantFirst || checkoutScopes[1] != wantSecond {
		t.Fatalf("checkout scopes=%+v, want first=%+v second=%+v", checkoutScopes, wantFirst, wantSecond)
	}
}

func TestPrepareStagedStopsBeforeRemainingWritesWhenEarlyCASFails(t *testing.T) {
	t.Parallel()
	_, repo, preparer, oid, target := prepareEdgesFixture(t)
	failure := errors.New("early readiness CAS failed")
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return failure })
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
	t.Parallel()
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
	_, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
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

// exit 0 の post-checkout hook が出した出力は、準備を成功させたまま notice として残す。
// 捨ててしまうと、hook が内部の失敗を飲み込んだ回を wx から正常と区別できない。
func TestPrepareStagedRecordsHookOutputOfSuccessfulHook(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	notices := &PrepareNotices{}
	preparer.Notices = notices
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	script := "#!/bin/sh\necho 'submodule update skipped' >&2\necho 'hook done'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(string(repo.CommonDir), "hooks", "post-checkout"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	recorded := notices.Notices()
	if len(recorded) != 1 {
		t.Fatalf("notices = %+v, want one entry", recorded)
	}
	if recorded[0].Phase != "post-checkout" || recorded[0].Target != target {
		t.Fatalf("notice = %+v", recorded[0])
	}
	// `git hook run` は hook の stdout も stderr へ流すため、どちらへ出た行も同じ出力に現れる。
	output := recorded[0].Stdout + recorded[0].Stderr
	if !strings.Contains(output, "submodule update skipped") || !strings.Contains(output, "hook done") {
		t.Fatalf("notice output = %+v", recorded[0])
	}
}

// 出力を出さない hook では notice を作らない。区間を通っただけの回が診断へ並ぶのを避ける。
func TestPrepareStagedRecordsNoNoticeForSilentHook(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	notices := &PrepareNotices{}
	preparer.Notices = notices
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(string(repo.CommonDir), "hooks", "post-checkout"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if recorded := notices.Notices(); len(recorded) != 0 {
		t.Fatalf("notices = %+v, want none", recorded)
	}
}

// TestPrepareStagedStagesExpandedLinkGlobMatches は、readiness.early_paths の照合が展開後の path で行われ、
// 同じ glob から出た match でも早期・後期に分かれることを確かめる。
// 展開しない実装では `local-*` という名前の path が無いため、どの match も早期に振られなかった。
func TestPrepareStagedStagesExpandedLinkGlobMatches(t *testing.T) {
	t.Parallel()
	source, repo, preparer, _, target := prepareEdgesFixture(t)
	preparer.Config.Storage.CopyMode = config.CopyModeCopy
	preparer.Config.Readiness.EarlyPaths = []string{"local-a"}
	writeRepositoryFiles(t, source, map[string]string{
		".gitignore":    "local-*\n",
		".worktreelink": "local-*\n",
	})
	gitCommand(t, source, "add", ".")
	gitCommand(t, source, "commit", "-m", "glob link rule")
	oid := gitOutput(t, source, "rev-parse", "HEAD")
	writeRepositoryFiles(t, source, map[string]string{"local-a/value": "a\n", "local-b/value": "b\n"})
	var earlyLinks []string
	if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error {
		for _, name := range []string{"local-a", "local-b"} {
			if _, err := os.Readlink(filepath.Join(target, name)); err == nil {
				earlyLinks = append(earlyLinks, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("staged preparation failed: %v", err)
	}
	if !slices.Equal(earlyLinks, []string{"local-a"}) {
		t.Fatalf("early links=%v, want only the match named in readiness.early_paths", earlyLinks)
	}
	for _, name := range []string{"local-a", "local-b"} {
		if link, err := os.Readlink(filepath.Join(target, name)); err != nil || link != filepath.Join(source, name) {
			t.Fatalf("final link %s=%q err=%v", name, link, err)
		}
	}
}
