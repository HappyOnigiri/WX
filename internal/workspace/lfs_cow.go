package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
)

// LFSObjectCandidate は snapshot で新しく現れた LFS pointer と、その path である。
type LFSObjectCandidate struct {
	Path    string
	Pointer LFSPointer
}

// LFSCompactionResult は snapshot 後の LFS cache 最適化の結果である。
type LFSCompactionResult struct {
	Replaced       int
	ReclaimedBytes int64
	Skipped        int
	Failed         int
}

type lfsCompactFunc func(context.Context, *os.Root, *os.Root, LFSObjectCandidate) (bool, int64, error)

var (
	errLFSVerification = errors.New("LFS object verification failed")
	errLFSNotEligible  = errors.New("LFS object is not eligible for CoW")
)

// verifyLFSClone は clone した実体の size と SHA-256 を pointer と照合する。
// clean filter の結果には依存せず、読み取り中の context cancel も即座に返す。
func verifyLFSClone(ctx context.Context, file *os.File, pointer LFSPointer) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() != pointer.Size {
		return fmt.Errorf("%w: got size %d, want %d", errLFSVerification, info.Size(), pointer.Size)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	buffer := make([]byte, 128<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := file.Read(buffer)
		if count > 0 {
			_, _ = hash.Write(buffer[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	got := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if got != strings.ToLower(pointer.OID) {
		return fmt.Errorf("%w: got %s, want %s", errLFSVerification, got, pointer.OID)
	}
	return nil
}

// CompactLFSObjects は worktree の実体を検証済み CoW clone として cache に保存する。
// 失敗は呼び出し側が snapshot を継続できるよう集約して返すが、cache の bytes は変更しない。
func (p *Preparer) CompactLFSObjects(ctx context.Context, repo discovery.Repository, worktree string, candidates []LFSObjectCandidate) (result LFSCompactionResult, resultErr error) {
	if len(candidates) == 0 {
		return result, nil
	}
	if p == nil {
		return result, errors.New("LFS CoW compaction requires a preparer")
	}
	mode := p.Config.CopyModeForWorkspaceRepository(p.workspaceRootForRepository(repo), repo.RelativePath, string(repo.MainPath))
	if mode == config.CopyModeCopy || !cowAvailable() {
		result.Skipped = len(candidates)
		if p.Log != nil {
			p.Log.Warn("LFS cache CoW skipped", "reason", "copy mode or unsupported platform", "mode", mode, "candidates", len(candidates))
		}
		return result, nil
	}
	return p.compactLFSObjectsWithRoots(ctx, repo, worktree, candidates)
}

// compactLFSBatch は各 object の結果を集約し、1件の失敗で残りの候補を止めない。
// platform adapter を引数に分けることで、CoW の有無にかかわらずこの契約を検証できる。
func (p *Preparer) compactLFSBatch(ctx context.Context, donorRoot, commonRoot *os.Root, candidates []LFSObjectCandidate, compact lfsCompactFunc) (result LFSCompactionResult, resultErr error) {
	var errs []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Failed += len(candidates) - result.Replaced - result.Skipped - result.Failed
			errs = append(errs, err)
			break
		}
		replaced, bytes, err := compact(ctx, donorRoot, commonRoot, candidate)
		if err != nil {
			if errors.Is(err, errLFSVerification) {
				result.Skipped++
				p.logLFSCompactionSkip(candidate, err)
				continue
			}
			result.Failed++
			p.logLFSCompactionFailure(candidate, err)
			errs = append(errs, err)
			continue
		}
		if !replaced {
			result.Skipped++
			p.logLFSCompactionSkip(candidate, errLFSNotEligible)
			continue
		}
		result.Replaced++
		result.ReclaimedBytes = addBytes(result.ReclaimedBytes, bytes)
		if p.Log != nil {
			p.Log.Info("LFS cache CoW replacement", "path", candidate.Path, "oid", candidate.Pointer.OID, "bytes", bytes)
		}
	}
	return result, errors.Join(errs...)
}

func (p *Preparer) logLFSCompactionSkip(candidate LFSObjectCandidate, err error) {
	if p.Log != nil {
		p.Log.Warn("LFS cache CoW skipped", "path", candidate.Path, "oid", candidate.Pointer.OID, "error", err)
	}
}

func (p *Preparer) logLFSCompactionFailure(candidate LFSObjectCandidate, err error) {
	if p.Log != nil {
		p.Log.Warn("LFS cache CoW failed", "path", candidate.Path, "oid", candidate.Pointer.OID, "error", err)
	}
}
