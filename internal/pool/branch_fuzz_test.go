package pool

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

func FuzzBranchSpecifications(f *testing.F) {
	for _, seed := range []string{"main", "api=feature", "services/api=release/v1", "=main", "api=", "api=one,api=two", "a=b=c"} {
		f.Add(seed)
	}
	repositories := []discovery.Repository{
		{ID: "services-api", RelativePath: "services/api", MainPath: domain.CanonicalPath("/repositories/services/api")},
		{ID: "legacy-api", RelativePath: "legacy/api", MainPath: domain.CanonicalPath("/repositories/legacy/api")},
		{ID: "web", RelativePath: "web", MainPath: domain.CanonicalPath("/repositories/web")},
	}
	f.Fuzz(func(t *testing.T, input string) {
		if len(input) > 4096 {
			t.Skip()
		}
		for _, specification := range strings.Split(input, ",") {
			selector, _, qualified := strings.Cut(specification, "=")
			if !qualified {
				continue
			}
			matches := matchRepositories(repositories, selector)
			for _, match := range matches {
				if filepath.Clean(match.RelativePath) != filepath.Clean(selector) && filepath.Base(match.RelativePath) != selector && filepath.Base(string(match.MainPath)) != selector {
					t.Fatalf("selector %q produced unrelated repository %+v", selector, match)
				}
			}
		}
	})
}
