package daemon

import (
	"slices"
	"testing"
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
