package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
)

// categories は reconcile と prune の境界なので、typed report から作る分類済み文字列の形を固定する。
func TestArtifactReportCategoriesKeepTheSortedStringShape(t *testing.T) {
	report := artifactReport{
		UnknownPaths: []string{"/root/b", "/root/a"},
		Missing:      []missingArtifact{{SlotID: "slot-1", Path: "/root/leased", State: "LEASED"}},
		UnknownRefs:  []recoveryRefIssue{{RepositoryID: "repo", Ref: "refs/wx/recovery/b"}, {RepositoryID: "repo", Ref: "refs/wx/recovery/a"}},
		MismatchedRefs: []recoveryRefIssue{
			{RepositoryID: "repo", Ref: "refs/wx/recovery/mismatched", ExpiresAt: "2026-01-01T00:00:00Z"},
		},
		MissingRefs: []recoveryRefIssue{{RepositoryID: "repo", Ref: "refs/wx/recovery/missing"}},
		Errors:      []string{"inspect slot slot-2: boom"},
	}
	categories := report.categories()
	if got := categories["unknown_paths"].([]string); !slices.Equal(got, []string{"/root/a", "/root/b"}) {
		t.Fatalf("unknown paths=%v", got)
	}
	if got := categories["missing_paths"].([]string); !slices.Equal(got, []string{"/root/leased (slot-1, LEASED)"}) {
		t.Fatalf("missing paths=%v", got)
	}
	if got := categories["unknown_refs"].([]string); !slices.Equal(got, []string{"repo:refs/wx/recovery/a", "repo:refs/wx/recovery/b"}) {
		t.Fatalf("unknown refs=%v", got)
	}
	if got := categories["mismatched_refs"].([]string); !slices.Equal(got, []string{"repo:refs/wx/recovery/mismatched"}) {
		t.Fatalf("mismatched refs=%v", got)
	}
	if got := categories["missing_refs"].([]string); !slices.Equal(got, []string{"repo:refs/wx/recovery/missing"}) {
		t.Fatalf("missing refs=%v", got)
	}
	if got := categories["errors"].([]string); !slices.Equal(got, []string{"inspect slot slot-2: boom"}) {
		t.Fatalf("errors=%v", got)
	}
}

// 分類済み文字列は表示用の派生値であり、typed report を書き換えない。
func TestArtifactReportCategoriesDoNotMutateTheReport(t *testing.T) {
	report := artifactReport{UnknownPaths: []string{"/root/b", "/root/a"}}
	_ = report.categories()
	if !slices.Equal(report.UnknownPaths, []string{"/root/b", "/root/a"}) {
		t.Fatalf("report unknown paths=%v, want the detection order", report.UnknownPaths)
	}
}

// TestArtifactReportKeepsUnreferencedUnreadableRepositoriesOutOfErrors は、
// forget 後に残った repository 記録で doctor が恒久的に失敗しないことを確認する。
// repositories の行を消す経路が無いため、この分類だけが利用者の手当てなしに解消する道である。
func TestArtifactReportKeepsUnreferencedUnreadableRepositoriesOutOfErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = filepath.Join(root, "worktrees")
	m := testManager(t, cfg, store)
	defer m.Close()
	ctx := context.Background()
	gone := filepath.Join(root, "gone")
	if err := os.MkdirAll(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := openTestDatabase(t, filepath.Join(root, "state.db"))
	if _, err := raw.ExecContext(ctx, `INSERT INTO repositories(id,main_worktree_path,common_git_dir,default_branch,remote_name,first_seen_at,last_seen_at) VALUES('gone',?,?,'main','',?,?)`,
		gone, filepath.Join(gone, ".git"), state.FormatTime(time.Now()), state.FormatTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	report := m.artifactOwnershipReport(ctx)
	if len(report.Errors) != 0 {
		t.Fatalf("ownership errors=%v", report.Errors)
	}
	if len(report.UnreadableRepositories) != 1 || report.UnreadableRepositories[0].Path != gone {
		t.Fatalf("unreadable repositories=%+v", report.UnreadableRepositories)
	}
	// categories は reconcile と prune の境界であり、この分類は隔離記録の対象にしない。
	categories := report.categories()
	if errorsList, _ := categories["errors"].([]string); len(errorsList) != 0 {
		t.Fatalf("categories errors=%v", errorsList)
	}
}
