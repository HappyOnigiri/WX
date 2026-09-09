package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/gitx"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// probeUsageTimeout は準備した slot の使用量測定を待つ上限である。
// 測定は daemon が background で行うため、待ちきれない回は内訳なしで結果を出し、実地検査そのものは失敗させない。
const probeUsageTimeout = 30 * time.Second

// probeUsagePoll は使用量の測定完了を確かめる間隔である。
const probeUsagePoll = 500 * time.Millisecond

// probeAgentKind は実地検査の貸出を `wx slots` の AGENT 列で見分けるための種別である。
const probeAgentKind = "wx-doctor-probe"

// RunDoctorProbe は登録済みの全 workspace で実際に slot を1つ作り、準備の失敗と worktree の内容を検査する。
// 返す finding は doctor の findings へそのまま合流させ、計測値は失敗ではないので Probe として分けて返す。
// progress が nil でなければ workspace ごとの開始を書き出し、長い検査の途中経過が見えるようにする。
func (c Client) RunDoctorProbe(ctx context.Context, progress io.Writer) ([]diag.Finding, []diag.Probe) {
	if err := c.ensureDaemon(ctx); err != nil {
		return []diag.Finding{{
			Check: diag.CheckProbe, Severity: diag.SeverityUnchecked, Summary: "the workspace probe did not run",
			Cause: err.Error(), Action: "start the wx daemon, then run wx doctor --probe again", DependsOn: diag.CheckDaemon,
		}}, nil
	}
	workspaces, err := c.probeWorkspaces(ctx)
	if err != nil {
		return []diag.Finding{{
			Check: diag.CheckProbe, Severity: diag.SeverityUnchecked, Summary: "the workspace probe did not run",
			Cause:  "the registered workspaces could not be read from the daemon: " + err.Error(),
			Action: "fix the reported daemon failure, then run wx doctor --probe again", DependsOn: diag.CheckDaemon,
		}}, nil
	}
	if len(workspaces) == 0 {
		return []diag.Finding{{
			Check: diag.CheckProbe, Severity: diag.SeverityInfo, Summary: "no workspace was probed",
			Cause:  "no registered workspace uses a wx worktree, so there was nothing to prepare and check",
			Action: "no action is required; run wx new or an agent command in a repository to register one",
		}}, nil
	}
	findings, probes := []diag.Finding{}, []diag.Probe{}
	for _, root := range workspaces {
		if progress != nil {
			// 実地検査は workspace 1 個あたり数十秒かかるため、どこまで進んだかを都度出す。
			_, _ = fmt.Fprintln(progress, "probing "+root)
		}
		probe, workspaceFindings := c.probeWorkspace(ctx, root)
		probes = append(probes, probe)
		findings = append(findings, workspaceFindings...)
	}
	return findings, probes
}

// probeWorkspaces は worktree を使う登録済み workspace の root を昇順で返す。
// 方針が off の workspace は貸出そのものを断られるため、検査の対象にしない。
func (c Client) probeWorkspaces(ctx context.Context) ([]string, error) {
	callCtx, cancel := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancel()
	var status struct {
		Workspaces []struct {
			Root   string `json:"root"`
			Policy string `json:"policy"`
		} `json:"workspace_details"`
	}
	if err := c.RPC.Call(callCtx, "Status", nil, &status); err != nil {
		return nil, err
	}
	roots := make([]string, 0, len(status.Workspaces))
	for _, item := range status.Workspaces {
		if item.Root == "" || item.Policy == "off" {
			continue
		}
		roots = append(roots, item.Root)
	}
	sort.Strings(roots)
	return roots, nil
}

