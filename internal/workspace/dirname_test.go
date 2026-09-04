package workspace

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

// TestRepositoryDirNameResolutionOrder walks the documented resolution order
// one step at a time: a pinned dir_name wins outright, then the repository's
// own dir_source, then storage.repo_dir_source, and finally the main
// worktree's directory name.
func TestRepositoryDirNameResolutionOrder(t *testing.T) {
	repo := discovery.Repository{
		ID:         "repo-id",
		MainPath:   domain.CanonicalPath("/src/local-checkout"),
		RemoteName: "RemoteName",
	}
	override := func(value config.Repository) config.Config {
		cfg := config.Defaults()
		cfg.Repositories = map[string]config.Repository{string(repo.MainPath): value}
		return cfg
	}
	globalSource := func(source string) config.Config {
		cfg := config.Defaults()
		cfg.Storage.RepoDirSource = source
		return cfg
	}
	for _, test := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "pinned dir_name wins", cfg: override(config.Repository{DirName: "pinned", DirSource: config.RepoDirSourceDirectory}), want: "pinned"},
		{name: "repository dir_source directory", cfg: override(config.Repository{DirSource: config.RepoDirSourceDirectory}), want: "local-checkout"},
		{name: "repository dir_source remote", cfg: override(config.Repository{DirSource: config.RepoDirSourceRemote}), want: "RemoteName"},
		{name: "global remote is the default", cfg: config.Defaults(), want: "RemoteName"},
		{name: "global directory", cfg: globalSource(config.RepoDirSourceDirectory), want: "local-checkout"},
		{name: "unusable pinned name falls through", cfg: override(config.Repository{DirName: "bad/name"}), want: "RemoteName"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := RepositoryDirName(repo, test.cfg); got != test.want {
				t.Fatalf("directory name=%q want=%q", got, test.want)
			}
		})
	}
}

// TestRepositoryDirNameFallsBackWhenSourcesAreUnusable covers the steps that
// exist so the name can never fail: a repository with no origin falls back to
// its directory name, and a directory name wx cannot use falls back to the
// repository ID, which is hex and therefore always a legal component.
func TestRepositoryDirNameFallsBackWhenSourcesAreUnusable(t *testing.T) {
	noRemote := discovery.Repository{ID: "repo-id", MainPath: domain.CanonicalPath("/src/checkout")}
	if got := RepositoryDirName(noRemote, config.Defaults()); got != "checkout" {
		t.Fatalf("missing remote fallback=%q", got)
	}
	reserved := discovery.Repository{ID: "repo-id", MainPath: domain.CanonicalPath("/src/_reserved")}
	if got := RepositoryDirName(reserved, config.Defaults()); got != "repo-id" {
		t.Fatalf("reserved directory name fallback=%q", got)
	}
	// A remote name wx cannot use must not shadow a usable directory name.
	unusableRemote := discovery.Repository{ID: "repo-id", MainPath: domain.CanonicalPath("/src/checkout"), RemoteName: "_reserved"}
	if got := RepositoryDirName(unusableRemote, config.Defaults()); got != "checkout" {
		t.Fatalf("unusable remote fallback=%q", got)
	}
}

func TestSanitizeDirNameRejectsUnusableComponents(t *testing.T) {
	for _, value := range []string{
		"", ".", "..", "a/b", `a\b`, "_unbound", "with\x00null", "with\tcontrol",
		// The ownership marker shares the slot directory with the repository
		// directories, so a repository may not claim a marker's name.
		OwnershipMarkerName("deadbeef"), ownershipMarkerPrefix, ownershipMarkerPrefix + "anything",
	} {
		if got, ok := sanitizeDirName(value); ok {
			t.Fatalf("sanitizeDirName(%q)=%q was accepted", value, got)
		}
	}
	if got, ok := sanitizeDirName("Repo.Name-1"); !ok || got != "Repo.Name-1" {
		t.Fatalf("sanitizeDirName kept=%q ok=%v", got, ok)
	}
	long := strings.Repeat("n", maxRepositoryDirNameLength+10)
	got, ok := sanitizeDirName(long)
	if !ok || len(got) != maxRepositoryDirNameLength {
		t.Fatalf("truncated name=%q len=%d ok=%v", got, len(got), ok)
	}
	// Truncation must not be able to produce a value wx would refuse.
	if _, ok := sanitizeDirName(got); !ok {
		t.Fatalf("truncated name %q is not itself acceptable", got)
	}
	// A multi-byte name over the byte budget has to be cut on a rune
	// boundary: the checks after truncation do not inspect UTF-8 validity, so
	// a split rune would reach slot_repositories.dir_name and the directory
	// name on disk as an invalid byte sequence.
	multibyte := strings.Repeat("あ", maxRepositoryDirNameLength)
	wide, ok := sanitizeDirName(multibyte)
	if !ok || len(wide) > maxRepositoryDirNameLength {
		t.Fatalf("truncated multibyte name=%q len=%d ok=%v", wide, len(wide), ok)
	}
	if !utf8.ValidString(wide) {
		t.Fatalf("truncated multibyte name %q is not valid UTF-8", wide)
	}
	if wide != strings.Repeat("あ", len(wide)/len("あ")) {
		t.Fatalf("truncated multibyte name %q lost or split a rune", wide)
	}
	if _, ok := sanitizeDirName(wide); !ok {
		t.Fatalf("truncated multibyte name %q is not itself acceptable", wide)
	}
}

// TestUniqueDirNameResolvesCaseInsensitiveCollisions pins the APFS-driven
// rule: two names that differ only in case are one directory on disk, so the
// second one must be suffixed even though the strings differ.
func TestUniqueDirNameResolvesCaseInsensitiveCollisions(t *testing.T) {
	taken := map[string]bool{}
	if got := UniqueDirName("Repo", taken); got != "Repo" {
		t.Fatalf("first name=%q", got)
	}
	if got := UniqueDirName("repo", taken); got != "repo-2" {
		t.Fatalf("case-insensitive collision name=%q", got)
	}
	if got := UniqueDirName("REPO", taken); got != "REPO-3" {
		t.Fatalf("second collision name=%q", got)
	}
	if got := UniqueDirName("other", taken); got != "other" {
		t.Fatalf("unrelated name=%q", got)
	}
	// A pre-taken suffix must be skipped rather than reused.
	prepared := map[string]bool{"repo": true, "repo-2": true}
	if got := UniqueDirName("repo", prepared); got != "repo-3" {
		t.Fatalf("pre-taken suffix name=%q", got)
	}
}

func TestOwnershipMarkerNameIsDerivedFromRepositoryID(t *testing.T) {
	if got := OwnershipMarkerName(testRepositoryID); got != ownershipMarkerPrefix+testRepositoryID {
		t.Fatalf("exported marker name=%q", got)
	}
	got, err := ownershipMarkerName(testRepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if got != OwnershipMarkerName(testRepositoryID) {
		t.Fatalf("internal marker name=%q does not match the exported spelling", got)
	}
	for _, value := range []string{"", ".", "..", "bad/repo", `bad\repo`} {
		if _, err := ownershipMarkerName(value); err == nil {
			t.Fatalf("marker name accepted repository id %q", value)
		}
	}
}
