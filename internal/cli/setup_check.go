package cli

import (
	"context"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

const setupCheckAgentKind = "wx-setup-check"

// RunSetupCheck は指定 workspace を通常どおり貸し出し、保存経路を保ったまま初回検査と同じ確認を行う。
func (c Client) RunSetupCheck(ctx context.Context, root string) []diag.Finding {
	if err := c.ensureDaemon(ctx); err != nil {
		return []diag.Finding{setupMeasurementFinding(root, err.Error())}
	}
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	params := rpc.ResolveAndLeaseParams{
		Agent: setupCheckAgentKind, ClientPID: 0, CWD: root, ForceWorktree: c.forceWorktree,
		LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: ownerID, LeaseOwnerToken: ownerToken,
		Language: c.Config.LanguageForRPC(),
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancelLease()
	var lease daemon.Lease
	if err := c.RPC.Call(leaseCtx, "ResolveAndLease", params, &lease); err != nil {
		return []diag.Finding{probeLeaseProblem(root, newProbeStage(probeStageLease, err))}
	}
	defer c.releaseLeaseToken(lease, "setup-check")
	if !lease.Ready {
		readinessTimeout := leaseReadinessTimeout(c.Config, lease)
		waitCtx, cancel := context.WithTimeout(ctx, readinessTimeout)
		err := c.RPC.Call(waitCtx, "WaitReady", map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(readinessTimeout.Milliseconds())}, nil)
		cancel()
		if err != nil {
			return []diag.Finding{probePrepareProblem(root, lease.Path, newProbeStage(probeStageFullReady, err))}
		}
	}
	_, findings := c.inspectLeasedWorkspace(ctx, lease.SourceWorkspace, lease.SessionID, lease.Path, true)
	if len(lease.SetupCheckRepositories) > 0 {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = recordInitialSetup(lease.SetupCheckRepositories, lease.SourceWorkspace, now, "")
	}
	return findings
}
