package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// SubmoduleCapsule は snapshot が保存した子 repository 1 件である。
// CapsuleOID は子の worktree tree を持ち、親に HEAD・index tree 用 commit・停止中 rebase 用 commit を並べた commit で、
// この 1 本の ref だけで保存した全 object の到達性を確保する。復元は commit の親子関係を辿らず、記録した各 OID を直接使う。
// ModuleDir は ref の公開先である source のローカル module、GitDir は capsule を作った子の gitdir で公開の fetch 元になる。
// commentlint:allow-long -- capsule 1 本で到達性を確保する構造と、公開の向きを保守時に確認できるようにする
type SubmoduleCapsule struct {
	Path, Name                                                   string
	HeadOID, HeadRef, IndexTreeOID, WorktreeTreeOID, GitStateOID string
	CapsuleOID, CapsuleRef                                       string
	ModuleDir, GitDir                                            string
}

// Snapshot は capsule を state の保存形へ畳む。ModuleDir・GitDir は保存時だけの値なので載せない。
func (c SubmoduleCapsule) Snapshot(sessionID, repositoryID string) state.SubmoduleSnapshot {
	return state.SubmoduleSnapshot{
		SessionID: sessionID, RepositoryID: repositoryID, Path: c.Path, Name: c.Name,
		HeadOID: c.HeadOID, HeadRef: c.HeadRef, IndexTreeOID: c.IndexTreeOID, WorktreeTreeOID: c.WorktreeTreeOID,
		GitStateOID: c.GitStateOID, CapsuleOID: c.CapsuleOID, CapsuleRef: c.CapsuleRef,
	}
}

// submoduleCapsuleRef は子 1 件の capsule を公開する ref 名を返す。
// path を含めた stable ID にするのは、同じ session・repository の子を ref 名で区別するためである。
func submoduleCapsuleRef(sessionID, repositoryID, path string) string {
	return fmt.Sprintf("refs/wx/recovery/%s/%s/submodule/%s", sessionID, repositoryID, domain.StableID("submodule", sessionID, repositoryID, path))
}

// submoduleGit は親 worktree に束縛した runner から、子の中で動く runner を作る。
// RunGitInWorktree は worktree root へ fchdir で束縛するため、子で動かすには `-C` が要る。
// `-c` を渡す呼び出しがあるので、`-C` は常に先頭へ置いて subcommand より前に並ぶようにする。
func submoduleGit(value gitValueFunc, run gitRunFunc, path string) (gitValueFunc, gitRunFunc) {
	prefix := func(args []string) []string {
		return append([]string{"-C", path}, args...)
	}
	return func(env []string, args ...string) (string, error) {
			return value(env, prefix(args)...)
		}, func(env []string, input []byte, args ...string) (gitx.Result, error) {
			return run(env, input, prefix(args)...)
		}
}

// captureSubmodules は未保全と判定された子だけを capsule へ保存し、保存できた子を reasons から外す。
// clean な子は prepare の実体化で元に戻るため対象にしない。submodule を多数持つ repository で Git の起動を増やさないためである。
// 子 1 件の失敗は snapshot 全体を失敗させず、その子を未保全の記録として残す。親の保存は既に済んでおり、
// ここで失敗させると再開手段まで失うためである。
// commentlint:allow-long -- 対象を絞る理由と、失敗を snapshot へ昇格させない理由を残す
func (m *Manager) captureSubmodules(repo discovery.Repository, value gitValueFunc, run gitRunFunc, sessionID string, modules []workspace.Submodule, reasons map[string][]string) []SubmoduleCapsule {
	if len(reasons) == 0 {
		return nil
	}
	var capsules []SubmoduleCapsule
	for _, module := range modules {
		if len(reasons[module.Path]) == 0 {
			continue
		}
		moduleDir, ok := m.submoduleModuleDir(repo, module.Name)
		if !ok {
			// ローカル module が無い子は capsule の書込み先が無い。従来どおり未保全の記録に留める。
			continue
		}
		capsule, nested, err := captureSubmodule(value, run, sessionID, string(repo.ID), module, moduleDir)
		for _, path := range nested {
			// 入れ子 submodule は wx が実体化しないため保存できない。保護だけを効かせるために記録へ加える。
			reasons[module.Path+"/"+path.path] = append(reasons[module.Path+"/"+path.path], path.reasons...)
		}
		if err != nil {
			reasons[module.Path] = append(reasons[module.Path], submoduleCaptureReason(err))
			continue
		}
		delete(reasons, module.Path)
		capsules = append(capsules, capsule)
	}
	return capsules
}

