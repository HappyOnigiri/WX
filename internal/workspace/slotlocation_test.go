package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/state"
)

// TestWorktreeDirNameAcceptsOnlyDirectSlotChildren pins the layout invariant
// the ownership request depends on: a repository worktree is exactly one path
// component below its slot directory, so anything else has no dir_name to
// compare against and must fail closed.
func TestWorktreeDirNameAcceptsOnlyDirectSlotChildren(t *testing.T) {
	worktreeRoot := string(filepath.Separator) + "wx"
	slotPath := filepath.Join(worktreeRoot, testSlotRelPath)
	preparer := &Preparer{SlotPath: slotPath}
	got, err := preparer.WorktreeDirName(filepath.Join(slotPath, testRepositoryID))
	if err != nil || got != testRepositoryID {
		t.Fatalf("direct child dir name=%q err=%v", got, err)
	}
	for _, test := range []struct {
		name   string
		target string
	}{
		{name: "slot directory itself", target: slotPath},
		{name: "nested below the repository", target: filepath.Join(slotPath, testRepositoryID, "nested")},
		{name: "sibling slot", target: filepath.Join(worktreeRoot, testWorkspaceID, "slot02", testRepositoryID)},
		{name: "outside the root", target: "/elsewhere/repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := preparer.WorktreeDirName(test.target); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("target %s dir name=%q err=%v", test.target, got, err)
			}
		})
	}
	// Without a slot path there is nothing to measure the worktree against.
	if _, err := (&Preparer{}).WorktreeDirName(filepath.Join(slotPath, testRepositoryID)); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing slot path error=%v", err)
	}
}

// TestValidateStateOwnershipWithIdentityFailsClosedWithoutAnIdentity covers
// the distinction between the two state-ownership helpers: the identity-bearing
// form exists so a caller that holds a descriptor cannot pass by omitting the
// identity, which is what would let a pathname-only proof back in.
func TestValidateStateOwnershipWithIdentityFailsClosedWithoutAnIdentity(t *testing.T) {
	ctx := context.Background()
	slotPath := filepath.Join(string(filepath.Separator)+"wx", testSlotRelPath)
	target := filepath.Join(slotPath, testRepositoryID)
	repo := discovery.Repository{ID: testRepositoryID, CommonDir: "/src/repository/.git"}
	recorder := &requestRecordingValidator{}
	preparer := &Preparer{Ownership: recorder, SlotPath: slotPath, RootID: testRootID, SlotRelPath: testSlotRelPath}

	if err := preparer.validateStateOwnershipWithIdentity(ctx, repo, target, "slot", "", nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing identity error=%v", err)
	}
	if recorder.calls != 0 {
		t.Fatal("a request without an identity reached the validator")
	}
	if err := preparer.validateStateOwnershipWithIdentity(ctx, repo, target, "", "identity", nil, nil); err != nil {
		t.Fatalf("empty slot id should be a no-op: %v", err)
	}
	if err := preparer.validateStateOwnershipWithIdentity(ctx, repo, target, "slot", "dev:ino", []string{"READY"}, []string{"READY"}); err != nil {
		t.Fatal(err)
	}
	if recorder.last.RootID != testRootID || recorder.last.SlotRelPath != testSlotRelPath ||
		recorder.last.DirName != testRepositoryID || recorder.last.DirIdentity != "dev:ino" {
		t.Fatalf("request does not describe the recorded location: %+v", recorder.last)
	}
	// The identity-free form is what prepare uses before the worktree
	// exists; it must send no identity so the validator does not fail closed
	// on a directory that cannot have one yet.
	if err := preparer.validateStateOwnership(ctx, repo, target, "slot", []string{"PREPARING"}, []string{"PREPARING"}); err != nil {
		t.Fatal(err)
	}
	if recorder.last.DirIdentity != "" {
		t.Fatalf("pre-creation request carried an identity: %+v", recorder.last)
	}
	if err := (&Preparer{SlotPath: slotPath}).validateStateOwnershipWithIdentity(ctx, repo, target, "slot", "dev:ino", nil, nil); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing validator error=%v", err)
	}
}

type requestRecordingValidator struct {
	calls int
	last  state.WorktreeOwnershipRequest
}

func (v *requestRecordingValidator) ValidateWorktreeOwnership(_ context.Context, request state.WorktreeOwnershipRequest) (state.WorktreeOwnership, error) {
	v.calls++
	v.last = request
	return state.WorktreeOwnership{}, nil
}
