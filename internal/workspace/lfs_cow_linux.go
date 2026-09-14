package workspace

import (
	"context"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func (p *Preparer) compactLFSObjectsWithRoots(context.Context, discovery.Repository, string, []LFSObjectCandidate) (LFSCompactionResult, error) {
	return LFSCompactionResult{}, nil
}
