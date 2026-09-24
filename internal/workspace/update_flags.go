package workspace

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
)

const (
	// maxFlaggedUpdatePaths と maxFlaggedUpdateBytes は、1 回の更新で退避する flag 付き path の上限である。
	// 退避先は worktree 内に置けない（untracked・tracked の検査に掛かる）のでメモリに持つため、
	// 個人設定の規模を超える更新は書込み前にCold Startへ戻す。
	maxFlaggedUpdatePaths = 64
	maxFlaggedUpdateBytes = 8 << 20
)

// flaggedUpdatePath は、更新の間だけ解除するflag付きpath 1 件分の退避である。
// hookがflagを張り直さなかった場合に、この内容・mode・flagの種別で元へ戻す。
type flaggedUpdatePath struct {
	path   string
	mode   fs.FileMode
	data   []byte
	skip   bool
	assume bool
}

func indexFlagsOf(entries []flaggedUpdatePath) IndexFlags {
	flags := IndexFlags{}
	for _, entry := range entries {
		if entry.skip {
			flags.SkipWorktree = append(flags.SkipWorktree, entry.path)
		}
		if entry.assume {
			flags.AssumeUnchanged = append(flags.AssumeUnchanged, entry.path)
		}
		flags.FlaggedPaths = append(flags.FlaggedPaths, entry.path)
	}
	return flags
}

// flaggedUpdatePaths は、stat比較を抑止するindex flagが付き、かつ旧OID→新OIDの差分に乗るpathを返す。
// force checkoutが更新できないのはこの集合だけなので、解除と復元の対象もここに限る。
// flagを読めないまま進むと個人設定を壊すため、読み取りの失敗はそのまま返してfail-closedにする。
func (p *Preparer) flaggedUpdatePaths(ctx context.Context, repo discovery.Repository, target, identity, oldOID, newOID string) ([]string, IndexFlags, error) {
	flags, err := p.readWorktreeIndexFlags(ctx, target, identity)
	if err != nil || !flags.Blinding() {
		return nil, flags, err
	}
	diff, err := p.readUpdateTreeDiff(ctx, repo, oldOID, newOID)
	if err != nil {
		return nil, IndexFlags{}, err
	}
	return flaggedPathsInDiff(flags, diff), flags, nil
}

func (p *Preparer) readWorktreeIndexFlags(ctx context.Context, target, identity string) (IndexFlags, error) {
	value, _ := p.worktreeIndexGit(ctx, target, identity)
	return ReadIndexFlags(value, nil)
}

func flaggedPathsInDiff(flags IndexFlags, diff updateTreeDiff) []string {
	var paths []string
	for _, path := range flags.FlaggedPaths {
		if _, changed := diff.newModes[path]; changed {
			paths = append(paths, path)
		}
	}
	return paths
}

