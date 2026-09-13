package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/state"
)

// 保護中の slot は、なぜ保持期限を過ぎても消えないのかと、退避・削除の手順つきで問題として報告する。
func TestUnsavedSubmoduleFindingsReportProtectedSlots(t *testing.T) {
	t.Parallel()
	manager, store, workspaceID := cleanFixture(t)
	ctx := context.Background()
	if findings := manager.unsavedSubmoduleFindings(ctx); len(findings) != 1 || findings[0].Severity != diag.SeverityOK {
		t.Fatalf("findings without any record=%+v", findings)
	}
	slot := testSlot(t, manager, workspaceID, "protected", 1, "READY")
	if _, err := store.CreateStandby(ctx, slot, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceUnsavedSubmodules(ctx, slot.ID, "repository", []state.UnsavedSubmodule{
		{RepositoryID: "repository", Path: "sub/kid", Reasons: "MODIFIED,UNTRACKED"},
		{RepositoryID: "repository", Reasons: "UNDETERMINED"},
	}); err != nil {
		t.Fatal(err)
	}
	findings := manager.unsavedSubmoduleFindings(ctx)
	if len(findings) != 1 {
		t.Fatalf("findings=%+v, want one per protected slot", findings)
	}
	finding := findings[0]
	if finding.Check != diag.CheckUnsavedSubmodules || finding.Severity != diag.SeverityProblem || finding.Target != slot.Path {
		t.Fatalf("finding=%+v, want a problem targeting %s", finding, slot.Path)
	}
	if !strings.Contains(finding.Action, "wx clear --discard") {
		t.Fatalf("action=%q, want the explicit deletion route", finding.Action)
	}
	if len(finding.Details) != 2 || finding.Details[0] != "repository repository (UNDETERMINED)" || finding.Details[1] != "sub/kid (MODIFIED UNTRACKED)" {
		t.Fatalf("details=%v", finding.Details)
	}
}

// `wx doctor` の検査一覧に並び、store を読めないときは未検査として扱われる。
func TestUnsavedSubmoduleCheckIsStoreDependent(t *testing.T) {
	t.Parallel()
	for _, check := range diag.StoreDependentChecks() {
		if check == diag.CheckUnsavedSubmodules {
			return
		}
	}
	t.Fatal("the unsaved submodule check is missing from the store-dependent checks")
}
