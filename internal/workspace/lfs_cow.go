package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
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

var (
	errLFSVerification = errors.New("LFS object verification failed")
	errLFSNotEligible  = errors.New("LFS object is not eligible for CoW")
)

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
	owner, relative, closeOwner, err := p.openOwnedRoot(p.RootPath, filepath.Clean(worktree))
	if err != nil {
		return result, err
	}
	defer closeOwner()
	donorRoot, err := domain.OpenRootAt(owner, relative)
	if err != nil {
		return result, fmt.Errorf("open LFS worktree donor: %w", err)
	}
	defer func() { _ = donorRoot.Close() }()
	commonRoot, err := OpenPhysicalRoot(string(repo.CommonDir))
	if err != nil {
		return result, fmt.Errorf("open LFS cache root: %w", err)
	}
	defer func() { _ = commonRoot.Close() }()

	var errs []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Failed += len(candidates) - result.Replaced - result.Skipped - result.Failed
			errs = append(errs, err)
			break
		}
		replaced, bytes, err := compactLFSObject(ctx, donorRoot, commonRoot, candidate)
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
