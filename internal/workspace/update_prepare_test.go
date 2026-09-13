package workspace

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/state"
)

// prepare.inputs は完全一致・ディレクトリ祖先・segment 単位の glob を扱い、別階層へ glob を広げない。
func TestMatchesPrepareInputSupportsExactDirectoriesAndSegments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		pattern string
		path    string
		want    bool
	}{
		{name: "exact", pattern: "config/db.yml", path: "config/db.yml", want: true},
		{name: "directory", pattern: "config", path: "config/db.yml", want: true},
		{name: "glob", pattern: "config/*.yml", path: "config/db.yml", want: true},
		{name: "glob does not cross directory", pattern: "config/*.yml", path: "config/db/db.yml", want: false},
		{name: "nonmatch", pattern: "scripts", path: "config/db.yml", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := matchesPrepareInput([]string{test.pattern}, test.path)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("pattern=%q path=%q got=%t want=%t", test.pattern, test.path, got, test.want)
			}
		})
	}
	if _, err := matchesPrepareInput([]string{"["}, "config/db.yml"); err == nil {
		t.Fatal("malformed pattern was accepted")
	}
}

// 配置の追加・削除・source 変更は tracked diff が空でも prepare.inputs の更新対象になる。
func TestChangedPlacementPathsIncludesAddedRemovedAndSourceChanges(t *testing.T) {
	t.Parallel()
	previous := []state.Placement{
		{RepositoryID: "repo", RelativePath: "removed", Kind: "copy", SourcePath: "/tmp/removed"},
		{RepositoryID: "repo", RelativePath: "changed", Kind: "copy", SourcePath: "/tmp/old"},
		{RepositoryID: "repo", RelativePath: "content", Kind: "copy", SourcePath: "/tmp/content", ContentSHA256: "old"},
		{RepositoryID: "repo", RelativePath: "same", Kind: "link", SourcePath: "/tmp/same"},
	}
	desired := []state.Placement{
		{RepositoryID: "repo", RelativePath: "added", Kind: "copy", SourcePath: "/tmp/added"},
		{RepositoryID: "repo", RelativePath: "changed", Kind: "copy", SourcePath: "/tmp/new"},
		{RepositoryID: "repo", RelativePath: "content", Kind: "copy", SourcePath: "/tmp/content", ContentSHA256: "new"},
		{RepositoryID: "repo", RelativePath: "same", Kind: "link", SourcePath: "/tmp/same"},
	}
	if got, want := changedPlacementPaths(previous, desired), []string{"added", "changed", "content", "removed"}; !slices.Equal(got, want) {
		t.Fatalf("changed paths=%v, want %v", got, want)
	}
}

// prepare.inputs 未設定時は Git diff を実行せず、既存の standby 更新コストを変えない。
func TestPrepareInputChangesSkipsGitWhenInputsUnset(t *testing.T) {
	t.Parallel()
	p := &Preparer{Config: config.Defaults()}
	repo := discovery.Repository{MainPath: domain.CanonicalPath(t.TempDir())}
	got, err := p.prepareInputChanges(context.Background(), repo, "old", "new", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("changed inputs=%v, want nil", got)
	}
}

// testlint:allow-serial -- cowFixture が隔離 repository の構築中に HOME を変更する。
func TestPrepareInputChangesReadsTrackedDiff(t *testing.T) {
	p, repo, oldOID, _ := cowFixture(t)
	main := string(repo.MainPath)
	p.Config.Repositories = map[string]config.Repository{
		main: {Prepare: config.Prepare{Inputs: []string{"config"}}},
	}
	writeTestFile(t, filepath.Join(main, "config", "db.yml"), "v2\n")
	cowGit(t, main, "add", "config/db.yml")
	cowGit(t, main, "commit", "-m", "add config")
	newOID := cowGit(t, main, "rev-parse", "HEAD")
	got, err := p.prepareInputChanges(context.Background(), repo, oldOID, newOID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"config/db.yml"}; !slices.Equal(got, want) {
		t.Fatalf("changed inputs=%v, want %v", got, want)
	}
}