// submoduleModuleDir は source のローカル module を返す。module directory の配下に収まらない name は使わない。
func (m *Manager) submoduleModuleDir(repo discovery.Repository, name string) (string, bool) {
	commonModules := filepath.Join(string(repo.CommonDir), "modules")
	source := filepath.Join(commonModules, name)
	if !domain.IsWithin(commonModules, source) {
		return "", false
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return source, true
}

// errUnmergedSubmoduleIndex は子の index に未解消の衝突が残り、tree を書けなかったことを表す。
var errUnmergedSubmoduleIndex = errors.New("submodule index has unmerged entries")

// submoduleCaptureReason は保存の失敗を記録用の理由コードへ畳む。
func submoduleCaptureReason(err error) string {
	if errors.Is(err, errUnmergedSubmoduleIndex) {
		return ReasonUnmergedIndex
	}
	return ReasonSaveFailed
}

// nestedSubmodule は子の中で見つかった入れ子 submodule 1 件である。path は子からの相対 path である。
type nestedSubmodule struct {
	path    string
	reasons []string
}

// captureSubmodule は子 1 件の HEAD・index・worktree・停止中 rebase を capsule commit へ畳み、子の gitdir に ref を作る。
// 未追跡 file と未 push commit は worktree tree と HEAD から到達できるため、この 1 本で一緒に保護される。
// 公開（ローカル module への fetch）は行わない。親と同じく、DB へ記録してから publish するためである。
func captureSubmodule(parentValue gitValueFunc, parentRun gitRunFunc, sessionID, repositoryID string, module workspace.Submodule, moduleDir string) (SubmoduleCapsule, []nestedSubmodule, error) {
	value, run := submoduleGit(parentValue, parentRun, module.Path)
	gitDir, err := value(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return SubmoduleCapsule{}, nil, fmt.Errorf("resolve submodule git directory: %w", err)
	}
	head, err := value(nil, "rev-parse", "HEAD")
	if err != nil {
		return SubmoduleCapsule{}, nil, fmt.Errorf("resolve submodule HEAD: %w", err)
	}
	headRef := ""
	if symbolic, symbolicErr := value(nil, "symbolic-ref", "-q", "HEAD"); symbolicErr == nil {
		headRef = symbolic
	}
	nested, err := nestedSubmodules(value)
	if err != nil {
		return SubmoduleCapsule{}, nil, err
	}
	indexTree, err := value(nil, "write-tree")
	if err != nil {
		return SubmoduleCapsule{}, nested, fmt.Errorf("%w: %w", errUnmergedSubmoduleIndex, err)
	}
	worktreeTree, err := submoduleWorktreeTree(value, run, head)
	if err != nil {
		return SubmoduleCapsule{}, nested, err
	}
	gitState, err := captureGitState(value, gitRunner(run), head)
	if err != nil {
		return SubmoduleCapsule{}, nested, fmt.Errorf("capture submodule rebase state: %w", err)
	}
	indexCommit, err := run(recoveryCommitEnv(nil), []byte("wx submodule index snapshot\n"), "commit-tree", indexTree, "-p", head)
	if err != nil {
		return SubmoduleCapsule{}, nested, fmt.Errorf("commit submodule index tree: %w", err)
	}
	args := []string{"commit-tree", worktreeTree, "-p", head, "-p", strings.TrimSpace(indexCommit.Stdout)}
	if gitState != "" {
		args = append(args, "-p", gitState)
	}
	capsuleCommit, err := run(recoveryCommitEnv(nil), []byte("wx submodule snapshot\n"), args...)
	if err != nil {
		return SubmoduleCapsule{}, nested, fmt.Errorf("commit submodule capsule: %w", err)
	}
	capsule := SubmoduleCapsule{
		Path: module.Path, Name: module.Name, HeadOID: head, HeadRef: headRef,
		IndexTreeOID: indexTree, WorktreeTreeOID: worktreeTree, GitStateOID: gitState,
		CapsuleOID: strings.TrimSpace(capsuleCommit.Stdout), CapsuleRef: submoduleCapsuleRef(sessionID, repositoryID, module.Path),
		ModuleDir: moduleDir, GitDir: gitDir,
	}
	// 公開は ref 名で取りに行くため、先に子の gitdir へ ref を作る。
	// OID 指定の fetch は相手側の uploadpack.allowAnySHA1InWant に依存し、設定が無い環境で静かに失敗する。
	if _, err := run(nil, nil, "update-ref", "--create-reflog", capsule.CapsuleRef, capsule.CapsuleOID); err != nil {
		return SubmoduleCapsule{}, nested, fmt.Errorf("create submodule capsule ref: %w", err)
	}
	return capsule, nested, nil
}

// submoduleWorktreeTree は子の worktree の現状を tree にする。手順は親と同じで、
// 一時 index へ HEAD を読み、元 index の flag を引き写してから add する。
// flag 付き path を HEAD の内容のまま記録する点も親と揃える。
func submoduleWorktreeTree(value gitValueFunc, run gitRunFunc, head string) (string, error) {
	flags, err := readIndexFlags(value, nil)
	if err != nil {
		return "", err
	}
	tmp, cleanup, err := temporaryIndex("submodule snapshot", ".wx-submodule-index-*")
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := run(env, nil, "read-tree", head); err != nil {
		return "", fmt.Errorf("read submodule HEAD into a temporary index: %w", err)
	}
	if err := applyIndexFlags(run, value, env, flags); err != nil {
		return "", err
	}
	if _, err := run(env, nil, addWorktreeArgs()...); err != nil {
		return "", fmt.Errorf("add submodule worktree contents: %w", err)
	}
	tree, err := value(env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write submodule worktree tree: %w", err)
	}
	return tree, nil
}

// nestedSubmodules は子の中の入れ子 submodule の変化を返す。
// `--ignore-submodules=none` を付けるのは、利用者設定の submodule.<name>.ignore と diff.ignoreSubmodules が
// 孫の変化を隠し得るためである。
func nestedSubmodules(value gitValueFunc) ([]nestedSubmodule, error) {
	output, err := value(nil, "-c", "core.quotePath=false", "status", "--porcelain=v2", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, fmt.Errorf("inspect nested submodules: %w", err)
	}
	reasons := statusSubmoduleReasons(output)
	paths := make([]string, 0, len(reasons))
	for path := range reasons {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]nestedSubmodule, 0, len(paths))
	for _, path := range paths {
		out = append(out, nestedSubmodule{path: path, reasons: reasons[path]})
	}
	return out, nil
}

