package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPrepareStagedFollowsSparseCheckoutOfSource は、補充経路の展開が worktree の sparse 条件に従うことを確かめる。
// この経路の read-tree は条件を適用しないため、以前は範囲外まで実体化して通常の `git worktree add` と結果が食い違っていた。
func TestPrepareStagedFollowsSparseCheckoutOfSource(t *testing.T) {
	t.Parallel()
	for _, mode := range []struct {
		name    string
		pattern []string
	}{
		{name: "cone", pattern: []string{"--cone", "inside"}},
		{name: "non-cone", pattern: []string{"--no-cone", "/inside/"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			source, repo, preparer, _, target := prepareEdgesFixture(t)
			for path, content := range map[string]string{"inside/kept": "kept\n", "outside/dropped": "dropped\n"} {
				path = filepath.Join(source, path)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			gitCommand(t, source, "add", ".")
			gitCommand(t, source, "commit", "-m", "sparse fixtures")
			oid := gitOutput(t, source, "rev-parse", "HEAD")
			gitCommand(t, source, append([]string{"sparse-checkout", "set"}, mode.pattern...)...)
			if _, err := preparer.PrepareStaged(context.Background(), "slot", []Preparation{{Repository: repo, Target: target, OID: oid}}, nil, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			// 通常の `git worktree add` を基準にする。wx 独自の条件解釈を持たない以上、結果は一致しなければならない。
			reference := filepath.Join(t.TempDir(), "reference")
			gitCommand(t, source, "worktree", "add", "--detach", reference, oid)
			t.Cleanup(func() { gitCommand(t, source, "worktree", "remove", "--force", reference) })
			if prepared, want := checkedOutPaths(t, target), checkedOutPaths(t, reference); !slices.Equal(prepared, want) {
				t.Fatalf("staged preparation checked out %v, git worktree add checked out %v", prepared, want)
			}
			if listing := gitOutput(t, target, "ls-files", "-v", "outside/dropped"); listing != "S outside/dropped" {
				t.Fatalf("path outside the sparse cone is not marked skip-worktree: %q", listing)
			}
		})
	}
}

// checkedOutPaths は worktree に実体がある tracked file の相対 path を並べ替えて返す。
// `.git` は worktree の指定方法で中身が変わるため数えない。
func checkedOutPaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if relative == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && relative != "." {
			paths = append(paths, relative)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}
