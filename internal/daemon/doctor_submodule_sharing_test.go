package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
)

func newSubmoduleSharingDoctorFixture(t *testing.T) (*managerFixture, discovery.Workspace, string) {
	t.Helper()
	f := manualManagerFixture(t)
	repository := filepath.Join(f.Root, "repository")
	initGitRepoWithSubmodule(t, repository)
	w, err := (&discovery.Discoverer{Git: f.Manager.git, Config: f.Config}).Resolve(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	w = registerTestWorkspace(t, f.Store, w)
	source := filepath.Join(string(w.Repositories[0].CommonDir), "modules", daemonSubmoduleName)
	return f, w, source
}

func findSubmoduleSharingFinding(t *testing.T, findings []diag.Finding, severity diag.Severity, target string) diag.Finding {
	t.Helper()
	for _, finding := range findings {
		if finding.Check == diag.CheckSubmoduleSharing && finding.Severity == severity && finding.Target == target {
			return finding
		}
	}
	t.Fatalf("submodule sharing finding severity=%s target=%s not found: %+v", severity, target, findings)
	return diag.Finding{}
}

func TestSubmoduleSharingDoctorReportsShallowAndPromisorStates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, test := range []struct {
		name     string
		prepare  func(*managerFixture, discovery.Workspace, string) error
		severity diag.Severity
	}{
		{name: "shallow", prepare: func(_ *managerFixture, _ discovery.Workspace, source string) error {
			if err := os.WriteFile(filepath.Join(source, "shallow"), nil, 0o600); err != nil {
				return err
			}
			return nil
		}, severity: diag.SeverityInfo},
		{name: "promisor complete", prepare: func(_ *managerFixture, _ discovery.Workspace, source string) error {
			cmd := exec.Command("git", "--git-dir=.", "config", "remote.origin.promisor", "true")
			cmd.Dir = source
			return cmd.Run()
		}, severity: diag.SeverityInfo},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, w, source := newSubmoduleSharingDoctorFixture(t)
			if err := test.prepare(f, w, source); err != nil {
				t.Fatal(err)
			}
			finding := findSubmoduleSharingFinding(t, f.Manager.submoduleSharingFindings(ctx), test.severity, source)
			if finding.Cause == "" || finding.Action == "" {
				t.Fatalf("finding=%+v, want cause and action", finding)
			}
		})
	}
}

func TestSubmoduleSharingDoctorReportsMissingPromisorObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, w, source := newSubmoduleSharingDoctorFixture(t)
	child := filepath.Join(f.Root, "child")
	if err := os.WriteFile(filepath.Join(child, "tracked.txt"), []byte("ahead\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, child, "add", ".")
	gitRun(t, child, "commit", "-m", "child ahead")
	ahead := gitOutput(t, child, "rev-parse", "HEAD")
	repository := string(w.Repositories[0].MainPath)
	gitRun(t, repository, "update-index", "--cacheinfo", "160000,"+ahead+","+daemonSubmodulePath)
	gitRun(t, repository, "commit", "-m", "advance gitlink")
	gitRun(t, source, "--git-dir=.", "config", "remote.origin.promisor", "true")
	finding := findSubmoduleSharingFinding(t, f.Manager.submoduleSharingFindings(ctx), diag.SeverityProblem, source)
	if !strings.Contains(finding.Cause, ahead) {
		t.Fatalf("finding cause=%q, want requested object %s", finding.Cause, ahead)
	}
}