// publishSubmoduleCapsules は capsule を source のローカル module へ取り込む。
// 復元側の実体化は必ずそのローカル module から clone するため、親の object store へ置くと
// 親が gitlink を commit した子の commit を clone 時点で解決できない。冪等性は親の recovery ref と同じ規則で担保する。
func (m *Manager) publishSubmoduleCapsules(ctx context.Context, repo discovery.Repository, capsules []SubmoduleCapsule) error {
	worktrees := filepath.Join(string(repo.CommonDir), "worktrees")
	for _, capsule := range capsules {
		if !domain.IsWithin(worktrees, capsule.GitDir) {
			return fmt.Errorf("submodule %s git directory is outside the source repository worktrees", capsule.Path)
		}
		existing, err := m.gitValue(ctx, capsule.ModuleDir, nil, "--git-dir=.", "show-ref", "--verify", "--hash", capsule.CapsuleRef)
		if err == nil {
			if existing != capsule.CapsuleOID {
				return fmt.Errorf("submodule capsule ref %s already points to unexpected object", capsule.CapsuleRef)
			}
			continue
		}
		// ローカル path からの fetch には protocol.file.allow=always が要る。設定は必ず `-c` で渡す。
		// gitx の環境サニタイズが GIT_CONFIG_* を落とすため、環境変数や repo-local config では子プロセスに効かない。
		if _, err := m.Git.Run(ctx, capsule.ModuleDir, "--git-dir=.", "-c", "protocol.file.allow=always",
			"fetch", "--no-tags", "--no-write-fetch-head", capsule.GitDir, capsule.CapsuleRef+":"+capsule.CapsuleRef); err != nil {
			return fmt.Errorf("publish submodule capsule %s: %w", capsule.Path, err)
		}
	}
	return nil
}

