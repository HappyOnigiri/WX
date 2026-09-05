package archive

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	temporaryRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve physical test temp directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", temporaryRoot); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set physical test temp directory: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// testRootID は roots.id 行を表す。slot と workspace snapshot の場所は root generation と
// root 相対パスで記録するため、これを組み立てる fixture は generation を必ず指定する。
const testRootID = "rt0001"

// workspaceSnapshotRelPath は session に記録した workspace snapshot の OS 区切り形式で、
// 決定的な場所の検査が比較する値である。
func workspaceSnapshotRelPath(sessionID string) string {
	return filepath.FromSlash(workspaceSnapshotRelativePath(sessionID))
}

// recoveryNamespace は wx root 直下で workspace recovery archive を保持する最上位項目である。
// これを塞いだり衝突させたりするテストは、本番定数から名前を導出して重複を避ける。
var recoveryNamespace = strings.Split(workspaceSnapshotDirectory, "/")[0]
