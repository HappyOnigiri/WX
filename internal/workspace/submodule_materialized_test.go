package workspace

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// update policy が子を省略した場合、origin 復元へ進まず source repository の origin を保つ。
// 空の子 path への `git -C` は親 worktree を解決するため、復元先が親の remote になってしまう。
func TestPrepareKeepsParentOriginWhenSubmoduleUpdateIsNone(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	parentUpstream := filepath.Join(filepath.Dir(f.repository), "parent-upstream.git")
	gitCommand(t, f.repository, "remote", "add", "origin", parentUpstream)
	gitCommand(t, f.repository, "config", "submodule."+submoduleName+".update", "none")
	outcomes := &SubmoduleOutcomes{}
	f.preparer.SubmoduleOutcomes = outcomes
	if err := f.preparer.Prepare(context.Background(), f.repo, f.target, f.head, "slot"); err != nil {
		t.Fatal(err)
	}
	if got := gitOutput(t, f.repository, "config", "--get", "remote.origin.url"); got != parentUpstream {
		t.Fatalf("parent origin=%q, want the unchanged %q", got, parentUpstream)
	}
	assertEmptyGitlinkDirectory(t, f.submoduleTarget())
	if !strings.Contains(f.logged.String(), "not materialized by the repository update policy") {
		t.Fatalf("logged=%q, want a skip warning for the unmaterialized submodule", f.logged.String())
	}
	_, results := outcomes.Snapshot()
	if len(results) != 1 || results[0].Action != SubmoduleActionSkipped || results[0].Reason != SubmoduleReasonNotMaterialized {
		t.Fatalf("outcomes=%+v, want one skipped %q result", results, SubmoduleReasonNotMaterialized)
	}
}
