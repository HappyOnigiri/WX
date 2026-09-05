package daemon

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/HappyOnigiri/WX/internal/archive"
)

// PruneKeptRef は削除しなかった孤児 ref と、その理由になった object 数。
type PruneKeptRef struct {
	Repository         string `json:"repository"`
	Ref                string `json:"ref"`
	UnreachableObjects int    `json:"unreachable_objects"`
}

// PruneResult は `wx prune` の結果。Kept は失敗ではなく、既定の安全側の判断である。
type PruneResult struct {
	DryRun   bool           `json:"dry_run"`
	All      bool           `json:"all"`
	Deleted  int            `json:"deleted"`
	Kept     int            `json:"kept"`
	KeptRefs []PruneKeptRef `json:"kept_refs"`
	Errors   []string       `json:"errors"`
}

// PruneRecoveryRefs は現行 DB が説明しない recovery ref を削除する。
// 対象はその場の検出結果の unknown_refs に限り、all=true のときだけ到達可能性を証明できなかった ref も消す（その作業内容は失われる）。
// commentlint:allow-long -- 破棄が起きる条件を呼び出し側が取り違えないため
func (m *Manager) PruneRecoveryRefs(ctx context.Context, all, dryRun bool) (PruneResult, error) {
	result := PruneResult{DryRun: dryRun, All: all, KeptRefs: []PruneKeptRef{}, Errors: []string{}}
	diagnostics := m.artifactDiagnostics(ctx)
	if diagnosticErrors, _ := diagnostics["errors"].([]string); len(diagnosticErrors) > 0 {
		result.Errors = append(result.Errors, diagnosticErrors...)
	}
	items, _ := diagnostics["unknown_refs"].([]string)
	byRepository := map[string][]string{}
	for _, item := range items {
		// path は `<repository_id>:<ref>`。repository_id は 32 hex でコロンを含まないため、最初のコロンで切る。
		repositoryID, ref, ok := strings.Cut(item, ":")
		if !ok {
			result.Errors = append(result.Errors, fmt.Sprintf("parse quarantined recovery ref %q", item))
			continue
		}
		byRepository[repositoryID] = append(byRepository[repositoryID], ref)
	}
	repositoryIDs := make([]string, 0, len(byRepository))
	for repositoryID := range byRepository {
		repositoryIDs = append(repositoryIDs, repositoryID)
	}
	sort.Strings(repositoryIDs)
	manager := archive.Manager{Git: m.git}
	for _, repositoryID := range repositoryIDs {
		if err := m.pruneRepositoryRefs(ctx, manager, repositoryID, byRepository[repositoryID], all, dryRun, &result); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("prune recovery refs for %s: %v", repositoryID, err))
		}
	}
	return result, nil
}

func (m *Manager) pruneRepositoryRefs(ctx context.Context, manager archive.Manager, repositoryID string, candidates []string, all, dryRun bool, result *PruneResult) error {
	repository, err := m.store.Repository(ctx, repositoryID)
	if err != nil {
		return err
	}
	// 検出から削除までの間に snapshot が公開されうるので、期待値を引き直して現行 DB が求める ref を候補から必ず外す。
	expectations, err := m.store.RecoveryRefExpectations(ctx, repositoryID)
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, expectation := range expectations {
		expected[expectation.Ref] = true
	}
	filtered := make([]string, 0, len(candidates))
	for _, ref := range candidates {
		if !expected[ref] {
			filtered = append(filtered, ref)
		}
	}
	safe, unsafe, err := manager.ClassifyOrphanRefs(ctx, repository, filtered)
	if err != nil {
		return err
	}
	deletable := safe
	if all {
		for _, ref := range unsafe {
			deletable = append(deletable, ref.OrphanRef)
		}
		unsafe = nil
	}
	for _, ref := range unsafe {
		result.Kept++
		result.KeptRefs = append(result.KeptRefs, PruneKeptRef{Repository: repositoryID, Ref: ref.Ref, UnreachableObjects: ref.UnreachableObjects})
	}
	if dryRun {
		result.Deleted += len(deletable)
		return nil
	}
	if err := manager.DeleteOrphanRefs(ctx, repository, deletable); err != nil {
		return err
	}
	for _, ref := range deletable {
		if err := m.store.ForgetQuarantinedArtifact(ctx, repositoryID+":"+ref.Ref); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("forget quarantine record for %s: %v", ref.Ref, err))
		}
	}
	result.Deleted += len(deletable)
	m.log.Info("pruned recovery refs not explained by the current database",
		"repository", repositoryID, "deleted", len(deletable), "kept", len(unsafe))
	return nil
}
