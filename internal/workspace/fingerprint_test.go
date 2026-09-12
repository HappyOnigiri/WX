package workspace

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func TestFingerprintRejectsMissingExplicitCopyAndAllowsMissingDefaults(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	cfg := config.Defaults()
	if _, err := Fingerprint(1, "oid", repo, cfg); err != nil {
		t.Fatalf("missing default workspace copies should remain optional: %v", err)
	}
	cfg.Workspaces[source] = config.Workspace{Copy: []string{"required.json"}}
	err := func() error {
		_, err := Fingerprint(1, "oid", repo, cfg)
		return err
	}()
	if err == nil {
		t.Fatal("missing explicit workspace copy fingerprint succeeded")
	}
	missing := filepath.Join(source, "required.json")
	if !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), source) {
		t.Fatalf("missing explicit copy fingerprint error=%v", err)
	}
}

func TestFingerprintTracksMaterializedCopyInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("local.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(repository, "local.env")
	if err := os.WriteFile(local, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: "repository"}
	cfg := config.Defaults()
	first, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || second == first {
		t.Fatalf("include content fingerprint first=%s second=%s err=%v", first, second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || third == second {
		t.Fatalf("workspace copy fingerprint second=%s third=%s err=%v", second, third, err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom.txt"), []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Workspaces[root] = config.Workspace{Copy: []string{"custom.txt"}}
	fourth, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil || fourth == third {
		t.Fatalf("workspace rule fingerprint third=%s fourth=%s err=%v", third, fourth, err)
	}
}

func TestFingerprintDistinguishesPrepareArgumentBoundaries(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: config.Prepare{
		Command: []string{"/usr/bin/printf", "%s|", "a b", "c"},
		Version: "v1",
	}}
	first, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}

	cfg.Repositories[string(repo.MainPath)] = config.Repository{Prepare: config.Prepare{
		Command: []string{"/usr/bin/printf", "%s|", "a", "b c"},
		Version: "v1",
	}}
	second, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("prepare argument boundaries were lost: fingerprint=%s", first)
	}
}

func TestFingerprintSchemaMismatchInvalidatesPreviousValue(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	cfg := config.Defaults()
	legacy, err := fingerprintWithSchema(fingerprintSchemaVersion-1, 1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	current, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if current == legacy {
		t.Fatalf("fingerprint schema change did not invalidate previous value: %s", current)
	}
}

// repository 個別の共有下限は、その repository の fingerprint だけを変える。
// 同じ設定に居る他 repository の hash が動かないことが、READY standby を巻き添えで捨てない根拠になる。
func TestFingerprintFollowsRepositoryCOWMinSize(t *testing.T) {
	t.Parallel()
	a := discovery.Repository{MainPath: domain.CanonicalPath(t.TempDir())}
	b := discovery.Repository{MainPath: domain.CanonicalPath(t.TempDir())}
	cfg := config.Defaults()
	fingerprints := func() (string, string, string) {
		t.Helper()
		first, err := Fingerprint(1, "oid", a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Fingerprint(1, "oid", b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		update, err := UpdateCompatibilityFingerprint(1, a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return first, second, update
	}
	beforeA, beforeB, beforeUpdateA := fingerprints()
	minimum := config.DefaultCOWMinSizeKiB * 2
	cfg.Repositories[string(a.MainPath)] = config.Repository{COWMinSizeKiB: &minimum}
	afterA, afterB, afterUpdateA := fingerprints()
	if afterA == beforeA || afterUpdateA == beforeUpdateA {
		t.Fatalf("a repository minimum did not change its own fingerprint: %s %s", afterA, afterUpdateA)
	}
	if afterB != beforeB {
		t.Fatalf("another repository's fingerprint changed: %s want=%s", afterB, beforeB)
	}
}

func TestFingerprintCoversRecursiveDuplicateAndWorkspaceLinkInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repository := filepath.Join(root, "nested", "repository")
	if err := os.MkdirAll(filepath.Join(repository, "included", "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "included", "child", "value"), []byte("one\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("included\nincluded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom.txt"), []byte("custom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "shared"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: filepath.Join("nested", "repository")}
	cfg := config.Defaults()
	cfg.Workspaces[root] = config.Workspace{
		Copy: []string{"custom.txt", "custom.txt"},
		Link: []string{"shared"},
	}
	cfg.Repositories[repository] = config.Repository{Prepare: config.Prepare{Command: []string{"true"}, Version: "v2"}}
	first, err := Fingerprint(2, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "included", "child", "value"), []byte("two\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	second, err := Fingerprint(2, "oid", repo, cfg)
	if err != nil || first == second {
		t.Fatalf("recursive fingerprint first=%s second=%s err=%v", first, second, err)
	}

	if err := os.RemoveAll(filepath.Join(repository, "included")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, filepath.Join(repository, "included")); err != nil {
		t.Fatal(err)
	}
	// symlink の include は materializer が skip するため、fingerprint も失敗せず skip 済みとして値が変わる。
	third, err := Fingerprint(2, "oid", repo, cfg)
	if err != nil || third == second {
		t.Fatalf("include symlink fingerprint=%s err=%v", third, err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), []byte("../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(2, "oid", repo, cfg); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("unsafe fingerprint include error=%v", err)
	}

	cleanRepo := discovery.Repository{MainPath: domain.CanonicalPath(repository), RelativePath: "../outside"}
	if err := os.WriteFile(filepath.Join(repository, ".worktreeinclude"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Fingerprint(2, "oid", cleanRepo, cfg); err == nil {
		t.Fatal("unsafe repository relative path was fingerprinted")
	}

	unsafeCopy := cfg
	unsafeCopy.Workspaces[root] = config.Workspace{Copy: []string{"../outside"}}
	if _, err := Fingerprint(2, "oid", repo, unsafeCopy); err == nil {
		t.Fatal("unsafe workspace copy path was fingerprinted")
	}
	unsafeLink := cfg
	unsafeLink.Workspaces[root] = config.Workspace{Link: []string{"../outside"}}
	if _, err := Fingerprint(2, "oid", repo, unsafeLink); err == nil {
		t.Fatal("unsafe workspace link path was fingerprinted")
	}
	missingLink := cfg
	missingLink.Workspaces[root] = config.Workspace{Link: []string{"missing"}}
	if _, err := Fingerprint(2, "oid", repo, missingLink); err == nil {
		t.Fatal("missing workspace link path was fingerprinted")
	}
}

func TestFingerprintTracksDefaultIncludeContent(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "init", "-b", "main")
	cfg := config.Defaults()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(repository)}
	bare, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	disabledCfg := cfg
	disabledCfg.Includes.DefaultAgentRules = false
	disabled, err := Fingerprint(1, "oid", repo, disabledCfg)
	if err != nil {
		t.Fatal(err)
	}
	if disabled != bare {
		t.Fatal("disabling default includes changed the fingerprint without materialized content")
	}
	added, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if added == bare {
		t.Fatal("an added default include left the fingerprint unchanged")
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	edited, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if edited == added {
		t.Fatal("an edited default include left the fingerprint unchanged")
	}
	// 明示的な link ルールが所有するパスはコピーせず、その内容で slot も
	// 再構築してはならない。
	if err := os.WriteFile(filepath.Join(repository, ".worktreelink"), []byte("GEMINI.local.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "GEMINI.local.md"), []byte("third\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relinked, err := Fingerprint(1, "oid", repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if relinked != linked {
		t.Fatal("a linked default include still contributed its content")
	}
}

// TestFingerprintRejectsAMissingRepositoryMainPathは、存在しないrepositoryに対するFingerprint先頭のphysical-path検査を確認する。
// 別テストのsymlink祖先や入力不可のケースとは異なる。
func TestFingerprintRejectsAMissingRepositoryMainPath(t *testing.T) {
	t.Parallel()
	missing := domain.CanonicalPath(filepath.Join(t.TempDir(), "missing-repository"))
	if _, err := Fingerprint(1, "oid", discovery.Repository{MainPath: missing}, config.Defaults()); err == nil {
		t.Fatal("fingerprint of a missing repository main path succeeded")
	}
}

func TestFingerprintAndRelativePathBoundaries(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "deep", "file"), []byte("fingerprint"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	owner, err := OpenPhysicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	for _, test := range []struct {
		name    string
		rel     string
		bad     bool
		skipped bool
	}{
		{name: "directory", rel: "nested"},
		{name: "file", rel: "nested/deep/file"},
		{name: "missing", rel: "missing", bad: true},
		{name: "symlink", rel: "link", skipped: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := sha256.New()
			err := fingerprintRootPath(h, owner, test.rel, test.rel)
			if test.bad {
				if err == nil {
					t.Fatal("unsafe fingerprint input succeeded")
				}
				return
			}
			if test.skipped {
				marker := sha256.New()
				_, _ = fmt.Fprintf(marker, "path=%s skipped-symlink\n", test.rel)
				if err != nil {
					t.Fatalf("symlink fingerprint: %v", err)
				}
				if !bytes.Equal(h.Sum(nil), marker.Sum(nil)) {
					t.Fatal("symlink fingerprint did not record the skip marker")
				}
				return
			}
			if err != nil {
				t.Fatalf("fingerprintRootPath: %v", err)
			}
			if h.Size() == 0 {
				t.Fatal("fingerprint did not include metadata or file contents")
			}
		})
	}
	if err := fingerprintPath(sha256.New(), root, filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("fingerprintPath accepted an outside path")
	}
	if got, err := repositoryWorkspaceRoot(discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "nested/deep"}); err != nil || got != filepath.Dir(filepath.Dir(root)) {
		t.Fatalf("repository workspace root=%q err=%v", got, err)
	}
	if _, err := repositoryWorkspaceRoot(discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "../outside"}); err == nil {
		t.Fatal("unsafe repository relative path accepted")
	}
	for _, value := range []string{"", ".", "..", "../escape", "/absolute", "nested/file"} {
		_, err := safeRelative(value)
		if (value == "nested/file") == (err != nil) {
			t.Fatalf("safeRelative(%q) err=%v", value, err)
		}
	}
}

// submodule 方針は両 fingerprint に入り、方針変更後に旧方針の READY slot を再利用させない。
// 更新経路の `checkout --detach --force` は submodule を実体化しないため、更新互換側にも必要である。
func TestSubmodulePolicyChangesBothFingerprints(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(source)}
	seenPrepare := map[string]bool{}
	seenUpdate := map[string]bool{}
	for _, enabled := range []bool{true, false} {
		cfg := config.Defaults()
		cfg.Worktree.Submodules = enabled
		prepare, err := Fingerprint(1, "oid", repo, cfg)
		if err != nil {
			t.Fatal(err)
		}
		update, err := UpdateCompatibilityFingerprint(1, repo, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if seenPrepare[prepare] || seenUpdate[update] {
			t.Fatalf("submodules=%t did not change both fingerprints", enabled)
		}
		seenPrepare[prepare] = true
		seenUpdate[update] = true
		// workspace 個別の上書きも同じ hash 入力として効く。
		cfg.Worktree.Submodules = !enabled
		cfg.Workspaces[source] = config.Workspace{Submodules: &enabled}
		overridden, err := Fingerprint(1, "oid", repo, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if overridden != prepare {
			t.Fatalf("workspace override submodules=%t did not resolve to the global equivalent", enabled)
		}
	}
}

// repository 個別の copy_mode は、その repository の fingerprint と更新互換 fingerprint だけを変える。
// 個別指定が1つも無い設定では、以前と同じ値のままでなければ全 READY slot が無効になる。
func TestFingerprintFollowsRepositoryCopyMode(t *testing.T) {
	t.Parallel()
	a := discovery.Repository{MainPath: domain.CanonicalPath(t.TempDir())}
	b := discovery.Repository{MainPath: domain.CanonicalPath(t.TempDir())}
	cfg := config.Defaults()
	values := func() (string, string, string) {
		t.Helper()
		first, err := Fingerprint(1, "oid", a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Fingerprint(1, "oid", b, cfg)
		if err != nil {
			t.Fatal(err)
		}
		update, err := UpdateCompatibilityFingerprint(1, a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return first, second, update
	}
	beforeA, beforeB, beforeUpdateA := values()
	// 上書きの無い設定を組み直しても同じ値になる。schema を上げていないことの確認でもある。
	if againA, againB, againUpdateA := values(); againA != beforeA || againB != beforeB || againUpdateA != beforeUpdateA {
		t.Fatal("fingerprints changed without any override")
	}
	cfg.Repositories[string(a.MainPath)] = config.Repository{Storage: config.RepositoryStorage{CopyMode: config.CopyModeCopy}}
	afterA, afterB, afterUpdateA := values()
	if afterA == beforeA || afterUpdateA == beforeUpdateA {
		t.Fatalf("a repository copy mode did not change its own fingerprint: %s %s", afterA, afterUpdateA)
	}
	if afterB != beforeB {
		t.Fatalf("another repository's fingerprint changed: %s want=%s", afterB, beforeB)
	}
}
