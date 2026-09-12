package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 1 repository の for-each-ref 失敗は、その repository の問題として報告し、他の repository の分類は続ける。
// errors へ積むと doctor が「検査できなかった」を返し、どの repository の話かも分からなくなる。
func TestRefListFailureStaysScopedToItsRepository(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	databasePath := filepath.Join(root, "state.db")
	store, err := openTestStoreAtPath(t, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	registered, forgotten := filepath.Join(root, "registered"), filepath.Join(root, "forgotten")
	for _, path := range []string{registered, forgotten} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	raw := openTestDatabase(t, databasePath)
	stamp := state.FormatTime(time.Now())
	for id, path := range map[string]string{"registered": registered, "forgotten": forgotten} {
		if _, err := raw.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES(?,?,?,'main','',?,?)`,
			id, path, filepath.Join(path, ".git"), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO workspaces(id,root_path,kind,generation,discovery_state,first_seen_at,last_seen_at,last_reconciled_at) VALUES('workspace',?,'repository',1,'READY',?,?,?)`,
		registered, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO workspace_repositories(workspace_id,repository_id,relative_path,ordinal) VALUES('workspace','registered','',0)`); err != nil {
		t.Fatal(err)
	}
	report := m.artifactOwnershipReport(ctx)
	if len(report.Errors) != 0 {
		t.Fatalf("ownership errors=%v", report.Errors)
	}
	if len(report.RefListFailures) != 1 || report.RefListFailures[0].Path != registered {
		t.Fatalf("ref list failures=%+v", report.RefListFailures)
	}
	// 失敗した repository の後も走査が続くため、登録の無い repository は参考情報として分類される。
	if len(report.UnreadableRepositories) != 1 || report.UnreadableRepositories[0].Path != forgotten {
		t.Fatalf("unreadable repositories=%+v", report.UnreadableRepositories)
	}
	// prune と reconcile が使う分類では、どの repository の失敗かを path で示す。
	errorsList, _ := report.categories()["errors"].([]string)
	if len(errorsList) != 1 || !strings.Contains(errorsList[0], registered) {
		t.Fatalf("categories errors=%v", errorsList)
	}
	var scoped []diag.Finding
	for _, finding := range m.artifactFindings(ctx) {
		if finding.Severity == diag.SeverityUnchecked {
			t.Fatalf("a single repository failure left the ownership check unchecked: %+v", finding)
		}
		if finding.Target == registered {
			scoped = append(scoped, finding)
		}
	}
	if len(scoped) != 1 || scoped[0].Severity != diag.SeverityProblem || scoped[0].Action == "" {
		t.Fatalf("findings for the failing repository=%+v", scoped)
	}
}
