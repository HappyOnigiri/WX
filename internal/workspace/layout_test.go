package workspace

import "path/filepath"

// この package の fixture は本番 layout `<worktree_root>/<workspace-id>/<slot-id>/<RepoName>` を再現し、daemon が組み立てる path component を同じく検証する。
// ID は domain.NewShortID が生成する6文字 lowercase base36 形式を使う。
const (
	testRootID       = "rt0001"
	testWorkspaceID  = "ws0001"
	testSlotID       = "slot01"
	testRepositoryID = "repository"
)

// testSlotRelPath は worktree root に対する slot directory の位置であり、SQLite が slots.rel_path に記録する値である。
var testSlotRelPath = filepath.Join(testWorkspaceID, testSlotID)

// markerFor はこの package の既定 root generation と repository の marker identity を作る。
// root/repository を意図的に変える case は MarkerIdentity を inline で作る。
func markerFor(slotID string) MarkerIdentity {
	return MarkerIdentity{SlotID: slotID, RootID: testRootID, RepositoryID: testRepositoryID}
}
