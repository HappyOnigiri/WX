package workspace

// このファイルは`cloneCOW`の呼び出しと、cloneが成功した後にしか進まない処理だけを持つ。
// linuxでは`cloneCOW`が常にENOTSUPを返してここから先へ到達しないため、`coverage-exclusions.txt`でcoverageの分母から外している。
// clone後の差し替えを増やすときはこのファイルへ足し、cloneの有無に依らず成立する契約は`cow.go`・`cow_place.go`・`cow_share.go`へ置く。

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/state"
)

// placeFile は1件を clone し、置けたかを返す。
// 置けなかった leaf は通常 checkout に回るだけなので、共有できない理由では準備を止めない。
func (c *cowPlacer) placeFile(source, destination *os.File, directory, leaf string) (bool, error) {
	start := time.Now()
	fd, err := unix.Openat(int(source.Fd()), leaf, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		// main 側の実体が走査中に消えた・symlink へ変わった回は共有対象外にするだけでよい。
		return false, nil
	}
	in := os.NewFile(uintptr(fd), leaf)
	defer func() { _ = in.Close() }()
	c.stats.open.observe(start)
	start = time.Now()
	cloneErr := cloneCOW(in, destination, leaf)
	c.stats.clone.observe(start)
	if cloneErr != nil {
		// 宛先に既に実体がある回は、通常 checkout の結果を CoW で上書きしないために諦める。
		// donor 側は共有できる状態なので、置換方式でまだ拾える候補として数える。
		if errors.Is(cloneErr, os.ErrExist) {
			c.stats.pending.Add(1)
			return false, nil
		}
		return false, fmt.Errorf("clone %s: %w", joinCOWPath(directory, leaf), cloneErr)
	}
	c.stats.shared.Add(1)
	return true, nil
}

func (s *cowSharer) replaceWithClone(ctx context.Context, scratch *cowScratch, in, original, parent *os.File, leaf string, before unix.Stat_t) (result error) {
	temporary := cowTemporaryPrefix + rand.Text()
	start := time.Now()
	if err := cloneCOW(in, parent, temporary); err != nil {
		return err
	}
	s.stats.clone.observe(start)
	candidate, err := openCOWLeaf(parent, temporary)
	if err != nil {
		return fmt.Errorf("%w: open CoW clone: %w", state.ErrOwnership, err)
	}
	defer func() { _ = candidate.Close() }()
	candidateInfo, err := candidate.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat CoW clone: %w", state.ErrOwnership, err)
	}
	cleanupInfo := candidateInfo
	defer func() {
		// swap 後は元ファイルが temporary にある。証明できない物は消さず隔離へ渡す。
		if errors.Is(result, state.ErrOwnership) {
			return
		}
		verifyStart := time.Now()
		if err := verifyCOWLeaf(parent, temporary, cleanupInfo); err != nil {
			result = err
			return
		}
		s.stats.verify.observe(verifyStart)
		unlinkStart := time.Now()
		if err := unix.Unlinkat(int(parent.Fd()), temporary, 0); err != nil {
			result = fmt.Errorf("%w: remove CoW temporary: %w", state.ErrOwnership, err)
			return
		}
		s.stats.unlink.observe(unlinkStart)
	}()
	start = time.Now()
	equal, err := sameCOWBytes(ctx, original, candidate)
	s.stats.compare.observe(start)
	if err != nil || !equal {
		return err
	}
	start = time.Now()
	compatible, err := cowMetadata(original, candidate, before, scratch)
	s.stats.metadata.observe(start)
	if err == nil && !compatible {
		if has, xattrErr := hasExtendedAttributes(in); xattrErr == nil && has {
			s.stats.skippedXattrs.Add(1)
		}
	}
	if err != nil || !compatible {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	originalInfo, err := original.Stat()
	if err != nil {
		return err
	}
	start = time.Now()
	if err := swapCOW(parent, temporary, leaf); err != nil {
		return err
	}
	s.stats.swap.observe(start)
	// swap は atomic なので入れ替わりは確認し直さない。cleanup が消す inode の同一性だけ後で検査する。
	cleanupInfo = originalInfo
	s.stats.shared.Add(1)
	return nil
}
