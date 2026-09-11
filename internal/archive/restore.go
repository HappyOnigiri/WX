package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/state"
)

func (m *Manager) Restore(ctx context.Context, repo discovery.Repository, target, slotID string, s state.Snapshot) error {
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
		if err := m.Preparer.ValidateRestoringOwnership(ctx, repo, target, s.HeadOID, slotID); err != nil {
			return fmt.Errorf("validate restore worktree before snapshot: %w", err)
		}
		// 復元先の index flag は外さない。外すと git が skip-worktree の実ファイルを tree の内容で上書きし、
		// hook が slot ごとに作り直した個人設定を失う。snapshot 側も flag 付き path を HEAD の内容で記録しているため、
		// entry は一致し `read-tree --reset -u` は flag を保ったまま通る。
		// ただし assume-unchanged の実ファイルだけは entry が一致していても git が書き戻すので、wx 側では防げない。
		// commentlint:allow-long -- flag を外さない理由と git 側の例外を説明する
		flags, err := readIndexFlags(targetRun, nil)
		if err != nil {
			return err
		}
		if _, err := targetRun(nil, nil, "read-tree", "--reset", "-u", s.WorktreeOID+"^{tree}"); err != nil {
			return err
		}
		if _, err := targetRun(nil, nil, "read-tree", s.IndexTreeOID); err != nil {
			return err
		}
		// 2 本の read-tree は index を丸ごと置き換えて flag を落とすため、resume prepare より前に立て直す。
		if err := applyIndexFlags(targetRun, nil, flags); err != nil {
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
		tmpFile, err := os.CreateTemp("", ".wx-verify-index-*")
		if err != nil {
			return fmt.Errorf("create temporary restore index: %w", err)
		}
		tmp := tmpFile.Name()
		if err := tmpFile.Close(); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("close temporary restore index: %w", err)
		}
		defer func() { _ = os.Remove(tmp) }()
		env := []string{"GIT_INDEX_FILE=" + tmp}
		if _, err := targetRun(env, nil, "read-tree", s.HeadOID); err != nil {
			return err
		}
		// 検証用の一時 index にも同じ flag を立て、snapshot と同じ基準（flag 付き path は HEAD の内容）で tree を作る。
		// snapshot 側と同じく add に pathspec を渡さない（渡すと flag 付き path だけに一致したとき git が exit 1 にする）。
		if err := applyIndexFlags(targetRun, env, flags); err != nil {
			return err
		}
		if _, err := targetRun(env, nil, "add", "-A"); err != nil {
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
		// ここでの ownership 再証明は行わない。
		// 直前の PrepareResumeWithIdentity と直後の FinishRestoreWithIdentity が同じ検査を行い、その間は読み取りだけである。
		if err := m.Preparer.FinishRestoreWithIdentity(ctx, repo, target, s.HeadOID, slotID, targetIdentity); err != nil {
			return err
		}
		return nil
	})
}