// deleteSubmoduleCapsuleRefs は子の capsule ref を、親の recovery ref と同じ規則で消す。
// 期待 OID と一致するときだけ削除し、一致しなければ拒否して原因を残す。
func (m *Manager) deleteSubmoduleCapsuleRefs(ctx context.Context, repo discovery.Repository, submodules []state.SubmoduleSnapshot) error {
	for _, sub := range submodules {
		moduleDir, ok := m.submoduleModuleDir(repo, sub.Name)
		if !ok {
			// ローカル module ごと無い場合、ref も一緒に消えている。
			continue
		}
		if _, err := m.Git.Run(ctx, moduleDir, "--git-dir=.", "check-ref-format", sub.CapsuleRef); err != nil {
			return fmt.Errorf("invalid submodule capsule ref %q: %w", sub.CapsuleRef, err)
		}
		got, err := m.gitValue(ctx, moduleDir, nil, "--git-dir=.", "show-ref", "--verify", "--hash", sub.CapsuleRef)
		if err != nil {
			var gitErr *gitx.Error
			if errors.As(err, &gitErr) && (gitErr.Result.ExitCode == 1 || gitErr.Result.ExitCode == 128) {
				continue // A prior interrupted GC may already have removed this ref.
			}
			return err
		}
		if got != sub.CapsuleOID {
			return fmt.Errorf("refuse to delete submodule capsule ref %s with unexpected OID", sub.CapsuleRef)
		}
		if _, err := m.Git.Run(ctx, moduleDir, "--git-dir=.", "update-ref", "-d", sub.CapsuleRef, sub.CapsuleOID); err != nil {
			return err
		}
	}
	return nil
}

// restoreSubmodules は子を親の read-tree より前に戻す。
// 親の snapshot worktree tree は子の移動後 HEAD を gitlink として持つため、後に回すと親の tree 一致検証が必ず失敗する。
// 子の復元失敗は Restore の失敗として扱う。中途半端な子を黙って残すより、隔離して原因を残す方を選ぶ。
func (m *Manager) restoreSubmodules(repo discovery.Repository, value gitValueFunc, run gitRunFunc, submodules []state.SubmoduleSnapshot) error {
	for _, sub := range submodules {
		moduleDir, ok := m.submoduleModuleDir(repo, sub.Name)
		if !ok {
			return fmt.Errorf("submodule %s has no local module to restore from", sub.Path)
		}
		if err := restoreSubmodule(value, run, sub, moduleDir); err != nil {
			return fmt.Errorf("restore submodule %s: %w", sub.Path, err)
		}
	}
	return nil
}

// verifySubmoduleCapsuleRefs は復元に使う capsule ref が記録どおりであることを、object を使う前に確かめる。
func (m *Manager) verifySubmoduleCapsuleRefs(ctx context.Context, repo discovery.Repository, submodules []state.SubmoduleSnapshot) error {
	for _, sub := range submodules {
		moduleDir, ok := m.submoduleModuleDir(repo, sub.Name)
		if !ok {
			return fmt.Errorf("submodule %s has no local module holding its capsule", sub.Path)
		}
		got, err := m.gitValue(ctx, moduleDir, nil, "--git-dir=.", "rev-parse", "--verify", sub.CapsuleRef)
		if err != nil || got != sub.CapsuleOID {
			return fmt.Errorf("submodule capsule ref %s changed during restore", sub.CapsuleRef)
		}
	}
	return nil
}

