package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
)

type mutationDiagnosticFile struct {
	writes int
}

func (f *mutationDiagnosticFile) Write(data []byte) (int, error) {
	f.writes++
	if f.writes == 2 {
		return 0, errors.New("payload write failed")
	}
	return len(data), nil
}

func (*mutationDiagnosticFile) Sync() error  { return nil }
func (*mutationDiagnosticFile) Close() error { return nil }

func TestPrepareDiagnosticWriterRecordsPayloadWriteError(t *testing.T) {
	t.Parallel()
	diagnostic := &prepareDiagnostic{file: &mutationDiagnosticFile{}, truncated: map[string]bool{}}
	data := []byte("payload")
	written, err := (prepareDiagnosticWriter{diagnostic: diagnostic, stream: "stdout"}).Write(data)
	if err != nil || written != len(data) {
		t.Fatalf("Write()=(%d,%v), want len=%d and nil error", written, err, len(data))
	}
	if diagnostic.writeError == nil {
		t.Fatal("payload write error was not retained")
	}
}

func TestConfigurePrepareProcessGroupFallsBackWhenGroupKillFails(t *testing.T) {
	t.Parallel()
	cmd := &exec.Cmd{Process: &os.Process{Pid: 1 << 30}}
	configurePrepareProcessGroup(cmd)
	if err := cmd.Cancel(); err == nil {
		t.Fatal("cancel reported success for a process group and process that do not exist")
	}
}

func TestDropUnmaterializedSubmodulesBoundsDecisionIndex(t *testing.T) {
	t.Parallel()
	target := t.TempDir()
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(target)}
	p := &Preparer{}
	module := materializedSubmodule{module: submodule{name: "child", path: "child"}}
	for _, test := range []struct {
		name          string
		index         int
		decisionCount int
		wantDecision  bool
	}{
		{name: "in range", index: 0, decisionCount: 1, wantDecision: true},
		{name: "negative", index: -1, decisionCount: 1},
		{name: "equal length", index: 1, decisionCount: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			decisions := make([]submoduleProbe, test.decisionCount)
			module.index = test.index
			stats := &submoduleStats{}
			var kept []materializedSubmodule
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("decision index panicked: %v", recovered)
					}
				}()
				kept = p.dropUnmaterializedSubmodules(repo, target, []materializedSubmodule{module}, decisions, stats)
			}()
			if len(kept) != 0 {
				t.Fatalf("unmaterialized module was retained: %+v", kept)
			}
			if test.wantDecision && (decisions[0].eligible || decisions[0].skipReason != SubmoduleReasonNotMaterialized) {
				t.Fatalf("in-range decision=%+v", decisions[0])
			}
		})
	}
}

func TestCompactSubmoduleWorktreePropagatesWorkspaceResolutionError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	p := &Preparer{Config: config.Defaults()}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: filepath.Join("..", "outside")}
	err := p.compactSubmoduleWorktree(context.Background(), repo, root, "oid", "slot", preparePhaseCreate, "", nil, false)
	if err == nil || !strings.Contains(err.Error(), "resolve workspace root") {
		t.Fatalf("workspace resolution error=%v", err)
	}
}

func submoduleOverrideFingerprintConfig(root string) config.Config {
	enabled, disabled := true, false
	return config.Config{Workspaces: map[string]config.Workspace{
		root: {
			RepositoryDefaults: config.RepositoryDefaults{Submodules: &enabled},
			Repositories:       map[string]config.Repository{".": {Submodules: &disabled}},
		},
	}}
}

func TestUpdateCompatibilityFingerprintUsesRepositorySubmoduleOverride(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "."}
	cfg := submoduleOverrideFingerprintConfig(root)
	withOverride, err := UpdateCompatibilityFingerprintWithGit(context.Background(), nil, 1, repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	delete(cfg.Workspaces[root].Repositories, ".")
	inherited, err := UpdateCompatibilityFingerprintWithGit(context.Background(), nil, 1, repo, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if withOverride == inherited {
		t.Fatal("repository submodule override did not change compatibility fingerprint")
	}
}

func TestFingerprintWorkspaceRootUsesRepositorySubmoduleOverride(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	repo := discovery.Repository{MainPath: domain.CanonicalPath(root), RelativePath: "."}
	var hash recordingMutationHash
	if err := fingerprintWorkspaceRoot(&hash, repo, submoduleOverrideFingerprintConfig(root)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hash.String(), "submodules=false") || strings.Contains(hash.String(), "submodules=true") {
		t.Fatalf("fingerprint ignored repository override: %q", hash.String())
	}
}

func TestPhaseNeedsTrackedStatusRefreshOnlyForCreate(t *testing.T) {
	t.Parallel()
	if !phaseNeedsTrackedStatusRefresh(preparePhaseCreate) {
		t.Fatal("create phase skipped tracked status refresh")
	}
	for _, phase := range []preparePhase{preparePhaseRestore, preparePhaseUpdate, "unknown"} {
		if phaseNeedsTrackedStatusRefresh(phase) {
			t.Fatalf("phase %q unexpectedly refreshed tracked status", phase)
		}
	}
}

func TestCloseIdentityDirectoryAcceptsMissingDescriptor(t *testing.T) {
	t.Parallel()
	closeIdentityDirectory(nil)
}

func TestCloseIdentityDirectoryClosesDescriptor(t *testing.T) {
	t.Parallel()
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })

	closeIdentityDirectory(directory)
	if _, err := directory.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Stat() error=%v, want %v", err, os.ErrClosed)
	}
}

func TestExistingTargetStateRejectsMarkerOutsideRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	p := &Preparer{}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(root)}
	outside := filepath.Join(t.TempDir(), "target")
	_, err = p.existingTargetState(context.Background(), repo, outside, "oid", "slot", preparePhaseCreate, root, owner, "target")
	if err == nil || !strings.Contains(err.Error(), "outside ownership root") {
		t.Fatalf("marker outside root was accepted: %v", err)
	}
}

func TestExistingTargetStatePropagatesMarkerInspectionError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	nonDirectory := filepath.Join(root, "marker-parent")
	if err := os.WriteFile(nonDirectory, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	p := &Preparer{}
	repo := discovery.Repository{ID: "repo", MainPath: domain.CanonicalPath(root)}
	_, err = p.existingTargetState(context.Background(), repo, filepath.Join(nonDirectory, "child"), "oid", "slot", preparePhaseCreate, root, owner, "target")
	if err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker inspection error was ignored: %v", err)
	}
}
