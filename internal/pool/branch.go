package pool

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

type Resolved struct {
	Repository        discovery.Repository
	RequestedRef, OID string
}

func ResolveBranches(ctx context.Context, git *gitx.Runner, w discovery.Workspace, specs []string) ([]Resolved, error) {
	global := ""
	qualified := map[string]string{}
	for _, s := range specs {
		if strings.Contains(s, "=") {
			parts := strings.SplitN(s, "=", 2)
			if parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("invalid branch specification %q", s)
			}
			matches := matchRepositories(w.Repositories, parts[0])
			if len(matches) == 0 {
				return nil, fmt.Errorf("branch selector %q matches no repository", parts[0])
			}
			if len(matches) > 1 {
				return nil, fmt.Errorf("branch selector %q is ambiguous", parts[0])
			}
			id := string(matches[0].ID)
			if _, ok := qualified[id]; ok {
				return nil, fmt.Errorf("repository %q has multiple branch specifications", parts[0])
			}
			qualified[id] = parts[1]
		} else {
			if global != "" {
				return nil, fmt.Errorf("multiple global branch specifications are ambiguous")
			}
			global = s
		}
	}
	globalMatches := map[string]bool{}
	if global != "" {
		matched := 0
		applicable := make([]discovery.Repository, 0, len(w.Repositories))
		for _, repo := range w.Repositories {
			if _, overridden := qualified[string(repo.ID)]; overridden {
				continue
			}
			applicable = append(applicable, repo)
			_, ok, err := gitx.ResolveRef(ctx, git, string(repo.MainPath), global)
			if err != nil {
				return nil, err
			}
			globalMatches[string(repo.ID)] = ok
			if ok {
				matched++
			}
		}
		if matched == 0 && len(applicable) == 1 {
			repo := applicable[0]
			return nil, fmt.Errorf("branch %q does not exist in repository %s; refusing to use default branch %q", global, repo.RelativePath, repo.DefaultBranch)
		}
		if matched == 0 && len(applicable) > 1 {
			paths := make([]string, 0, len(applicable))
			for _, repo := range applicable {
				paths = append(paths, repo.RelativePath)
			}
			return nil, fmt.Errorf("branch %q does not exist in any repository (%s); refusing to use default branches", global, strings.Join(paths, ", "))
		}
	}
	out := make([]Resolved, 0, len(w.Repositories))
	for _, repo := range w.Repositories {
		branch := repo.DefaultBranch
		if global != "" {
			if globalMatches[string(repo.ID)] {
				branch = global
			}
		}
		if q, ok := qualified[string(repo.ID)]; ok {
			branch = q
		}
		oid, ok, err := gitx.ResolveRef(ctx, git, string(repo.MainPath), branch)
		if err != nil {
			return nil, err
		}
		if !ok {
			if _, qualified := qualified[string(repo.ID)]; qualified {
				return nil, fmt.Errorf("branch %q does not exist in repository %s", branch, repo.RelativePath)
			}
			branch = repo.DefaultBranch
			oid, ok, err = gitx.ResolveRef(ctx, git, string(repo.MainPath), branch)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("default branch %q is missing in repository %s", branch, repo.RelativePath)
			}
		}
		out = append(out, Resolved{Repository: repo, RequestedRef: branch, OID: oid})
	}
	return out, nil
}

func matchRepositories(repos []discovery.Repository, selector string) []discovery.Repository {
	clean := filepath.Clean(selector)
	var exact []discovery.Repository
	for _, r := range repos {
		if filepath.Clean(r.RelativePath) == clean {
			exact = append(exact, r)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	var base []discovery.Repository
	for _, r := range repos {
		if filepath.Base(r.RelativePath) == selector || filepath.Base(string(r.MainPath)) == selector {
			base = append(base, r)
		}
	}
	return base
}
