package scanner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestMain は一時ディレクトリを symlink 解決済みの物理パスへ揃える。
// macOS の /var などを symlink のまま使うと path 検査と衝突する。
func TestMain(m *testing.M) {
	tmpDir, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "resolve temporary directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", tmpDir); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "set TMPDIR: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
