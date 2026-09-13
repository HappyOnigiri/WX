package archive

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

// changedAgainstHead は snapshot の worktree tree が HEAD と異なる path を返す。
// 復元先で実体を書き戻す対象を、作業が実際にあった path だけに絞るために使う。
func changedAgainstHead(value gitValueFunc, s state.Snapshot) ([]string, error) {
	listing, err := value(nil, "diff-tree", "-r", "--name-only", "-z", s.HeadOID+"^{tree}", s.WorktreeOID+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("list paths changed in snapshot: %w", err)
	}
	var paths []string
	for _, path := range strings.Split(listing, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// Restore は snapshot を復元先 worktree へ戻す。submodules は子 1 件ずつの snapshot で、親より先に戻す。
func (m *Manager) Restore(ctx context.Context, repo discovery.Repository, target, slotID string, s state.Snapshot, submodules []state.SubmoduleSnapshot) error {
	if expiry, err := time.Parse(time.RFC3339Nano, s.ExpiresAt); err != nil || !expiry.After(time.Now()) {
		return errors.New("recovery snapshot has expired")
	}
	// 先に clean base を作成してロックする。resume 段階の prepare は snapshot tree と
	// 保存 index を下で復元するまで遅延させる。
	if m.Preparer == nil {
		return errors.New("restore requires a workspace preparer")
	}
	// clean base の作成から READY 化までを同じ slot 排他の下に置く。
	// 内側の PrepareForRestore は取得済みの ctx を受け取るので、同じ slot を取り直さない。
	ctx, releaseSlot, err := m.lockSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()
	if err := m.Preparer.PrepareForRestore(ctx, repo, target, s.HeadOID, slotID); err != nil {
		return err
	}
	targetIdentity, err := m.Preparer.WorktreeIdentity(target)
	if err != nil {
		return fmt.Errorf("%w: capture restored worktree identity: %w", state.ErrOwnership, err)
	}
	return m.Git.WithCommonDirLock(ctx, string(repo.CommonDir), func(ctx context.Context) error {
		targetValue := func(env []string, args ...string) (string, error) {
			result, runErr := m.Preparer.RunGitInWorktree(ctx, target, targetIdentity, env, nil, args...)
			if runErr != nil {
				return "", runErr
			}
			return strings.TrimSpace(result.Stdout), nil
		}
		targetRun := func(env []string, input []byte, args ...string) (gitx.Result, error) {
			return m.Preparer.RunGitInWorktree(ctx, target, targetIdentity, env, input, args...)
		}
		// recovery ref の一致検査はここだけで行う。
		// lock の外で先に見ても object を使う時点までに変わり得るため、lock 取得後の一度に集約している。
		for ref, want := range recoveryRefTargets(s) {
			got, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", "--verify", ref)
			if err != nil || got != want {
				return fmt.Errorf("recovery ref %s changed during restore", ref)
			}
		}
		if err := m.verifySubmoduleCapsuleRefs(ctx, repo, submodules); err != nil {
			return err
		}
		if err := m.Preparer.ValidateRestoringOwnership(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("validate restore worktree before snapshot: %w", err)
		}
		// 子は親の read-tree より前に戻す。親の snapshot worktree tree は子の移動後 HEAD を gitlink として持つため、
		// 後に回すと親の tree 一致検証が必ず不一致になる。
		if err := m.restoreSubmodules(repo, targetValue, targetRun, submodules); err != nil {
			return err
		}
		// 復元先の index flag は外さない。外すと git が skip-worktree の実ファイルを tree の内容で上書きし、
		// hook が slot ごとに作り直した個人設定を失う。snapshot 側も flag 付き path を HEAD の内容で記録しているため、
		// entry は一致し `read-tree --reset -u` は flag を保ったまま通る。
		// ただし assume-unchanged の実ファイルだけは entry が一致していても git が書き戻すので、wx 側では防げない。
		// commentlint:allow-long -- flag を外さない理由と git 側の例外を説明する
		flags, err := readIndexFlags(targetValue, nil)
		if err != nil {
			return err
		}
		// --no-recurse-submodules が無いと、この read-tree は実体化済みの子を gitlink の内容へ戻し、
		// 直前に復元した子の作業ファイルと HEAD の branch を消してしまう。
		if _, err := targetRun(nil, nil, "read-tree", "--reset", "-u", "--no-recurse-submodules", s.WorktreeOID+"^{tree}"); err != nil {
			return err
		}
		// 復元先は sparse 条件を受け継いでいるため、直前の read-tree は範囲外の path を skip-worktree にして
		// 実体を書かない。範囲外で行った作業はここで書き戻す。index がまだ snapshot の worktree tree を指す
		// この位置でしか worktree 側の内容は取り出せない。次の read-tree は index を staged 内容へ置き換え、
		// 未 staged で追加された path の entry は消える。
		// HEAD と差の無い範囲外 path は対象にせず skip-worktree のまま残すので、sparse の利得は損なわない。
		// commentlint:allow-long -- この位置でしか復元できない理由を説明する
		changed, err := changedAgainstHead(targetValue, s)
		if err != nil {
			return err
		}
		materialized, err := materializeSkipped(targetRun, targetValue, changed)
		if err != nil {
			return err
		}
		flags = flags.without(materialized)
		if _, err := targetRun(nil, nil, "read-tree", s.IndexTreeOID); err != nil {
			return err
		}
		// 2 本の read-tree は index を丸ごと置き換えて flag を落とすため、resume prepare より前に立て直す。
		if err := applyIndexFlags(targetRun, targetValue, nil, flags); err != nil {
			return err
		}
		if err := m.Preparer.PrepareResumeWithIdentity(ctx, repo, target, s.HeadOID, slotID, targetIdentity); err != nil {
			return fmt.Errorf("resume prepare: %w", err)
		}
		head, err := targetValue(nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if head != s.HeadOID {
			return errors.New("restored HEAD does not match snapshot")
		}
		if _, detachedErr := targetRun(nil, nil, "symbolic-ref", "-q", "HEAD"); detachedErr == nil {
			return errors.New("restored worktree is not detached")
		} else if errors.Is(detachedErr, state.ErrOwnership) {
			return detachedErr
		}
		indexTree, err := targetValue(nil, "write-tree")
		if err != nil || indexTree != s.IndexTreeOID {
			return errors.New("restored index does not match snapshot")
		}
		// 一時 index への add -A で作業ツリー全体を再計算し、snapshot の tree と比較する。
		// read-tree の後に resume prepare が動くため、prepare command や補助リンクが作った差分はここでしか検出できない。
		// 後続の status は終了コードしか見ておらず代替にならない。
		tmp, cleanup, err := temporaryIndex("restore", ".wx-verify-index-*")
		if err != nil {
			return err
		}
		defer cleanup()
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, err := targetRun(env, nil, "read-tree", s.HeadOID); err != nil {
			return err
		}
		// 検証用の一時 index にも同じ flag を立て、snapshot と同じ基準（flag 付き path は HEAD の内容）で tree を作る。
		// snapshot 側と同じく add に pathspec を渡さない（渡すと flag 付き path だけに一致したとき git が exit 1 にする）。
		if err := applyIndexFlags(targetRun, targetValue, env, flags); err != nil {
			return err
		}
		if _, err := targetRun(env, nil, addWorktreeArgs()...); err != nil {
			return err
		}
		actualWorktreeTree, err := targetValue(env, "write-tree")
		if err != nil {
			return err
		}
		expectedWorktreeTree, err := m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", s.WorktreeOID+"^{tree}")
		if err != nil || actualWorktreeTree != expectedWorktreeTree {
			return errors.New("restored working tree does not match snapshot")
		}
		if _, err := targetRun(nil, nil, "status", "--porcelain=v2", "--untracked-files=all"); err != nil {
			return fmt.Errorf("validate restored status: %w", err)
		}
		if err := m.Preparer.VerifyWorktreeIdentity(target, targetIdentity); err != nil {
			return fmt.Errorf("validate restored worktree identity: %w", err)
		}
		// 衝突 stage の書き戻しは既存の検証をすべて終えた後に置く。
		// 未解消 path が index に無い窓を prepare と一致検証へ持ち込まないためである。
		wantConflict := ""
		if s.ConflictOID != "" {
			wantConflict, err = m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", s.ConflictOID+"^{tree}")
			if err != nil {
				return fmt.Errorf("resolve snapshot conflict state tree: %w", err)
			}
		}
		if err := restoreConflictIndex(targetValue, targetRun, wantConflict); err != nil {
			return fmt.Errorf("restore unmerged index: %w", err)
		}
		// 停止中の操作の制御ファイルも同じ位置で書き戻す。
		// read-tree の後には resume prepare と tree 一致検証が続くため、その相手を操作進行中のリポジトリにしないという意図である。
		wantGitState := ""
		if s.GitStateOID != "" {
			wantGitState, err = m.gitValue(ctx, string(repo.MainPath), nil, "rev-parse", s.GitStateOID+"^{tree}")
			if err != nil {
				return fmt.Errorf("resolve snapshot operation state tree: %w", err)
			}
		}
		if err := restoreGitState(targetValue, targetRun, wantGitState); err != nil {
			return fmt.Errorf("restore in-progress operation state: %w", err)
		}
		// ここでの ownership 再証明は行わない。
		// 直前の PrepareResumeWithIdentity と直後の FinishRestoreWithIdentity が同じ検査を行い、その間は読み取りだけである。
		if err := m.Preparer.FinishRestoreWithIdentity(ctx, repo, target, s.HeadOID, slotID, targetIdentity); err != nil {
			return err
		}
		return nil
	})
}