// restoreSubmodule は子 1 件へ capsule を取り込み、HEAD・worktree・index・停止中 rebase を保存時の状態へ戻す。
// 取り込みの向きは保存時と逆で、ローカル module から子の gitdir へ fetch する。
func restoreSubmodule(parentValue gitValueFunc, parentRun gitRunFunc, sub state.SubmoduleSnapshot, moduleDir string) error {
	value, run := submoduleGit(parentValue, parentRun, sub.Path)
	if _, err := run(nil, nil, "-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-write-fetch-head", moduleDir, "+"+sub.CapsuleRef+":"+sub.CapsuleRef); err != nil {
		return fmt.Errorf("fetch capsule: %w", err)
	}
	for _, object := range []string{sub.CapsuleOID, sub.HeadOID + "^{commit}", sub.IndexTreeOID + "^{tree}", sub.WorktreeTreeOID + "^{tree}"} {
		if _, err := run(nil, nil, "cat-file", "-e", object); err != nil {
			return fmt.Errorf("capsule does not carry %s: %w", object, err)
		}
	}
	// HEAD は file を書かずに ref だけで戻し、実体の書き出しは直後の read-tree に任せる。
	if sub.HeadRef != "" {
		if _, err := run(nil, nil, "update-ref", sub.HeadRef, sub.HeadOID); err != nil {
			return fmt.Errorf("restore branch %s: %w", sub.HeadRef, err)
		}
		if _, err := run(nil, nil, "symbolic-ref", "HEAD", sub.HeadRef); err != nil {
			return fmt.Errorf("attach HEAD to %s: %w", sub.HeadRef, err)
		}
	} else if _, err := run(nil, nil, "update-ref", "--no-deref", "HEAD", sub.HeadOID); err != nil {
		return fmt.Errorf("restore detached HEAD: %w", err)
	}
	// --no-recurse-submodules は親側と同じ理由で要る。入れ子 submodule は実体化しないため、
	// Git に gitlink を辿らせると実体の無い孫の checkout を試みる。
	if _, err := run(nil, nil, "read-tree", "--reset", "-u", "--no-recurse-submodules", sub.WorktreeTreeOID); err != nil {
		return fmt.Errorf("restore worktree: %w", err)
	}
	if _, err := run(nil, nil, "read-tree", sub.IndexTreeOID); err != nil {
		return fmt.Errorf("restore index: %w", err)
	}
	// git_state_oid が空でも呼ぶ。再利用した slot の古い進行情報を残さないためである。
	wantGitState := ""
	if sub.GitStateOID != "" {
		resolved, err := value(nil, "rev-parse", sub.GitStateOID+"^{tree}")
		if err != nil {
			return fmt.Errorf("resolve rebase state tree: %w", err)
		}
		wantGitState = resolved
	}
	if err := restoreGitState(value, gitRunner(run), wantGitState); err != nil {
		return fmt.Errorf("restore in-progress rebase state: %w", err)
	}
	return verifyRestoredSubmodule(value, run, sub)
}

// verifyRestoredSubmodule は戻した子が保存時と同じ HEAD・index・worktree であることを確かめる。
func verifyRestoredSubmodule(value gitValueFunc, run gitRunFunc, sub state.SubmoduleSnapshot) error {
	head, err := value(nil, "rev-parse", "HEAD")
	if err != nil || head != sub.HeadOID {
		return errors.New("restored HEAD does not match the snapshot")
	}
	indexTree, err := value(nil, "write-tree")
	if err != nil || indexTree != sub.IndexTreeOID {
		return errors.New("restored index does not match the snapshot")
	}
	tmp, cleanup, err := temporaryIndex("submodule restore", ".wx-submodule-verify-index-*")
	if err != nil {
		return err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := run(env, nil, "read-tree", sub.HeadOID); err != nil {
		return err
	}
	if _, err := run(env, nil, addWorktreeArgs()...); err != nil {
		return err
	}
	worktreeTree, err := value(env, "write-tree")
	if err != nil || worktreeTree != sub.WorktreeTreeOID {
		return errors.New("restored working tree does not match the snapshot")
	}
	return nil
}
