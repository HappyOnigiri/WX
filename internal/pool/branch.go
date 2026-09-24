package pool

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

type Resolved struct {
	Repository        discovery.Repository
	RequestedRef, OID string
}

// FetchWarning は既定 branch の更新を採用できず、従来どおりローカル ref を
// 起点にした理由である。fetch は repository ごとに独立して扱うため、1 件の
// 警告で workspace 全体の解決を失敗させない。
type FetchWarning struct {
	Repository discovery.Repository
	Operation  string
	Err        error
}

func (w FetchWarning) Error() string {
	if w.Err == nil {
		return w.Operation
	}
	return fmt.Sprintf("%s: %v", w.Operation, w.Err)
}

// MissingDefaultBranchError は明示された repository の既定 branch が存在しないことを表す。
// 診断はこの失敗を「設定で既定 branch を指し直す」対処へ結び付けるため、
// 呼び出し側が errors.As で判定できるよう branch 名と workspace 相対 path を保つ。
type MissingDefaultBranchError struct {
	Branch, RepositoryRelativePath string
}

func (e *MissingDefaultBranchError) Error() string {
	return fmt.Sprintf("default branch %q is missing in repository %s", e.Branch, e.RepositoryRelativePath)
}

// UnresolvedDefaultBranchError は明示設定にも Git の実体にも貸出の起点が見つからなかったことを表す。
// MissingDefaultBranchError は明示された branch が消えた場合に限って使い、この error は
// origin/HEAD や default_branch の設定を促す診断へ結び付ける。
type UnresolvedDefaultBranchError struct {
	RepositoryRelativePath string
}

func (e *UnresolvedDefaultBranchError) Error() string {
	return fmt.Sprintf("default branch could not be resolved for repository %s; set a remote HEAD or configure default_branch", e.RepositoryRelativePath)
}

func ResolveBranches(ctx context.Context, git *gitx.Runner, w discovery.Workspace, specs []string) ([]Resolved, error) {
	return resolveBranches(ctx, git, w, specs, false, nil)
}

// ResolveBranchesWithFetch は branch 未指定のときだけ各 repository の origin 既定
// branch を common-directory lock 下で fetch し、local branch が remote の祖先で
// ある場合に限って remote OID を採用する。fetch の失敗は warning へ渡して local
// OID へ戻し、context のキャンセルや通常の ref 解決障害だけを error とする。
// commentlint:allow-long -- fetch と fallback の契約を公開関数の doc comment にまとめる
func ResolveBranchesWithFetch(ctx context.Context, git *gitx.Runner, w discovery.Workspace, specs []string, warn func(FetchWarning)) ([]Resolved, error) {
	return resolveBranches(ctx, git, w, specs, true, warn)
}

func resolveBranches(ctx context.Context, git *gitx.Runner, w discovery.Workspace, specs []string, fetchDefault bool, warn func(FetchWarning)) ([]Resolved, error) {
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
	// global の解決結果は下の repository ごとの解決で使い回し、同じ ref を2回引かない。
	globalOIDs := map[string]string{}
	if global != "" {
		matched := 0
		applicable := make([]discovery.Repository, 0, len(w.Repositories))
		for _, repo := range w.Repositories {
			if _, overridden := qualified[string(repo.ID)]; overridden {
				continue
			}
			applicable = append(applicable, repo)
			oid, ok, err := gitx.ResolveRef(ctx, git, string(repo.MainPath), global)
			if err != nil {
				return nil, err
			}
			globalMatches[string(repo.ID)] = ok
			if ok {
				globalOIDs[string(repo.ID)] = oid
			}
			if ok {
				matched++
			}
		}
		if matched == 0 && len(applicable) == 1 {
			repo := applicable[0]
			if repo.DefaultBranch == "" {
				return nil, &UnresolvedDefaultBranchError{RepositoryRelativePath: repo.RelativePath}
			}
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
		if branch == "" {
			return nil, &UnresolvedDefaultBranchError{RepositoryRelativePath: repo.RelativePath}
		}
		var (
			oid string
			ok  bool
			err error
		)
		if cached, found := globalOIDs[string(repo.ID)]; found && branch == global {
			oid, ok = cached, true
		} else if fetchDefault && len(specs) == 0 {
			oid, ok, err = resolveFetchedDefaultBranch(ctx, git, repo, branch, warn)
		} else {
			oid, ok, err = gitx.ResolveRef(ctx, git, string(repo.MainPath), branch)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			if fetchDefault && len(specs) == 0 {
				// fetch 経路で local ref も失われている場合、古い remote-tracking
				// ref を採用すると fetch 失敗の一部更新を隠してしまう。
				return nil, &MissingDefaultBranchError{Branch: branch, RepositoryRelativePath: repo.RelativePath}
			}
			if _, qualified := qualified[string(repo.ID)]; qualified {
				return nil, fmt.Errorf("branch %q does not exist in repository %s", branch, repo.RelativePath)
			}
			if repo.DefaultBranch == "" {
				return nil, &UnresolvedDefaultBranchError{RepositoryRelativePath: repo.RelativePath}
			}
			branch = repo.DefaultBranch
			oid, ok, err = gitx.ResolveRef(ctx, git, string(repo.MainPath), branch)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, &MissingDefaultBranchError{Branch: branch, RepositoryRelativePath: repo.RelativePath}
			}
		}
		out = append(out, Resolved{Repository: repo, RequestedRef: branch, OID: oid})
	}
	return out, nil
}

