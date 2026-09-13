package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/pool"
	"github.com/HappyOnigiri/WX/internal/workspace"
)

// submoduleSharingFindings は登録済み workspace の要求 OIDごとに、local module の共有条件を診断する。
// source module が無い submodule は準備も診断も対象外で、promisor の欠落だけを問題として扱う。
func (m *Manager) submoduleSharingFindings(ctx context.Context) []diag.Finding {
	roots, err := m.store.WorkspaceRoots(ctx)
	if err != nil {
		return []diag.Finding{stateQueryProblem(diag.CheckSubmoduleSharing,
			"the registered workspaces for submodule sharing could not be read",
			message("diag.submodule_sharing.workspaces_unreadable"), "", err)}
	}
	cfg := m.Config()
	discoverer := discovery.Discoverer{Git: m.git, Config: cfg}
	preparer := &workspace.Preparer{Git: m.git}
	findings := make([]diag.Finding, 0)
	checked := 0
	for _, root := range roots {
		workspaceRecord, resolveErr := m.resolveRegisteredWorkspace(ctx, root, &discoverer)
		if resolveErr != nil {
			findings = append(findings, submoduleSharingWorkspaceProblem(root, resolveErr))
			continue
		}
		resolved, resolveErr := pool.ResolveBranches(ctx, m.git, workspaceRecord, nil)
		if resolveErr != nil {
			findings = append(findings, submoduleSharingBranchProblem(root, resolveErr))
			continue
		}
		for _, item := range resolved {
			if !submodulePreparationEnabled(cfg, workspaceRecord, item.Repository) {
				continue
			}
			modules, modulesErr := preparer.SubmodulesAtRevision(ctx, string(item.Repository.MainPath), item.OID)
			if modulesErr != nil {
				findings = append(findings, submoduleSharingRepositoryProblem(item.Repository, modulesErr))
				continue
			}
			for _, module := range modules {
				source := filepath.Join(string(item.Repository.CommonDir), "modules", module.Name)
				if !domain.IsWithin(filepath.Join(string(item.Repository.CommonDir), "modules"), source) {
					findings = append(findings, submoduleSharingRepositoryProblem(item.Repository,
						fmt.Errorf("submodule %s resolves outside the source repository module directory", module.Name)))
					continue
				}
				if !osIsDirectory(source) {
					continue
				}
				info, inspectErr := workspace.InspectSubmodule(ctx, m.git, source, module.OID)
				if inspectErr != nil {
					if errors.Is(inspectErr, fs.ErrNotExist) {
						continue
					}
					findings = append(findings, submoduleSharingModuleProblem(source, inspectErr))
					continue
				}
				// origin が無い module は準備側が clone を省略するため、共有条件を表示しない。
				if info.OriginURL == "" {
					continue
				}
				checked++
				switch info.Status() {
				case workspace.SubmoduleSharingPromisorMissing:
					findings = append(findings, submodulePromisorMissingFinding(source, module.Name, module.OID))
				case workspace.SubmoduleSharingPromisor:
					findings = append(findings, submodulePromisorFinding(source, module.Name))
				case workspace.SubmoduleSharingShallow:
					findings = append(findings, submoduleShallowFinding(source, module.Name))
				case workspace.SubmoduleSharingAvailable, workspace.SubmoduleSharingObjectMissing:
					// 通常 module の完全性と欠落は既存の prepare skip 診断に任せる。
				}
			}
		}
	}
	return append(findings, diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityOK,
		Summary: "submodule object sharing conditions were checked",
		Details: []string{strconv.Itoa(checked) + " local submodule module(s) checked"},
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.checked"),
			Details: []i18n.Message{message("diag.detail.submodule_sharing_checked", "Count", checked)},
		},
	})
}

