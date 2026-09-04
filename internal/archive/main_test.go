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

// testRootID stands in for a roots.id row. Slot and workspace-snapshot
// locations are recorded as a root generation plus a root-relative path, so
// every fixture that builds one has to name a generation.
const testRootID = "rt0001"

// workspaceSnapshotRelPath is the OS-separator form of a session's recorded
// workspace-snapshot location, which is what the deterministic-location
// checks compare against.
func workspaceSnapshotRelPath(sessionID string) string {
	return filepath.FromSlash(workspaceSnapshotRelativePath(sessionID))
}

// recoveryNamespace is the top-level entry under a wx root that holds
// workspace recovery archives. Tests that need to block or collide with it
// derive the name from the production constant rather than repeating it.
var recoveryNamespace = strings.Split(workspaceSnapshotDirectory, "/")[0]
