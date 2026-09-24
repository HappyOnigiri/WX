package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/state"
)

// ValidateUpdateCandidate は既知の更新不能条件をREADY slotの予約前に検査する。
// 所有権とtracked cleanを直列で確かめてから、読み取りだけの検査を並列に走らせる。
// `git status`はindexを書き換え得るため並列の側へ入れない。
func (p *Preparer) ValidateUpdateCandidate(ctx context.Context, repo discovery.Repository, target, oldOID, newOID string, previous, desired []state.Placement) error {
	if err := p.ValidateReady(ctx, repo, target, oldOID); err != nil {
		return err
	}
	var tracked, untracked map[string]bool
	// 並び順がエラーの優先順位である。並列化前の直列の検査順に揃えている。
	checks := []func(context.Context) error{
		func(ctx context.Context) error { return p.rejectChangedGitlinks(ctx, repo, oldOID, newOID) },
		func(ctx context.Context) error { return p.rejectChangedAttributes(ctx, repo, oldOID, newOID) },
		func(ctx context.Context) error {
			return p.rejectUnrestorableWorktreeState(ctx, repo, target, oldOID, newOID, previous)
		},
		func(ctx context.Context) (err error) {
			tracked, err = p.gitPaths(ctx, target, "ls-tree", "-r", "--name-only", "-z", newOID)
			return err
		},
		// 除外指定を付けない列挙は、untrackedとignoredを別々に列挙した和集合と同じ集合を1回の走査で返す。
		func(ctx context.Context) (err error) {
			untracked, err = p.gitPaths(ctx, target, "ls-files", "--others", "-z")
			return err
		},
	}
	if err := runChecksInOrder(ctx, checks); err != nil {
		return err
	}
	old := placementPathSet(previous)
	desiredPaths := placementPathSet(desired)
	for path := range untracked {
		if old[path] {
			continue
		}
		if pathsConflictAny(path, tracked) || pathsConflictAny(path, desiredPaths) {
			return fmt.Errorf("%w: untracked or ignored path %s conflicts with standby update", ErrUpdateIneligible, path)
		}
	}
	for path := range desiredPaths {
		if pathsConflictAny(path, tracked) {
			return fmt.Errorf("%w: placement path %s becomes tracked at requested OID", ErrUpdateIneligible, path)
		}
	}
	return nil
}

// runChecksInOrder は全ての検査を並列に実行し、完了を待ってからslice順で最初のエラーを返す。
// 呼び出し側はエラーの種類でSTALE化するかを分けるため、到着順に採ると同じ状態でも貸出ごとに判定が変わる。
// 失敗した検査より後ろの順位だけを取り消す。前の順位を取り消すと、取消しのエラーが本来のエラーを覆い得る。
func runChecksInOrder(ctx context.Context, checks []func(context.Context) error) error {
	errs := make([]error, len(checks))
	contexts := make([]context.Context, len(checks))
	cancels := make([]context.CancelFunc, len(checks))
	for i := range checks {
		contexts[i], cancels[i] = context.WithCancel(ctx)
	}
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
	}()
	var wait sync.WaitGroup
	for i, check := range checks {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if errs[i] = check(contexts[i]); errs[i] != nil {
				for _, cancel := range cancels[i+1:] {
					cancel()
				}
			}
		}()
	}
	wait.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// rejectUnrestorableWorktreeState はstandbyの実体に依存する更新不能条件を検査する。
// flag付きpathの退避元と記録済み配置は同じrootで読むため、1つの検査にまとめている。
func (p *Preparer) rejectUnrestorableWorktreeState(ctx context.Context, repo discovery.Repository, target, oldOID, newOID string, previous []state.Placement) error {
	root, err := p.destinationRoot(target)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return err
	}
	if err := p.rejectUnrestorableFlaggedPaths(ctx, repo, target, identity, oldOID, newOID, root); err != nil {
		return err
	}
	if err := validateRecordedPlacements(root, previous); err != nil {
		return fmt.Errorf("%w: %w", ErrUpdateIneligible, err)
	}
	return nil
}

// rejectChangedAttributes は.gitattributesに差のある更新を不適格として扱う。
// 更新の再展開は`git checkout-index`では済まず、内容が同じでstat cacheの一致するfileだけが旧属性のまま残る。
func (p *Preparer) rejectChangedAttributes(ctx context.Context, repo discovery.Repository, oldOID, newOID string) error {
	diff, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "-z", oldOID, newOID, "--", ".gitattributes", ":(glob)**/.gitattributes")
	if err != nil {
		return err
	}
	if diff.Stdout != "" {
		return fmt.Errorf("%w: .gitattributes changed between the standby and the requested OID", ErrUpdateIneligible)
	}
	return nil
}

func (p *Preparer) rejectChangedGitlinks(ctx context.Context, repo discovery.Repository, oldOID, newOID string) error {
	modules, err := p.Git.Run(ctx, string(repo.MainPath), "diff", "--name-only", "-z", oldOID, newOID, "--", ".gitmodules")
	if err != nil {
		return err
	}
	if modules.Stdout != "" {
		return fmt.Errorf("%w: submodule configuration changed", ErrUpdateIneligible)
	}
	oldLinks, err := p.Git.Run(ctx, string(repo.MainPath), "ls-tree", "-r", oldOID)
	if err != nil {
		return err
	}
	newLinks, err := p.Git.Run(ctx, string(repo.MainPath), "ls-tree", "-r", newOID)
	if err != nil {
		return err
	}
	filter := func(output string) string {
		var lines []string
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "160000 ") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	if filter(oldLinks.Stdout) != filter(newLinks.Stdout) {
		return fmt.Errorf("%w: submodule configuration or gitlink OID changed", ErrUpdateIneligible)
	}
	return nil
}

func (p *Preparer) gitPaths(ctx context.Context, target string, args ...string) (map[string]bool, error) {
	identity, err := p.WorktreeIdentity(target)
	if err != nil {
		return nil, err
	}
	result, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, entry := range strings.Split(result.Stdout, "\x00") {
		if entry != "" {
			out[filepath.Clean(entry)] = true
		}
	}
	return out, nil
}

func pathsConflictAny(path string, candidates map[string]bool) bool {
	path = filepath.Clean(path)
	for candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if path == candidate || strings.HasPrefix(path, candidate+string(filepath.Separator)) || strings.HasPrefix(candidate, path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