func submodulePreparationEnabled(cfg config.Config, w discovery.Workspace, repo discovery.Repository) bool {
	profile := cfg.RepositoryFor(string(w.Root), repo.RelativePath, string(repo.MainPath))
	if profile.Submodules != nil {
		return *profile.Submodules
	}
	enabled, _ := cfg.SubmodulesForWorkspace(string(w.Root))
	return enabled
}

func submoduleSharingWorkspaceProblem(root string, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityProblem,
		Summary: "a registered workspace could not be checked for submodule object sharing",
		Target:  root, Cause: err.Error(), Action: "check the registered workspace and run wx doctor again",
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.workspace_unchecked"),
			Action:  message("diag.action.submodule_sharing_workspace"),
		},
	}
}

func submoduleSharingBranchProblem(root string, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityProblem,
		Summary: "the branches of a registered workspace could not be resolved for submodule object sharing",
		Target:  root, Cause: err.Error(), Action: "fix the repository refs or default branch, then run wx doctor again",
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.branches_unresolved"),
			Action:  message("diag.action.submodule_sharing_branches"),
		},
	}
}

func submoduleSharingRepositoryProblem(repo discovery.Repository, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityProblem,
		Summary: "a repository could not be checked for submodule object sharing",
		Target:  string(repo.MainPath), Cause: err.Error(), Action: "check that the repository is readable, then run wx doctor again",
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.repository_unreadable"),
			Action:  message("diag.action.submodule_sharing_repository"),
		},
	}
}

func submoduleSharingModuleProblem(source string, err error) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityProblem,
		Summary: "a submodule local module could not be checked for object sharing",
		Target:  source, Cause: err.Error(), Action: "check that the local module is readable, then run wx doctor again",
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.module_unreadable"),
			Action:  message("diag.action.submodule_sharing_module"),
		},
	}
}

func submodulePromisorMissingFinding(source, name, oid string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityProblem,
		Summary: "a promisor submodule is missing the requested object",
		Target:  source,
		Cause:   fmt.Sprintf("the local module for %s does not contain requested gitlink %s; preparing it would fail after writing the worktree", name, oid),
		Action:  fmt.Sprintf("fetch object %s into %s without a filter, or reclone that module without a partial clone, then run wx doctor again", oid, source),
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.promisor_missing"),
			Cause:   message("diag.submodule_sharing.promisor_missing_cause", "Name", name, "OID", oid),
			Action:  message("diag.action.submodule_sharing_promisor_missing", "OID", oid, "Path", source),
		},
	}
}

func submodulePromisorFinding(source, name string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityInfo,
		Summary: "a promisor submodule cannot carry its lazy-fetch configuration into a slot",
		Target:  source,
		Cause:   fmt.Sprintf("the local module for %s has promisor objects; wx can prepare the current gitlink, but a later missing object cannot be fetched from the slot", name),
		Action:  "ensure every gitlink object needed by wx is present in the source module before leasing a worktree",
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.promisor"),
			Cause:   message("diag.submodule_sharing.promisor_cause", "Name", name),
			Action:  message("diag.action.submodule_sharing_promisor"),
		},
	}
}

func submoduleShallowFinding(source, name string) diag.Finding {
	return diag.Finding{
		Check: diag.CheckSubmoduleSharing, Severity: diag.SeverityInfo,
		Summary: "a shallow submodule cannot share its objects with a slot",
		Target:  source,
		Cause:   fmt.Sprintf("the local module for %s is shallow, so local clone optimization is disabled and each prepared slot copies its module objects outside wx usage measurements", name),
		Action:  fmt.Sprintf("run git -C %s fetch --unshallow if object sharing is required", source),
		Messages: diag.FindingMessages{
			Summary: message("diag.submodule_sharing.shallow"),
			Cause:   message("diag.submodule_sharing.shallow_cause", "Name", name),
			Action:  message("diag.action.submodule_sharing_shallow", "Path", source),
		},
	}
}

// osIsDirectory は doctor の source module 欠落を準備側と同じく無視するための小さな境界である。
func osIsDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
