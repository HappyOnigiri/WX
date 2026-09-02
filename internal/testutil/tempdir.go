package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// RunWithPhysicalTempDir makes testing.TempDir return canonical paths on
// systems such as macOS where TMPDIR is commonly reached through /var while
// filesystem APIs report the same directory through /private/var.
func RunWithPhysicalTempDir(m *testing.M) int {
	temporaryRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve physical test temp directory: %v\n", err)
		return 1
	}
	if err := os.Setenv("TMPDIR", temporaryRoot); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set physical test temp directory: %v\n", err)
		return 1
	}
	return m.Run()
}
