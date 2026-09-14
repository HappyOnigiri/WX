package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSubmoduleArgumentBatchesPreserveOrderAndSplitLargeInputs(t *testing.T) {
	t.Parallel()
	items := []materializedSubmodule{
		{module: submodule{name: strings.Repeat("a", 40_000), path: "sub/a"}, source: "/source/a"},
		{module: submodule{name: strings.Repeat("b", 40_000), path: "sub/b"}, source: "/source/b"},
	}
	batches := batchSubmoduleMaterialization(items, 4)
	if len(batches) != 2 {
		t.Fatalf("batches=%d, want two batches", len(batches))
	}
	if batches[0][0].module.name != items[0].module.name || batches[1][0].module.name != items[1].module.name {
		t.Fatalf("batch order=%q,%q", batches[0][0].module.name[:1], batches[1][0].module.name[:1])
	}
	for _, batch := range batches {
		if got := argvSize(submoduleUpdateArgs(batch, 4)); got > submoduleArgMaxBytes {
			t.Fatalf("batch argv bytes=%d, want <= %d", got, submoduleArgMaxBytes)
		}
	}
}

func TestSubmoduleWorkerCountStaysWithinBounds(t *testing.T) {
	t.Parallel()
	p := &Preparer{}
	workers := p.submoduleWorkers()
	if workers < 1 || workers > submoduleMaxWorkers {
		t.Fatalf("workers=%d", workers)
	}
	p.submoduleWorkerCount = 1
	if got := p.submoduleWorkers(); got != 1 {
		t.Fatalf("pinned workers=%d", got)
	}
}

func TestSubmoduleCompatibilityWrappers(t *testing.T) {
	t.Parallel()
	f := newSubmoduleFixture(t)
	f.preparer.Config.Worktree.Submodules = false
	if err := f.preparer.submodulePhase(context.Background(), f.repo, f.target, f.head, ""); err != nil {
		t.Fatalf("submodulePhase() error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.preparer.materializeSubmodules(ctx, f.repo, f.target, f.head, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("materializeSubmodules() error=%v, want cancellation", err)
	}
	module := submodule{name: submoduleName, path: submodulePath, oid: submoduleGitlink(t, f.repository, f.head)}
	upstream, eligible := f.preparer.submoduleUpstream(context.Background(), f.moduleDir(), module)
	if !eligible || upstream != f.child {
		t.Fatalf("submoduleUpstream()=(%q,%t), want (%q,true)", upstream, eligible, f.child)
	}
}
