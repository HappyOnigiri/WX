package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestDescribeReadyMismatchNamesIncludePathAndOID(t *testing.T) {
	requireDaemonIntegration(t)
	root := t.TempDir()
	repository := filepath.Join(root, "repo")
	initGitRepo(t, repository)
	for name, body := range map[string]string{
		".gitignore":       "local.cfg\nlocal.extra\n",
		".worktreeinclude": "local.cfg\nlocal.extra\n",
	} {
		if err := os.WriteFile(filepath.Join(repository, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", ".gitignore", ".worktreeinclude")
	gitRun(t, repository, "commit", "-m", "add include rules")
	store, err := openTestStoreAtPath(t, filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktree.Undefined = "hot"
	cfg.Pool.WarmPerWorkspace = 1
	m := testManager(t, cfg, store)
	m.git = &gitx.Runner{Timeout: 10 * time.Second}
	defer m.Close()
	ctx := context.Background()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	w, err := discoverer.Resolve(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, store, w)
	raw := openTestDatabase(t, filepath.Join(root, "state.db"))
	if _, err := raw.ExecContext(ctx, `UPDATE repositories SET last_leased_at=?`, state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := m.ensureStandby(ctx, w); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.RecoverJobs(ctx, false)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("prepare jobs=%+v err=%v", jobs, err)
	}
	prepareJob, err := store.ClaimJob(ctx, jobs[0].ID, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.runRecoveredJob(ctx, prepareJob); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, prepareJob.ID, "prepare", nil); err != nil {
		t.Fatal(err)
	}
	ready, ok, err := store.ReadySlot(ctx, string(w.ID))
	if err != nil || !ok {
		t.Fatalf("ready=%+v ok=%t err=%v", ready, ok, err)
	}
	resolved, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	if matched, err := m.readyMatches(ctx, ready, resolved); err != nil || !matched {
		t.Fatalf("prepared standby matched=%t err=%v, want an exact match", matched, err)
	}

	// include 対象の内容変更とファイル追加は、OID を動かさずに fingerprint だけを変える。
	if err := os.WriteFile(filepath.Join(repository, "local.cfg"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "local.extra"), []byte("added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if matched, err := m.readyMatches(ctx, ready, resolved); err != nil || matched {
		t.Fatalf("edited include matched=%t err=%v, want a mismatch", matched, err)
	}
	mismatch := m.describeReadyMismatch(ctx, ready, resolved)
	if mismatch.reason != "fingerprint" {
		t.Fatalf("mismatch=%+v, want reason=fingerprint", mismatch)
	}
	if !strings.Contains(mismatch.detail, "~local.cfg") || !strings.Contains(mismatch.detail, "+local.extra") {
		t.Fatalf("mismatch detail=%q, want the edited and added include paths", mismatch.detail)
	}

	// OID が動いた場合は、内容差より先に OID を理由として報告する。
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("new head\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", "tracked.txt")
	gitRun(t, repository, "commit", "-m", "advance main")
	advanced, err := pool.ResolveBranches(ctx, m.git, w, nil)
	if err != nil {
		t.Fatal(err)
	}
	mismatch = m.describeReadyMismatch(ctx, ready, advanced)
	if mismatch.reason != "oid" || !strings.Contains(mismatch.detail, "->") {
		t.Fatalf("mismatch=%+v, want reason=oid with both OIDs", mismatch)
	}
}

func TestPlacementDriftAndMismatchFormatting(t *testing.T) {
	previous := []state.Placement{
		{RelativePath: "keep", Kind: "copy", SourcePath: "/src/keep", ContentSHA256: "a"},
		{RelativePath: "edited", Kind: "copy", SourcePath: "/src/edited", ContentSHA256: "a"},
		{RelativePath: "removed", Kind: "copy", SourcePath: "/src/removed", ContentSHA256: "a"},
	}
	planned := []state.Placement{
		{RelativePath: "keep", Kind: "copy", SourcePath: "/src/keep", ContentSHA256: "a"},
		{RelativePath: "edited", Kind: "copy", SourcePath: "/src/edited", ContentSHA256: "b"},
		{RelativePath: "added", Kind: "link", SourcePath: "/src/added"},
	}
	if drift := placementDrift(previous, planned); drift != "+added -removed ~edited" {
		t.Fatalf("drift=%q", drift)
	}
	if drift := placementDrift(previous, previous); drift != "" {
		t.Fatalf("identical placements drift=%q, want empty", drift)
	}
	many := make([]state.Placement, 0, mismatchPathLimit+2)
	for i := range mismatchPathLimit + 2 {
		many = append(many, state.Placement{RelativePath: string(rune('a' + i)), Kind: "copy"})
	}
	drift := placementDrift(nil, many)
	if !strings.HasSuffix(drift, "(+2 more)") {
		t.Fatalf("drift=%q, want the remaining count", drift)
	}
	if got := shortOID("0123456789abcdef"); got != "0123456789ab" {
		t.Fatalf("shortOID=%q", got)
	}
	if got := shortOID("short"); got != "short" {
		t.Fatalf("shortOID=%q, want the value unchanged", got)
	}
	if args := (readyMismatch{}).logArgs(); args != nil {
		t.Fatalf("logArgs=%v, want nil for an empty reason", args)
	}
	args := readyMismatch{reason: "oid", detail: "server: a -> b"}.logArgs()
	if len(args) != 4 || args[0] != "mismatch" || args[2] != "mismatch_detail" {
		t.Fatalf("logArgs=%v", args)
	}
}

func TestCowThresholdSummaryUsesRepositoryOverrides(t *testing.T) {
	limit := 64
	cfg := config.Defaults()
	cfg.Repositories = map[string]config.Repository{"/src/api": {COWMinSizeKiB: &limit}}
	w := discovery.Workspace{Repositories: []discovery.Repository{
		{MainPath: "/src/web"},
		{MainPath: "/src/api"},
	}}
	if got := cowThresholdSummary(cfg, w); got != "api=64 web=16" {
		t.Fatalf("summary=%q", got)
	}
}
