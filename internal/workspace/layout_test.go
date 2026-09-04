package workspace

import "path/filepath"

// The fixtures in this package mirror the production layout,
// <worktree_root>/<workspace-id>/<slot-id>/<RepoName>, so the tests exercise
// the same path components the daemon builds. The IDs use the
// six-character lowercase base36 shape domain.NewShortID produces.
const (
	testRootID       = "rt0001"
	testWorkspaceID  = "ws0001"
	testSlotID       = "slot01"
	testRepositoryID = "repository"
)

// testSlotRelPath is the slot directory's location relative to the worktree
// root, which is what SQLite records in slots.rel_path.
var testSlotRelPath = filepath.Join(testWorkspaceID, testSlotID)

// markerFor builds the marker identity for this package's default root
// generation and repository. Cases that deliberately vary the root or
// repository build a MarkerIdentity inline instead.
func markerFor(slotID string) MarkerIdentity {
	return MarkerIdentity{SlotID: slotID, RootID: testRootID, RepositoryID: testRepositoryID}
}
