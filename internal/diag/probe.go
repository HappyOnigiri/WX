package diag

import "github.com/HappyOnigiri/WX/internal/i18n"

// 実地検査が返す検査名。findings と同じ名前空間なので `--json` の識別子としてそのまま使える。
const (
	CheckProbe          = "probe"
	CheckProbeSubmodule = "probe_submodule"
	CheckProbeTracked   = "probe_tracked_changes"
	CheckProbeSharing   = "probe_sharing"
	CheckPrepareOutput  = "prepare_output"
)

// Probe.Usage の値。ディスク使用量が実測なのか、測定を待てなかったのかを区別する。
const (
	// ProbeUsageMeasured は Repositories が実測であることを表す。
	ProbeUsageMeasured = "measured"
	// ProbeUsagePending は daemon の測定が待ち時間内に終わらなかったことを表す。Repositories は空になる。
	ProbeUsagePending = "pending"
	// ProbeUsageUnavailable は slot の使用量を引けなかったことを表す。
	ProbeUsageUnavailable = "unavailable"
)

// Probe は workspace 1 個を実際に準備して得た計測値である。
// 失敗は Finding が報告するので、ここには「速い・遅い」「大きい・小さい」の判断を持ち込まない。
type Probe struct {
	Workspace string `json:"workspace"`
	SlotID    string `json:"slot_id,omitempty"`
	Path      string `json:"path,omitempty"`
	// LeaseMS は貸出要求が返るまで、EarlyReadyMS と FullReadyMS は同じ起点からそれぞれの到達までである。
	LeaseMS      int64 `json:"lease_ms"`
	EarlyReadyMS int64 `json:"early_ready_ms"`
	FullReadyMS  int64 `json:"full_ready_ms"`
	// Usage は Repositories の由来である。測定が間に合わなかった回は pending、測れない platform では unavailable になる。
	Usage        string            `json:"usage"`
	Repositories []ProbeRepository `json:"repositories,omitempty"`
	// Phases は daemon が保持している区間内訳である。daemon 再起動などで引けない回は PhasesUnavailable が立つ。
	Phases            []ProbePhase          `json:"phases,omitempty"`
	PhasesUnavailable bool                  `json:"phases_unavailable,omitempty"`
	Submodules        *ProbeSubmoduleReport `json:"submodules,omitempty"`
	Error             string                `json:"error,omitempty"`
	// ErrorMessage は Error を表示言語で組み直すための解決前の表示文である。
	// JSON の形は変えないため出力せず、Error が英語の契約を持ち続ける。
	ErrorMessage i18n.Message `json:"-"`
}

// ProbeRepository は準備した slot の repository 1 個分のディスク使用量である。
type ProbeRepository struct {
	Name           string `json:"name"`
	Files          int    `json:"files"`
	AllocatedBytes int64  `json:"allocated_bytes"`
	SharedBytes    int64  `json:"shared_bytes"`
	ExclusiveBytes int64  `json:"exclusive_bytes"`
}

// ProbePhase は準備の1区間の名前・回数・所要時間である。
// 名前に "." を含む区間は並列 worker の合計なので、上位区間の実時間を超えることがある。
type ProbePhase struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	MS    int64  `json:"ms"`
}

// ProbeSubmoduleSummary は準備対象を repository と深さごとに集計した値である。
type ProbeSubmoduleSummary struct {
	Repository   string `json:"repository"`
	Depth        int    `json:"depth"`
	Materialized int    `json:"materialized"`
	OutOfScope   int    `json:"out_of_scope"`
	Skipped      int    `json:"skipped"`
	Unreachable  int    `json:"unreachable"`
}

// ProbeSubmoduleDetail は submodule 1 件の機械可読な結果である。
type ProbeSubmoduleDetail struct {
	Repository string `json:"repository"`
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	Action     string `json:"action"`
	Reason     string `json:"reason,omitempty"`
}

// ProbeSubmoduleReport は準備計測から doctor の応答へ移した submodule 結果である。
type ProbeSubmoduleReport struct {
	Summaries []ProbeSubmoduleSummary `json:"summaries"`
	Details   []ProbeSubmoduleDetail  `json:"details,omitempty"`
	Truncated bool                    `json:"truncated,omitempty"`
}