// rejectUnrestorableFlaggedPaths は、解除しても元へ戻せないflag付きpathが差分に乗る更新を不適格として扱う。
// 新OIDで消えるpathとmodeが通常file以外へ変わるpathはflagを張り直す先が無く、退避内容を書き戻すと残骸になる。
// 退避はメモリに持つため、件数と合計サイズの上限も同じく書込み前に判定する。
func (p *Preparer) rejectUnrestorableFlaggedPaths(ctx context.Context, target, identity string, diff updateTreeDiff, root *os.Root) error {
	flags, err := p.readWorktreeIndexFlags(ctx, target, identity)
	if err != nil {
		return err
	}
	if !flags.Blinding() {
		return nil
	}
	paths := flaggedPathsInDiff(flags, diff)
	if len(paths) == 0 {
		return nil
	}
	if len(paths) > maxFlaggedUpdatePaths {
		return fmt.Errorf("%w: %d index-flagged paths in the diff exceed the update limit of %d", ErrUpdateIneligible, len(paths), maxFlaggedUpdatePaths)
	}
	total := int64(0)
	for _, path := range paths {
		if mode := diff.newModes[path]; mode != "100644" && mode != "100755" {
			return fmt.Errorf("%w: index flag on %s cannot be reinstated because the requested OID has no regular file there", ErrUpdateIneligible, path)
		}
		info, err := root.Lstat(filepath.FromSlash(path))
		if err != nil {
			return fmt.Errorf("%w: index-flagged path %s is not readable in the standby: %w", ErrUpdateIneligible, path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: index-flagged path %s is not a regular file in the standby", ErrUpdateIneligible, path)
		}
		total += info.Size()
		if total > maxFlaggedUpdateBytes {
			return fmt.Errorf("%w: index-flagged paths in the diff exceed the update limit of %d bytes", ErrUpdateIneligible, int64(maxFlaggedUpdateBytes))
		}
	}
	return nil
}

// checkoutUpdate はstandbyを要求OIDへ切り替える。
// flag付きpathが差分に乗る場合は「解除 → checkout → post-checkout → 復元」の順に進め、
// hookがflagを張り直さない構成でも個人設定の内容・mode・flagが残るようにする。
func (p *Preparer) checkoutUpdate(ctx context.Context, repo discovery.Repository, target, identity, oldOID, newOID string, root *os.Root) error {
	paths, flags, err := p.flaggedUpdatePaths(ctx, repo, target, identity, oldOID, newOID)
	if err != nil {
		return err
	}
	stashed, err := stashFlaggedPaths(root, paths, flags)
	if err != nil {
		return err
	}
	value, run := p.worktreeIndexGit(ctx, target, identity)
	if len(stashed) > 0 {
		if err := ClearIndexFlags(run, value, nil, indexFlagsOf(stashed)); err != nil {
			return err
		}
	}
	if _, err := p.RunGitInWorktree(ctx, target, identity, nil, nil, "-c", "core.hooksPath=/dev/null", "checkout", "--detach", "--force", newOID); err != nil {
		return err
	}
	if len(stashed) == 0 {
		return nil
	}
	// hookがflagを張り直す構成では、個人設定はここで作り直される。
	// 実行するのはflagを解除した更新だけで、通常の更新は外部コマンドを動かさない既存の流儀のままにする。
	if err := p.runPostCheckout(ctx, target, identity, newOID); err != nil {
		return err
	}
	return restoreFlaggedPaths(root, run, value, stashed)
}

// stashFlaggedPaths は解除対象のpathの内容・mode・flagの種別をメモリへ退避する。
func stashFlaggedPaths(root *os.Root, paths []string, flags IndexFlags) ([]flaggedUpdatePath, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	stashed := make([]flaggedUpdatePath, 0, len(paths))
	total := int64(0)
	for _, path := range paths {
		entry := flaggedUpdatePath{path: path, skip: slices.Contains(flags.SkipWorktree, path), assume: slices.Contains(flags.AssumeUnchanged, path)}
		file, err := root.OpenFile(filepath.FromSlash(path), os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: open index-flagged path %s: %w", ErrUpdateIneligible, path, err)
		}
		info, err := file.Stat()
		if err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("%w: read index-flagged path %s: %w", ErrUpdateIneligible, path, err)
		}
		if !info.Mode().IsRegular() {
			err = fmt.Errorf("index-flagged path %s is not a regular file", path)
			_ = file.Close()
			return nil, fmt.Errorf("%w: read index-flagged path %s: %w", ErrUpdateIneligible, path, err)
		}
		remaining := int64(maxFlaggedUpdateBytes) - total
		if info.Size() > remaining {
			_ = file.Close()
			return nil, fmt.Errorf("%w: index-flagged paths in the diff exceed the update limit of %d bytes", ErrUpdateIneligible, int64(maxFlaggedUpdateBytes))
		}
		data, err := io.ReadAll(io.LimitReader(file, remaining+1))
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: read index-flagged path %s: %w", ErrUpdateIneligible, path, err)
		}
		// 読み取り中に file が伸びても、上限を越えた内容を退避先へ積まない。
		if int64(len(data)) > remaining {
			return nil, fmt.Errorf("%w: index-flagged paths in the diff exceed the update limit of %d bytes", ErrUpdateIneligible, int64(maxFlaggedUpdateBytes))
		}
		total += int64(len(data))
		entry.mode, entry.data = info.Mode().Perm(), data
		stashed = append(stashed, entry)
	}
	return stashed, nil
}

// restoreFlaggedPaths は、hookがflagを張り直さなかったpathへ退避した内容・mode・flagを戻す。
// この復元は更新後のtracked clean検査より前に終える必要がある。flagの無い個人版は差分として検出されるためである。
func restoreFlaggedPaths(root *os.Root, run GitRunFunc, value GitValueFunc, stashed []flaggedUpdatePath) error {
	current, err := ReadIndexFlags(value, nil)
	if err != nil {
		return err
	}
	var restored []flaggedUpdatePath
	for _, entry := range stashed {
		if current.Has(entry.path) {
			continue
		}
		if _, ok := current.Paths[entry.path]; !ok {
			return fmt.Errorf("%w: index-flagged path %s left the index during the update", ErrUpdateIneligible, entry.path)
		}
		if err := writeStashedPath(root, entry); err != nil {
			return fmt.Errorf("%w: %w", ErrUpdateIneligible, err)
		}
		restored = append(restored, entry)
	}
	if len(restored) == 0 {
		return nil
	}
	if err := ApplyIndexFlags(run, value, nil, indexFlagsOf(restored)); err != nil {
		return fmt.Errorf("%w: %w", ErrUpdateIneligible, err)
	}
	return nil
}

func writeStashedPath(root *os.Root, entry flaggedUpdatePath) error {
	file, err := root.OpenFile(filepath.FromSlash(entry.path), os.O_WRONLY|os.O_TRUNC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("reopen index-flagged path %s: %w", entry.path, err)
	}
	defer file.Close()
	if _, err := file.Write(entry.data); err != nil {
		return fmt.Errorf("restore index-flagged path %s: %w", entry.path, err)
	}
	if err := file.Chmod(entry.mode); err != nil {
		return fmt.Errorf("restore mode of index-flagged path %s: %w", entry.path, err)
	}
	return nil
}
