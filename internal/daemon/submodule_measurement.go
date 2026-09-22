package daemon

import (
	"cmp"
	"sort"

	"github.com/HappyOnigiri/WX/internal/workspace"
)

// PrepareSubmoduleSummary は repository と深さごとの submodule 結果の集計である。
type PrepareSubmoduleSummary struct {
	Repository   string `json:"repository"`
	Depth        int    `json:"depth"`
	Materialized int    `json:"materialized"`
	OutOfScope   int    `json:"out_of_scope"`
	Skipped      int    `json:"skipped"`
	Unreachable  int    `json:"unreachable"`
}

// PrepareSubmoduleDetail は準備した submodule の明細である。
// JSON では成功・範囲外の対象も上限まで含め、集計との照合を可能にする。
type PrepareSubmoduleDetail struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
}

// PrepareSubmoduleReport は準備 1 回が扱った submodule の表示用結果である。
type PrepareSubmoduleReport struct {
	Summaries []PrepareSubmoduleSummary `json:"summaries"`
	Details   []PrepareSubmoduleDetail  `json:"details,omitempty"`
	Truncated bool                      `json:"truncated,omitempty"`
}

// SubmoduleSummary は準備 submodule 集計の互換的な短縮名である。
type SubmoduleSummary = PrepareSubmoduleSummary

// SubmoduleDetail は準備 submodule 明細の互換的な短縮名である。
type SubmoduleDetail = PrepareSubmoduleDetail

// SubmoduleReport は準備 submodule 報告の互換的な短縮名である。
type SubmoduleReport = PrepareSubmoduleReport

const prepareSubmoduleDetailLimit = 256

// recordPrepareSubmodules は workspace の結果を表示用の集計へ畳む。
// 入力順は repository ごとの並列準備に依存するため、応答へ載せる前に順序を固定する。
func (m *Manager) recordPrepareSubmodules(outcomes *workspace.SubmoduleOutcomes) *PrepareSubmoduleReport {
	repositories, items := outcomes.Snapshot()
	if len(repositories) == 0 && len(items) == 0 {
		return nil
	}
	type key struct {
		repository string
		depth      int
	}
	summaryByKey := map[key]*PrepareSubmoduleSummary{}
	for _, repository := range repositories {
		summaryByKey[key{repository: repository, depth: 1}] = &PrepareSubmoduleSummary{Repository: repository, Depth: 1}
	}
	for _, item := range items {
		repository := item.Repository
		if repository == "" && len(repositories) == 1 {
			repository = repositories[0]
		}
		k := key{repository: repository, depth: item.Depth}
		summary := summaryByKey[k]
		if summary == nil {
			summary = &PrepareSubmoduleSummary{Repository: repository, Depth: item.Depth}
			summaryByKey[k] = summary
		}
		switch item.Action {
		case workspace.SubmoduleActionMaterialized:
			summary.Materialized++
		case workspace.SubmoduleActionOutOfScope:
			summary.OutOfScope++
		case workspace.SubmoduleActionSkipped:
			summary.Skipped++
		case workspace.SubmoduleActionUnreachable:
			summary.Unreachable++
		}
	}
	summaries := make([]PrepareSubmoduleSummary, 0, len(summaryByKey))
	for _, summary := range summaryByKey {
		summaries = append(summaries, *summary)
	}
	sort.Slice(summaries, func(i, j int) bool {
		if order := cmp.Compare(summaries[i].Repository, summaries[j].Repository); order != 0 {
			return order == -1
		}
		return cmp.Compare(summaries[i].Depth, summaries[j].Depth) == -1
	})
	report := &PrepareSubmoduleReport{Summaries: summaries}
	orderedItems := append([]workspace.SubmoduleOutcome(nil), items...)
	if len(repositories) == 1 {
		for index := range orderedItems {
			if orderedItems[index].Repository == "" {
				orderedItems[index].Repository = repositories[0]
			}
		}
	}
	sort.SliceStable(orderedItems, func(i, j int) bool {
		priority := func(action workspace.SubmoduleAction) int {
			switch action {
			case workspace.SubmoduleActionSkipped:
				return 0
			case workspace.SubmoduleActionUnreachable:
				return 1
			case workspace.SubmoduleActionOutOfScope:
				return 2
			default:
				return 3
			}
		}
		left, right := priority(orderedItems[i].Action), priority(orderedItems[j].Action)
		if left != right {
			return cmp.Compare(left, right) == -1
		}
		return outcomeLess(orderedItems[i], orderedItems[j])
	})
	for index, item := range orderedItems {
		if index >= prepareSubmoduleDetailLimit {
			report.Truncated = true
			break
		}
		report.Details = append(report.Details, PrepareSubmoduleDetail{
			Repository: item.Repository, Path: item.Path, Depth: item.Depth,
			Action: item.Action, Reason: item.Reason,
		})
	}
	if m.log != nil {
		materialized, outOfScope, skipped, unreachable := 0, 0, 0, 0
		for _, summary := range summaries {
			materialized += summary.Materialized
			outOfScope += summary.OutOfScope
			skipped += summary.Skipped
			unreachable += summary.Unreachable
		}
		m.log.Info("submodule preparation range", "repositories", len(repositories), "materialized", materialized, "out_of_scope", outOfScope, "skipped", skipped, "unreachable", unreachable, "truncated", report.Truncated)
	}
	return report
}

func outcomeLess(a, b workspace.SubmoduleOutcome) bool {
	// 上位キーが異なるときは順序を確定し、等値のときだけ次のキーへ進む。
	if a.Repository < b.Repository {
		return true
	}
	if a.Repository > b.Repository {
		return false
	}
	if a.Depth < b.Depth {
		return true
	}
	if a.Depth > b.Depth {
		return false
	}
	if a.Path < b.Path {
		return true
	}
	if a.Path > b.Path {
		return false
	}
	if a.Action < b.Action {
		return true
	}
	if a.Action > b.Action {
		return false
	}
	return a.Reason < b.Reason
}
