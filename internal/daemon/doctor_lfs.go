package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/textfmt"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// lfsObjectFindings は登録済み workspace の要求 tree から LFS cache の
// 欠落・破損を調べる。候補の判定は source working tree の size だけで行い、
// doctor では source と cache の本文を hash しない。
func (m *Manager) lfsObjectFindings(ctx context.Context) []diag.Finding {
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckLFSObjects,
			"the registered workspaces for LFS object checks could not be read",
			message("diag.lfs_objects.workspaces_unreadable"), "", err)}
	}
	cfg := m.Config()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	findings := make([]diag.Finding, 0)
	for _, root := range roots {
		workspaceRecord, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			findings = append(findings, lfsObjectUncheckedFinding(root, resolveErr))
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if resolveErr != nil {
			findings = append(findings, lfsObjectUncheckedFinding(string(workspaceRecord.Root), resolveErr))
			continue
		}
		preparer := &workspace.Preparer{Git: m.git, Config: cfg, WorkspaceRoot: string(workspaceRecord.Root)}
		for _, item := range resolved {
			estimate, estimateErr := m.estimateCapacity(ctx, preparer, cfg, item.Repository, item.OID)
			if estimateErr != nil {
				findings = append(findings, lfsObjectUncheckedFinding(string(item.Repository.MainPath), estimateErr))
				continue
			}
			if !lfsEstimateNeedsCheck(estimate) {
				findings = append(findings, lfsObjectOKFinding(item.Repository, len(estimate.LFS)))
				continue
			}
			diagnostics, diagnosisErr := workspace.DiagnoseLFSObjects(item.Repository, estimate.LFS)
			if diagnosisErr != nil {
				findings = append(findings, lfsObjectUncheckedFinding(string(item.Repository.MainPath), diagnosisErr))
				continue
			}
			findings = append(findings, lfsObjectFinding(item.Repository, diagnostics))
		}
	}
	if len(findings) == 0 {
		findings = append(findings, diag.Finding{
			Check: diag.CheckLFSObjects, Severity: diag.SeverityOK,
			Summary:  "no registered repository needs an LFS object check",
			Messages: diag.FindingMessages{Summary: message("diag.lfs_objects.none")},
		})
	}
	return findings
}

func lfsEstimateNeedsCheck(estimate workspace.CapacityEstimate) bool {
	return len(estimate.LFS) > 0 && estimate.MissingLFSObjects > 0
}

func lfsObjectFinding(repo discovery.Repository, diagnostics workspace.LFSObjectDiagnostics) diag.Finding {
	if len(diagnostics.Objects) == 0 {
		return lfsObjectOKFinding(repo, 0)
	}
	repairable := 0
	bytes := int64(0)
	var missing []workspace.LFSObjectDiagnostic
	details := make([]string, 0, len(diagnostics.Objects))
	detailMessages := make([]i18n.Message, 0, len(diagnostics.Objects))
	for _, diagnostic := range diagnostics.Objects {
		bytes = capacityAdd(bytes, diagnostic.Object.Size)
		candidate := diagnostic.CandidatePath
		if candidate != "" {
			repairable++
			details = append(details, fmt.Sprintf("object %s (%s): size-matching source candidate %s; hash will be verified during preparation", diagnostic.Object.OID, textfmt.HumanBytes(diagnostic.Object.Size), filepath.Join(string(repo.MainPath), candidate)))
			detailMessages = append(detailMessages, message("diag.detail.lfs_candidate", "OID", diagnostic.Object.OID, "Size", textfmt.HumanBytes(diagnostic.Object.Size), "Path", filepath.Join(string(repo.MainPath), candidate)))
			continue
		}
		missing = append(missing, diagnostic)
		details = append(details, fmt.Sprintf("object %s (%s): no size-matching source candidate", diagnostic.Object.OID, textfmt.HumanBytes(diagnostic.Object.Size)))
		detailMessages = append(detailMessages, message("diag.detail.lfs_missing", "OID", diagnostic.Object.OID, "Size", textfmt.HumanBytes(diagnostic.Object.Size)))
	}
	if len(missing) == 0 {
		return diag.Finding{
			Check: diag.CheckLFSObjects, Severity: diag.SeverityInfo, Target: string(repo.MainPath),
			Summary: "LFS cache objects are missing but have size-matching source candidates",
			Cause:   fmt.Sprintf("%d LFS object(s), %s total, are absent or have the wrong cache size; the source candidates have only been checked by size", repairable, textfmt.HumanBytes(bytes)),
			Action:  "the next worktree preparation will hash and install these candidates when possible",
			Details: details,
			Messages: diag.FindingMessages{
				Summary: message("diag.lfs_objects.repairable"),
				Cause:   message("diag.lfs_objects.repairable_cause", "Count", repairable, "Bytes", textfmt.HumanBytes(bytes)),
				Action:  message("diag.lfs_objects.repairable_action"),
				Details: detailMessages,
			},
		}
	}
	return diag.Finding{
		Check: diag.CheckLFSObjects, Severity: diag.SeverityProblem, Target: string(repo.MainPath),
		Summary: "LFS cache objects are missing and cannot be repaired from the source worktree",
		Cause:   fmt.Sprintf("%d of %d missing or corrupt LFS object(s) have no source file whose size matches the pointer", len(missing), len(diagnostics.Objects)),
		Action:  fmt.Sprintf("run git lfs fetch in %s to restore the missing objects, then run wx doctor again", repo.MainPath),
		Details: details,
		Messages: diag.FindingMessages{
			Summary: message("diag.lfs_objects.missing"),
			Cause:   message("diag.lfs_objects.missing_cause", "Missing", len(missing), "Count", len(diagnostics.Objects)),
			Action:  message("diag.lfs_objects.missing_action", "Path", string(repo.MainPath)),
			Details: detailMessages,
		},
	}
}

func lfsObjectOKFinding(repo discovery.Repository, count int) diag.Finding {
	return diag.Finding{
		Check: diag.CheckLFSObjects, Severity: diag.SeverityOK, Target: string(repo.MainPath),
		Summary: "the repository LFS cache objects are present with the expected sizes",
		Details: []string{strconv.Itoa(count) + " LFS object(s) checked"},
		Messages: diag.FindingMessages{
			Summary: message("diag.lfs_objects.ok"),
			Details: []i18n.Message{message("diag.detail.lfs_checked", "Count", count)},
		},
	}
}

func lfsObjectUncheckedFinding(target string, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckLFSObjects, Severity: diag.SeverityUnchecked, Target: target,
		Summary: "the LFS object check could not be completed", Cause: err.Error(),
		Action:    "fix the reported workspace or repository problem, then run wx doctor again",
		DependsOn: diag.CheckPrepareCapacity,
		Messages: diag.FindingMessages{
			Summary: message("diag.lfs_objects.unchecked"),
			Action:  message("diag.lfs_objects.unchecked_action"),
		},
	}
}
