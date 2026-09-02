package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func TestOwnershipMarkerLifecycleAndMalformedProofs(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "slots", "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()

	if err := EnsureOwnershipMarker(root, target, "", common); err == nil {
		t.Fatal("empty slot marker was accepted")
	}
	if err := EnsureOwnershipMarker(root, target, "bad/slot", common); err == nil {
		t.Fatal("path-containing slot marker was accepted")
	}
	if err := EnsureOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatalf("idempotent marker creation: %v", err)
	}
	if err := EnsureOwnershipMarker(root, target, "other", common); err == nil {
		t.Fatal("mismatched existing marker was accepted for another slot")
	}
	if err := EnsureOwnershipMarker(root, target, "slot", t.TempDir()); err == nil {
		t.Fatal("mismatched existing marker was accepted for another common directory")
	}
	if err := EnsureOwnershipMarker(root, target, "slot", filepath.Join(root, "missing-common")); err == nil {
		t.Fatal("marker creation accepted a missing Git common directory")
	}
	outsideTarget := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outsideTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureOwnershipMarker(root, outsideTarget, "slot", common); err == nil {
		t.Fatal("marker creation accepted a target outside the ownership root")
	}
	if err := ValidateOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatalf("valid marker rejected: %v", err)
	}
	if err := ValidateOwnershipMarker(root, target, "", common); err != nil {
		t.Fatalf("marker with unspecified slot rejected: %v", err)
	}
	if err := ValidateOwnershipMarker(root, target, "other", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("wrong slot error=%v", err)
	}
	if err := ValidateOwnershipMarker(root, target, "slot", t.TempDir()); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("wrong common directory error=%v", err)
	}

	markerPath := filepath.Join(filepath.Dir(target), ownershipMarkerNameForTarget(target))
	writeMarker := func(value string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(markerPath, []byte(value), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(markerPath, mode); err != nil {
			t.Fatal(err)
		}
	}
	valid := ownershipMarker{Version: 1, SlotID: "slot", Target: target, CommonDir: common}
	validJSON, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data string
		mode os.FileMode
	}{
		{name: "world readable", data: string(validJSON), mode: 0o644},
		{name: "invalid json", data: "{", mode: 0o600},
		{name: "unknown field", data: `{"version":1,"slot_id":"slot","target":"` + target + `","common_dir":"` + common + `","extra":true}`, mode: 0o600},
		{name: "trailing data", data: string(validJSON) + "\n{}", mode: 0o600},
		{name: "malformed trailing data", data: string(validJSON) + "{", mode: 0o600},
		{name: "incomplete", data: `{"version":1,"slot_id":"slot"}`, mode: 0o600},
		{name: "invalid slot", data: `{"version":1,"slot_id":"bad/slot","target":"` + target + `","common_dir":"` + common + `"}`, mode: 0o600},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeMarker(test.data, test.mode)
			if err := ValidateOwnershipMarker(root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
				t.Fatalf("malformed marker error=%v", err)
			}
			if err := os.Chmod(markerPath, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(markerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOwnershipMarker(root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("directory marker error=%v", err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOwnershipMarker(root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing marker error=%v", err)
	}
	if err := EnsureOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatalf("marker recreation: %v", err)
	}

	// A marker can be syntactically valid while binding a different physical
	// target; the proof must reject that identity mismatch.
	otherTarget := filepath.Join(root, "slots", "other", "root")
	if err := os.MkdirAll(otherTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	other := ownershipMarker{Version: 1, SlotID: "slot", Target: otherTarget, CommonDir: common}
	otherJSON, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	writeMarker(string(otherJSON), 0o600)
	if err := ValidateOwnershipMarker(root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("target mismatch error=%v", err)
	}
	writeMarker(string(validJSON), 0o600)
	if _, err := ValidateRemovalOwnership(root, target, t.TempDir()); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("removal common mismatch error=%v", err)
	}
	writeMarker(string(otherJSON), 0o600)
	if _, err := ValidateRemovalOwnership(root, target, common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("removal target mismatch error=%v", err)
	}
	writeMarker(string(validJSON), 0o600)
	if _, err := ValidateRemovalOwnership(root, target, filepath.Join(root, "missing-common")); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing common directory removal error=%v", err)
	}
	if _, err := ValidateRemovalOwnership(root, outsideTarget, common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("outside target removal error=%v", err)
	}

	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if slot, err := ValidateRemovalOwnership(root, target, common); err != nil || slot != "slot" {
		t.Fatalf("missing-leaf removal proof slot=%q err=%v", slot, err)
	}
	if slot, err := ValidateRemovalOwnership(root, target, t.TempDir()); !errors.Is(err, state.ErrOwnership) || slot != "" {
		t.Fatalf("common mismatch slot=%q err=%v", slot, err)
	}
	if err := ValidateOwnershipMarker(root, target, "slot", common); !errors.Is(err, state.ErrOwnership) {
		t.Fatalf("missing target validation error=%v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newOwnershipMarker(filepath.Join(root, "file"), "slot", common, true); err == nil {
		t.Fatal("regular file accepted as target")
	}
	link := filepath.Join(root, "target-link")
	if err := os.Symlink(otherTarget, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newOwnershipMarker(link, "slot", common, true); err == nil {
		t.Fatal("target symlink accepted")
	}
	if _, err := newOwnershipMarker(target, "slot", filepath.Join(root, "missing-common"), true); err == nil {
		t.Fatal("missing common directory accepted")
	}
	if got := markerOwnershipFailure(state.ErrOwnership); got != state.ErrOwnership {
		t.Fatalf("ownership sentinel was rewrapped: %v", got)
	}
	if got := markerOwnershipFailure(nil); got != nil {
		t.Fatalf("nil ownership error became non-nil: %v", got)
	}
	if err := removeOwnershipMarker(root, outsideTarget); err == nil {
		t.Fatal("marker removal accepted a target outside the ownership root")
	}
}

func TestPhysicalFilesystemRejectsNondirectoryAndInvalidTraversal(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPhysicalRoot(filepath.Join(file, "child")); err == nil {
		t.Fatal("physical root below a regular file was opened")
	}

	owner, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	var matches []string
	if err := walkSafeGlob(owner, "bad\x00path", []string{"*"}, 0, &matches); err == nil {
		t.Fatal("invalid glob traversal path was accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "entry"), []byte("entry"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches = nil
	if err := walkSafeGlob(owner, ".", []string{"["}, 0, &matches); err == nil {
		t.Fatal("invalid child glob was accepted")
	}
	if err := walkSafeGlob(owner, "missing", []string{"*"}, 0, &matches); err != nil {
		t.Fatalf("missing glob ancestor returned an error: %v", err)
	}

	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(root, "file", destination, "copy"); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(root, "file", destination, "copy"); err != nil {
		t.Fatalf("copy over an existing regular file: %v", err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(root, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(linkedRoot, "file", destination, "linked-source"); err == nil {
		t.Fatal("copy accepted a symlinked source root")
	}
	linkedDestination := filepath.Join(t.TempDir(), "linked-destination")
	if err := os.Symlink(destination, linkedDestination); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(root, "file", linkedDestination, "linked-destination"); err == nil {
		t.Fatal("copy accepted a symlinked destination root")
	}
	manifestDirectory := filepath.Join(root, "manifest-directory")
	if err := os.Mkdir(manifestDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalManifest(root, "manifest-directory"); err == nil {
		t.Fatal("directory was accepted as a physical manifest")
	}
	socketRoot := filepath.Join("/private/tmp", "wx-socket-root-"+domain.StableID(root))
	if err := os.Mkdir(socketRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	socketPath := filepath.Join(socketRoot, "s")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(socketRoot, "s", destination, "socket"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("socket source copy error=%v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	loopA := filepath.Join(root, "loop-a")
	loopB := filepath.Join(root, "loop-b")
	if err := os.Symlink(loopB, loopA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loopA, loopB); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalPathAllowMissing(loopA); err == nil {
		t.Fatal("symlink loop was treated as a missing leaf")
	}
}

func TestRegisteredWorktreeLockProofBoundaries(t *testing.T) {
	repository := t.TempDir()
	gitCommand(t, repository, "init", "-b", "main")
	gitCommand(t, repository, "config", "user.name", "test")
	gitCommand(t, repository, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repository, "tracked"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, repository, "add", ".")
	gitCommand(t, repository, "commit", "-m", "initial")
	target := filepath.Join(t.TempDir(), "worktree")
	gitCommand(t, repository, "worktree", "add", "--detach", target, gitOutput(t, repository, "rev-parse", "HEAD"))
	runner := &gitx.Runner{Timeout: 5 * time.Second}
	ctx := context.Background()
	if err := ValidateRegisteredWorktree(ctx, runner, repository, target, "", false); err != nil {
		t.Fatalf("unlocked worktree without lock requirement: %v", err)
	}
	gitCommand(t, repository, "worktree", "lock", "--reason", "wx:slot:READY", target)
	if err := ValidateRegisteredWorktree(ctx, runner, repository, target, "", true); err != nil {
		t.Fatalf("wx lock without slot requirement: %v", err)
	}
	if err := ValidateRegisteredWorktree(ctx, runner, repository, target, "slot", true); err != nil {
		t.Fatalf("matching wx lock rejected: %v", err)
	}
	if err := ValidateRegisteredWorktree(ctx, runner, repository, target, "other", true); err == nil {
		t.Fatal("foreign wx slot lock accepted")
	}
	gitCommand(t, repository, "worktree", "unlock", target)
	if err := ValidateRegisteredWorktree(ctx, runner, repository, target, "slot", true); err == nil {
		t.Fatal("unlocked worktree accepted when lock was required")
	}
	if err := ValidateRegisteredWorktree(ctx, runner, repository, filepath.Join(filepath.Dir(target), "missing"), "", false); err == nil {
		t.Fatal("unregistered worktree accepted")
	}
}

func TestOwnershipMarkerCanBeCreatedBeforeGitAddsTheWorktree(t *testing.T) {
	root := t.TempDir()
	slotRoot := filepath.Join(root, "slots", "slot")
	if err := os.MkdirAll(slotRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(slotRoot, "root")
	common := t.TempDir()
	if err := EnsureOwnershipMarker(root, target, "slot", common); err != nil {
		t.Fatalf("pre-creation marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(slotRoot, ownershipMarkerNameForTarget(target))); err != nil {
		t.Fatalf("marker was not created beside missing worktree: %v", err)
	}
	if _, err := ValidateRemovalOwnership(root, target, common); err != nil {
		t.Fatalf("pre-creation removal proof: %v", err)
	}
}

func TestOwnershipMarkerPathsAndPhysicalHelpers(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "slots", "slot", "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ownershipMarkerBase(root, target); got != filepath.Dir(target) {
		t.Fatalf("slot marker base=%q", got)
	}
	ordinary := filepath.Join(root, "ordinary")
	if err := os.Mkdir(ordinary, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := ownershipMarkerBase(root, ordinary); got != root {
		t.Fatalf("ordinary marker base=%q", got)
	}
	if _, _, err := openMarkerRoot(root, filepath.Join(t.TempDir(), "outside")); err == nil {
		t.Fatal("marker root opened outside ownership root")
	}
	if err := EnsureOwnershipMarker(root, ordinary, "ordinary", root); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarker(root, ordinary); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnershipMarker(root, ordinary); err != nil {
		t.Fatalf("idempotent marker removal: %v", err)
	}

	missingParent := filepath.Join(root, "new", "target")
	if err := validatePhysicalPathAllowMissingLeaf(missingParent); err == nil {
		t.Fatal("missing physical ancestor accepted")
	}
	if err := os.MkdirAll(filepath.Dir(missingParent), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validatePhysicalPathAllowMissingLeaf(missingParent); err != nil {
		t.Fatalf("missing leaf with existing parent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePhysicalPathAllowMissingLeaf(filepath.Join(root, "plain")); err == nil {
		t.Fatal("regular file passed missing-leaf validation")
	}
	if err := validatePhysicalPathAllowMissingLeaf(filepath.Join(root, "plain", "child")); err == nil {
		t.Fatal("non-directory ancestor passed missing-leaf validation")
	}

	validRoot := filepath.Join(root, "physical")
	if err := os.Mkdir(validRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenPhysicalRoot(validRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "root-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPhysicalRoot(file); err == nil {
		t.Fatal("regular file accepted as physical root")
	}
	if _, err := OpenPhysicalRoot(filepath.Join(root, "missing-root")); err == nil {
		t.Fatal("missing physical root accepted")
	}
	rootLink := filepath.Join(root, "root-link")
	if err := os.Symlink(validRoot, rootLink); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPhysicalRoot(rootLink); err == nil {
		t.Fatal("symlink physical root accepted")
	}

	if _, err := safeGlob(validRoot, "."); err == nil {
		t.Fatal("dot glob accepted")
	}
	if _, err := safeGlob(validRoot, filepath.Join(validRoot, "*")); err == nil {
		t.Fatal("absolute glob accepted")
	}
	if _, err := safeGlob(validRoot, "["); err == nil {
		t.Fatal("malformed glob accepted")
	}
	if err := os.WriteFile(filepath.Join(validRoot, "one.txt"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(validRoot, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validRoot, "nested", "two.txt"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches, err := safeGlob(validRoot, "**/*.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("safe glob returned no recursive matches")
	}
	if _, err := safeGlob(validRoot, "missing/*.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside"), filepath.Join(validRoot, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := safeGlob(validRoot, "linked/*"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink ancestor glob error=%v", err)
	}
	if _, err := safeGlob(validRoot, "linked"); err != nil {
		t.Fatalf("symlink leaf glob should not descend: %v", err)
	}

	if !ruleContains("a", filepath.Join("a", "b")) || ruleContains("a", "a") || ruleContains("a", "..") {
		t.Fatal("rule containment boundary failed")
	}
	if err := validateRuleConflicts([]string{"same"}, []string{"same"}); err == nil {
		t.Fatal("copy/link overlap accepted")
	}
	if err := validateRuleConflicts([]string{"../unsafe"}, nil); err == nil {
		t.Fatal("unsafe copy rule accepted")
	}
	if err := validateRuleConflicts(nil, []string{"parent", filepath.Join("parent", "child")}); err == nil {
		t.Fatal("nested link rules accepted")
	}
}

func TestPhysicalCopyAndManifestHelpersRejectUnsafeShapes(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "value"), []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(source, "nested", destination, "copied"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "copied", "value")); err != nil || string(data) != "value" {
		t.Fatalf("copied directory data=%q err=%v", data, err)
	}
	if err := copyPathFromRoots(source, "file", destination, "file-copy"); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(source, "missing", destination, "missing"); err == nil {
		t.Fatal("missing source copied")
	}
	if err := copyPathFromRoots(source, "../source", destination, "unsafe"); err == nil {
		t.Fatal("unsafe source path copied")
	}
	if err := copyPathFromRoots(source, "file", destination, "../unsafe"); err == nil {
		t.Fatal("unsafe destination path copied")
	}
	if err := os.Symlink(source, filepath.Join(root, "source-link")); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(filepath.Join(root, "source-link"), "file", destination, "link-source"); err == nil {
		t.Fatal("symlink source root copied")
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(destination, "dest-link")); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(source, "file", destination, "dest-link/child"); err == nil {
		t.Fatal("symlink destination ancestor copied")
	}
	if err := os.WriteFile(filepath.Join(destination, "collision"), []byte("collision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyPathFromRoots(source, "nested", destination, "collision"); err == nil {
		t.Fatal("directory replaced regular file")
	}

	physicalManifest := filepath.Join(source, "manifest")
	if data, err := readPhysicalManifest(source, "missing"); err != nil || data != nil {
		t.Fatalf("missing manifest data=%q err=%v", data, err)
	}
	if err := os.WriteFile(physicalManifest, []byte("one\n# comment\n\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	patterns, err := readPhysicalPatterns(source, "manifest")
	if err != nil || !reflect.DeepEqual(patterns, []string{"one", "two"}) {
		t.Fatalf("manifest patterns=%v err=%v", patterns, err)
	}
	if err := os.Mkdir(filepath.Join(source, "manifest-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalManifest(source, "manifest-dir"); err == nil {
		t.Fatal("manifest directory accepted")
	}
	if err := os.Symlink(physicalManifest, filepath.Join(source, "manifest-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readPhysicalManifest(source, "manifest-link"); err == nil {
		t.Fatal("manifest symlink accepted")
	}
	if _, err := readPhysicalManifest(source, "../manifest"); err == nil {
		t.Fatal("manifest outside root accepted")
	}

	owner, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if err := ensureRootDirectory(owner, "."); err != nil {
		t.Fatal(err)
	}
	if err := ensureRootDirectory(owner, filepath.Join("new", "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "not-dir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRootDirectory(owner, "not-dir/child"); err == nil {
		t.Fatal("file destination component accepted")
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(destination, "symlink-dir")); err != nil {
		t.Fatal(err)
	}
	if err := ensureRootDirectory(owner, "symlink-dir/child"); err == nil {
		t.Fatal("symlink destination component accepted")
	}
	created, err := owner.OpenFile("new/nested/value", os.O_CREATE|os.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeRecordAndCanonicalPathEdges(t *testing.T) {
	records := parseWorktreeRecords("noise\x00worktree /one\x00locked\x00worktree /two\x00locked reason\x00")
	if len(records) != 2 || records[0].Path != "/one" || !records[0].Locked || records[0].LockReason != "" || records[1].LockReason != "reason" {
		t.Fatalf("records=%+v", records)
	}
	if _, err := canonicalPathAllowMissing(filepath.Join(t.TempDir(), "a", "b")); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "alias")); err != nil {
		t.Fatal(err)
	}
	resolved, err := canonicalPathAllowMissing(filepath.Join(base, "alias", "missing"))
	if err != nil || !strings.HasPrefix(resolved, filepath.Join(base, "real")) {
		t.Fatalf("canonical missing through alias=%q err=%v", resolved, err)
	}
	if err := validatePhysicalPathAllowMissingLeaf(filepath.Join(base, "alias", "missing")); err == nil {
		t.Fatal("missing leaf below symlink ancestor accepted")
	}
	if err := domain.ValidatePhysicalPath(filepath.Join(base, "real"), false); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemAndMarkerHelpersPropagateClosedDescriptorErrors(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootPath, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	closedOwner, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := closedOwner.Close(); err != nil {
		t.Fatal(err)
	}

	var matches []string
	if err := walkSafeGlob(closedOwner, ".", nil, 0, &matches); err == nil {
		t.Fatal("closed root accepted an empty glob endpoint")
	}
	if err := walkSafeGlob(closedOwner, "file", []string{"*"}, 0, &matches); err == nil {
		t.Fatal("closed root accepted a glob ancestor")
	}
	if err := walkSafeGlob(closedOwner, ".", []string{"*"}, 0, &matches); err == nil {
		t.Fatal("closed root accepted a glob open")
	}
	if err := ensureRootDirectory(closedOwner, "nested"); err == nil {
		t.Fatal("closed root accepted destination directory creation")
	}
	if _, err := readOwnedDirectory(closedOwner, "."); err == nil {
		t.Fatal("closed root accepted directory read")
	}
	if _, err := readOwnershipMarker(closedOwner, "missing"); err == nil {
		t.Fatal("closed root accepted marker read")
	}
	if err := fingerprintRootPath(sha256.New(), closedOwner, "file", "file"); err == nil {
		t.Fatal("closed root accepted fingerprinting")
	}

	if err := os.Symlink(filepath.Join(rootPath, "file"), filepath.Join(rootPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := walkSafeGlob(owner, "link", []string{"*"}, 0, &matches); err == nil {
		t.Fatal("symlink glob ancestor was accepted")
	}
	if err := walkSafeGlob(owner, "file", []string{"*"}, 0, &matches); err != nil {
		t.Fatalf("regular-file glob ancestor returned an error: %v", err)
	}

	destinationPath := t.TempDir()
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyRootEntry(owner, "file", destination, "copy"); err == nil {
		t.Fatal("copy into a closed destination root succeeded")
	}
	if err := copyRootEntry(closedOwner, "file", owner, "copy"); err == nil {
		t.Fatal("copy from a closed source root succeeded")
	}
	if err := copyPathFromRoots(rootPath, filepath.Join("file", "child"), destinationPath, "bad-copy"); err == nil {
		t.Fatal("copy below a regular source component succeeded")
	}

	if markerOwnershipFailure(nil) != nil || markerOwnershipFailure(state.ErrOwnership) != state.ErrOwnership {
		t.Fatal("marker ownership sentinel handling changed")
	}
	if err := markerOwnershipFailure(errors.New("marker fault")); !errors.Is(err, state.ErrOwnership) || !strings.Contains(err.Error(), "marker fault") {
		t.Fatalf("marker ownership wrapping=%v", err)
	}
}
