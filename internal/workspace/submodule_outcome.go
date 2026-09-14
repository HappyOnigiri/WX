package workspace

import (
	"sort"
	"sync"
)

// SubmoduleAction は準備が submodule をどう扱ったかを表す機械向けの識別子である。
type SubmoduleAction = string

const (
	SubmoduleActionMaterialized SubmoduleAction = "materialized"
	SubmoduleActionOutOfScope   SubmoduleAction = "out_of_scope"
	SubmoduleActionSkipped      SubmoduleAction = "skipped"
	SubmoduleActionUnreachable  SubmoduleAction = "unreachable"
)

// 既存の準備コードが使う短い名前を、JSONへ出す値とは分離して公開する。
const (
	ActionMaterialized = SubmoduleActionMaterialized
	ActionOutOfScope   = SubmoduleActionOutOfScope
	ActionSkipped      = SubmoduleActionSkipped
	ActionUnreachable  = SubmoduleActionUnreachable
)

// SubmoduleReason は準備できなかった理由を表す機械向けの識別子である。
const (
	SubmoduleReasonURLMissing       = "url_missing"
	SubmoduleReasonLocalModule      = "local_module_missing"
	SubmoduleReasonObjectMissing    = "object_missing"
	SubmoduleReasonOriginMissing    = "origin_missing"
	SubmoduleReasonInspectionFailed = "inspection_failed"
	SubmoduleReasonAncestorSkipped  = "ancestor_skipped"
)

// SubmoduleOutcome は準備 1 回が扱った submodule 1 件の結果である。
// Repository は複数 repository workspace での集計対象を区別するために保持する。
type SubmoduleOutcome struct {
	Repository string `json:"repository,omitempty"`
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
}

// SubmoduleOutcomes は準備 1 回分の結果を並列に集める器である。
// nil のままでも準備結果は変わらず、記録だけが落ちる。
type SubmoduleOutcomes struct {
	mu           sync.Mutex
	repositories map[string]bool
	items        []SubmoduleOutcome
}

// SubmoduleOutcomeCollector は準備結果を集める器の別名である。
type SubmoduleOutcomeCollector = SubmoduleOutcomes

// BeginRepository は submodule 方針が有効な repository を結果へ登録する。
// submodule が宣言されていない repository も、準備対象だったことを区別するため残す。
func (o *SubmoduleOutcomes) BeginRepository(repository string) {
	if o == nil || repository == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.repositories == nil {
		o.repositories = map[string]bool{}
	}
	o.repositories[repository] = true
}

// Add は submodule の結果を 1 件加える。
func (o *SubmoduleOutcomes) Add(outcome SubmoduleOutcome) {
	if o == nil || outcome.Path == "" || outcome.Depth < 1 || outcome.Action == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.repositories == nil {
		o.repositories = map[string]bool{}
	}
	if outcome.Repository != "" {
		o.repositories[outcome.Repository] = true
	}
	o.items = append(o.items, outcome)
}

// Record は Add と同じく結果を 1 件加える。
func (o *SubmoduleOutcomes) Record(outcome SubmoduleOutcome) { o.Add(outcome) }

// Snapshot は登録された repository と結果を安定した順序で返す。
func (o *SubmoduleOutcomes) Snapshot() ([]string, []SubmoduleOutcome) {
	if o == nil {
		return nil, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	repositories := make([]string, 0, len(o.repositories))
	for repository := range o.repositories {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	items := append([]SubmoduleOutcome(nil), o.items...)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Repository != b.Repository {
			return a.Repository < b.Repository
		}
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Action != b.Action {
			return a.Action < b.Action
		}
		return a.Reason < b.Reason
	})
	return repositories, items
}

// Outcomes は登録された結果だけを返す。表示集計を必要としない caller 用の短縮形である。
func (o *SubmoduleOutcomes) Outcomes() []SubmoduleOutcome {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]SubmoduleOutcome(nil), o.items...)
}