// probeWorkspace は workspace 1 個を cold start で準備し、その worktree を検査してから保存せずに返す。
// 途中で失敗した回も、そこまでに測れた区間と得られた原因を残す。
func (c Client) probeWorkspace(ctx context.Context, root string) (diag.Probe, []diag.Finding) {
	probe := diag.Probe{Workspace: root, Usage: diag.ProbeUsageUnavailable}
	// 前の workspace の返却・削除・補充と並走させると、測るのが準備の重さではなくなる。
	c.waitBenchIdle(ctx)
	if _, err := c.retireStandby(ctx, root); err != nil {
		probe.Error = "retire standby: " + err.Error()
		return probe, []diag.Finding{probeLeaseProblem(root, probe.Error)}
	}
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	params := rpc.ResolveAndLeaseParams{
		Agent: probeAgentKind, ClientPID: 0, CWD: root, ForceWorktree: c.forceWorktree,
		LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: ownerID, LeaseOwnerToken: ownerToken,
	}
	started := time.Now()
	leaseCtx, cancelLease := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancelLease()
	var lease daemon.Lease
	if err := c.RPC.Call(leaseCtx, "ResolveAndLease", params, &lease); err != nil {
		probe.Error = "lease: " + err.Error()
		return probe, []diag.Finding{probeLeaseProblem(root, probe.Error)}
	}
	probe.SlotID, probe.Path, probe.LeaseMS = lease.SessionID, lease.Path, time.Since(started).Milliseconds()
	defer c.releaseProbeLease(lease)
	findings := []diag.Finding{}
	if err := c.waitBenchReadiness(ctx, lease, "WaitEarlyReady"); err != nil {
		probe.Error = "early ready: " + err.Error()
		return probe, append(findings, probePrepareProblem(root, lease.Path, probe.Error))
	}
	probe.EarlyReadyMS = time.Since(started).Milliseconds()
	if err := c.waitBenchReadiness(ctx, lease, "WaitReady"); err != nil {
		probe.Error = "full ready: " + err.Error()
		return probe, append(findings, probePrepareProblem(root, lease.Path, probe.Error))
	}
	probe.FullReadyMS = time.Since(started).Milliseconds()
	measurement := c.prepareMeasurement(ctx, lease.SessionID)
	if measurement == nil {
		probe.PhasesUnavailable = true
	} else {
		for _, phase := range measurement.Phases {
			probe.Phases = append(probe.Phases, diag.ProbePhase{Name: phase.Name, Count: phase.Count, MS: phase.MS})
		}
		findings = append(findings, prepareNoticeFindings(root, measurement.Notices)...)
	}
	findings = append(findings, c.probeWorktreeFindings(ctx, root, lease.Path)...)
	slot := c.probeSlotView(ctx, lease.SessionID)
	probe.Usage, probe.Repositories = probeUsage(slot)
	return probe, append(findings, probeSharingFindings(root, lease.Path, slot)...)
}

// releaseProbeLease は検査し終えた貸出を保存せずに返す。検査用の worktree を retention 分残さないためである。
func (c Client) releaseProbeLease(lease daemon.Lease) {
	if lease.SessionID == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	params := map[string]any{"session_id": lease.SessionID, "reason": "wx-doctor-probe", "discard": true}
	if err := c.RPC.Call(releaseCtx, "ReleaseLease", params, nil); err != nil {
		fmt.Fprintln(os.Stderr, "warning: release probe lease "+lease.SessionID+":", err)
	}
}

// probeSlotView は準備した slot の使用量が測り終わるのを待って返す。
// 測定は daemon が background で行うので、待ちきれない回と引けない回はどちらも測定なしの view を返す。
func (c Client) probeSlotView(ctx context.Context, slotID string) daemon.SlotView {
	deadline := time.Now().Add(probeUsageTimeout)
	last := daemon.SlotView{}
	for {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var slots []daemon.SlotView
		err := c.RPC.Call(callCtx, "Slots", map[string]any{"all": false}, &slots)
		cancel()
		if err != nil {
			return last
		}
		for _, slot := range slots {
			if slot.SlotID != slotID && slot.SessionID != slotID {
				continue
			}
			last = slot
			if slot.Measurement != "" && slot.Measurement != daemon.MeasurementPending {
				return slot
			}
		}
		if !time.Now().Before(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(probeUsagePoll):
		}
	}
}

// probeGit は検査用の Git runner を返す。検査は読み取りだけなので詳細ログの置き場は持たせない。
func (c Client) probeGit() *gitx.Runner {
	return &gitx.Runner{Timeout: c.Config.Readiness.Timeout.Duration}
}
