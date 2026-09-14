package workspace

import (
	"context"
	"os"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

func (p *Preparer) compactLFSObjectsWithRoots(context.Context, discovery.Repository, string, []LFSObjectCandidate) (LFSCompactionResult, error) {
	return LFSCompactionResult{}, nil
}

func compactLFSObject(context.Context, *os.Root, *os.Root, LFSObjectCandidate) (bool, int64, error) {
	return false, 0, nil
}
