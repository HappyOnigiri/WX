package diag

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Severity は診断 1 件の扱いである。検出側がこの値を決め、表示側は文面から重大さを判定しない。
type Severity string

const (
	// SeverityProblem は利用者の対処が必要な故障で、通常表示に出す。
	SeverityProblem Severity = "problem"
	// SeverityUnchecked は前提の故障で実施できなかった検査である。原因を表示済みなら通常表示では省く。
	SeverityUnchecked Severity = "unchecked"
	// SeverityInfo は対処を要さない参考情報で、詳細表示にだけ出す。
	SeverityInfo Severity = "info"
	// SeverityOK は正常確認で、詳細表示にだけ出す。
	SeverityOK Severity = "ok"
)

// Finding は 1 件の診断結果である。
// Summary は失敗した機能・処理、Target は影響を受ける対象、Cause は失敗した操作と根本の理由、Action は原因に対応する手順を表す。
// Cause と Action は問題では必須で、`-v` を必要としない。Details は詳細表示にだけ出す補足である。
type Finding struct {
	Check    string   `json:"check"`
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	Target   string   `json:"target,omitempty"`
	Cause    string   `json:"cause,omitempty"`
	Action   string   `json:"action,omitempty"`
	Details  []string `json:"details,omitempty"`
	// DependsOn は未検査の原因になった検査の名前である。
	// その検査の問題を表示済みなら、同じ故障を独立した問題として重複表示しない。
	DependsOn string `json:"depends_on,omitempty"`
}

// Reply は `wx doctor` の応答である。findings は `-v` に左右されず全件を持つ。
type Reply struct {
	SchemaVersion   int       `json:"schema_version"`
	DBSchemaVersion int       `json:"db_schema_version"`
	Degraded        bool      `json:"degraded,omitempty"`
	Findings        []Finding `json:"findings"`
}

// NoProblems は問題が無いときに出す 1 行である。
const NoProblems = "No errors found."

// FindingsSchemaVersion は findings を返すようになった JSON schema 版である。
// これより古い daemon は checks map を返すため、CLI は診断結果を得られない。
const FindingsSchemaVersion = 17

// severityRank は詳細表示の並び順で、問題から順に読ませる。
func severityRank(severity Severity) int {
	switch severity {
	case SeverityProblem:
		return 0
	case SeverityUnchecked:
		return 1
	case SeverityInfo:
		return 2
	case SeverityOK:
		return 3
	}
	return 4
}

// severityLabel は finding の 1 行目のラベルである。
func severityLabel(severity Severity) string {
	switch severity {
	case SeverityProblem:
		return "error"
	case SeverityUnchecked:
		return "unchecked"
	case SeverityInfo:
		return "note"
	case SeverityOK:
		return "ok"
	}
	return "unknown"
}

// ExitCode は診断結果に対応する終了コードを返す。
// 問題と、必要な検査を実施できなかった未検査はどちらも 1 とする。引数不正の 2 は呼び出し側が決める。
func ExitCode(reply Reply) int {
	for _, finding := range reply.Findings {
		if finding.Severity == SeverityProblem || finding.Severity == SeverityUnchecked {
			return 1
		}
	}
	return 0
}

// visible は表示する finding を並べ替えて返す。
// 通常表示は問題と、原因を表示していない未検査だけにする。詳細表示は正常・参考も含めた全件を出す。
func visible(reply Reply, verbose bool) []Finding {
	shown := map[string]bool{}
	for _, finding := range reply.Findings {
		if finding.Severity == SeverityProblem {
			shown[finding.Check] = true
		}
	}
	out := make([]Finding, 0, len(reply.Findings))
	for _, finding := range reply.Findings {
		switch {
		case verbose:
		case finding.Severity == SeverityProblem:
		case finding.Severity == SeverityUnchecked && !shown[finding.DependsOn]:
		default:
			continue
		}
		out = append(out, finding)
	}
	sort.SliceStable(out, func(i, j int) bool { return severityRank(out[i].Severity) < severityRank(out[j].Severity) })
	return out
}

// Render は診断結果を人間向けに出力する。
// 問題が無ければ 1 行だけを出し、詳細表示ではその後に正常・参考・未検査を続ける。
func Render(w io.Writer, reply Reply, verbose bool) {
	findings := visible(reply, verbose)
	if !hasReportable(findings) {
		// 表示は stdout に出す。書込み失敗は対処できず、command の終了コードも変えない。
		_, _ = fmt.Fprintln(w, NoProblems)
	}
	for index, finding := range findings {
		if index > 0 {
			_, _ = fmt.Fprintln(w)
		}
		renderFinding(w, finding, verbose)
	}
}

// hasReportable は「問題なし」の 1 行を出すかを決める。
// 未検査は問題の有無を確認できていないため、これがあるときは正常と言わない。
func hasReportable(findings []Finding) bool {
	for _, finding := range findings {
		if finding.Severity == SeverityProblem || finding.Severity == SeverityUnchecked {
			return true
		}
	}
	return false
}

// renderFinding は 1 件を「エラー内容・対象・原因・対処方法」の順で出す。
// 空の項目は行を作らず、原因を特定できていない場合は検出側がその旨を Cause に入れる。
func renderFinding(w io.Writer, finding Finding, verbose bool) {
	rows := [][2]string{
		{severityLabel(finding.Severity), finding.Summary},
		{"check", finding.Check},
		{"target", finding.Target},
		{"cause", finding.Cause},
		{"action", finding.Action},
	}
	if verbose {
		for _, detail := range finding.Details {
			rows = append(rows, [2]string{"detail", detail})
		}
	}
	width := 0
	for _, row := range rows {
		if row[1] != "" && len(row[0]) > width {
			width = len(row[0])
		}
	}
	for _, row := range rows {
		if row[1] == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "%-*s %s\n", width+1, row[0]+":", singleLine(row[1]))
	}
}

// singleLine は改行を含む値を 1 行へ畳む。項目の境目を崩さずに、下位のエラー出力をそのまま載せるためである。
func singleLine(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.Join(strings.Split(strings.TrimRight(value, "\n"), "\n"), " / ")
}