// resolveFetchedDefaultBranch は fetch を common-directory lock 下で行い、取得後に
// ref と祖先関係を判定する。local branch が無い repository では remote OID だけを
// 採用し、remote が後退・分岐したときは local OID を維持する。
func resolveFetchedDefaultBranch(ctx context.Context, git *gitx.Runner, repo discovery.Repository, branch string, warn func(FetchWarning)) (string, bool, error) {
	var fetched bool
	err := git.WithCommonDirLock(ctx, string(repo.CommonDir), func(lockCtx context.Context) error {
		fetchErr := fetchDefaultBranch(lockCtx, git, repo, branch)
		if fetchErr != nil {
			if lockCtx.Err() != nil {
				return fetchErr
			}
			issueFetchWarning(warn, FetchWarning{Repository: repo, Operation: "fetch default branch", Err: fetchErr})
			return nil
		}
		fetched = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	if !fetched {
		return resolveLocalDefaultBranch(ctx, git, repo, branch)
	}

	// fetch 成功後は判定に使う ref だけを調べる。ここで gitx.ResolveRef を呼ぶと
	// local を優先するため、remote ref の欠落を見落とす。
	local, localOK, localErr := resolveExactRef(ctx, git, string(repo.MainPath), "refs/heads/"+branch)
	if localErr != nil {
		if ctx.Err() != nil {
			return "", false, localErr
		}
		issueFetchWarning(warn, FetchWarning{Repository: repo, Operation: "resolve local default branch", Err: localErr})
		return resolveLocalDefaultBranch(ctx, git, repo, branch)
	}
	remote, remoteOK, remoteErr := resolveExactRef(ctx, git, string(repo.MainPath), "refs/remotes/origin/"+branch)
	if remoteErr != nil {
		if ctx.Err() != nil {
			return "", false, remoteErr
		}
		issueFetchWarning(warn, FetchWarning{Repository: repo, Operation: "resolve fetched default branch", Err: remoteErr})
		return resolveLocalDefaultBranch(ctx, git, repo, branch)
	}
	if !remoteOK {
		issueFetchWarning(warn, FetchWarning{Repository: repo, Operation: "fetched default branch ref is missing", Err: errors.New("remote-tracking ref is missing after fetch")})
		return resolveLocalDefaultBranch(ctx, git, repo, branch)
	}
	if !localOK {
		return remote, true, nil
	}

	_, err = git.Run(ctx, string(repo.MainPath), "merge-base", "--is-ancestor", local, remote)
	if err == nil {
		return remote, true, nil
	}
	// exit 1 は祖先でないことを示すため、remote の後退・分岐として local を使う。
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 && gitErr.Result.Stderr == "" {
		return local, true, nil
	}
	if ctx.Err() != nil {
		return "", false, err
	}
	issueFetchWarning(warn, FetchWarning{Repository: repo, Operation: "check default branch fast-forward", Err: err})
	return local, true, nil
}

func resolveLocalDefaultBranch(ctx context.Context, git *gitx.Runner, repo discovery.Repository, branch string) (string, bool, error) {
	return resolveExactRef(ctx, git, string(repo.MainPath), "refs/heads/"+branch)
}

func fetchDefaultBranch(ctx context.Context, git *gitx.Runner, repo discovery.Repository, branch string) error {
	// 明示した refspec と --no-tags/--no-write-fetch-head により、remote-tracking
	// ref だけを更新し、source branch・tag・FETCH_HEAD は変更しない。
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	_, err := git.Run(ctx, string(repo.MainPath), "fetch", "--no-tags", "--no-write-fetch-head", "origin", refspec)
	return err
}

func resolveExactRef(ctx context.Context, git *gitx.Runner, repo, ref string) (string, bool, error) {
	res, err := git.Run(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err == nil {
		return strings.TrimSpace(res.Stdout), true, nil
	}
	if ctx.Err() != nil {
		return "", false, err
	}
	var gitErr *gitx.Error
	if errors.As(err, &gitErr) && gitErr.Result.ExitCode == 1 && gitErr.Result.Stderr == "" {
		return "", false, nil
	}
	return "", false, err
}

func issueFetchWarning(warn func(FetchWarning), warning FetchWarning) {
	if warn != nil {
		warn(warning)
	}
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
