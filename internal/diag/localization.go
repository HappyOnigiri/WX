package diag

import (
	"fmt"
	"io"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

// Resolve は finding が持つ message ID を lang で解決し、string フィールドへ書き戻す。
// Messages を持たない finding は、daemon が解決済みで返したものか、外部コマンドの
// 出力など訳さない本文なので、そのまま通す。
func Resolve(reply Reply, lang i18n.Language) Reply {
	localizer := i18n.New(string(lang))
	out := reply
	out.Findings = make([]Finding, len(reply.Findings))
	for index, finding := range reply.Findings {
		out.Findings[index] = resolveFinding(localizer, finding)
	}
	return out
}

func resolveFinding(localizer *i18n.Localizer, finding Finding) Finding {
	resolved := finding
	if text := localizer.Message(finding.Messages.Summary); text != "" {
		resolved.Summary = text
	}
	if text := localizer.Message(finding.Messages.Cause); text != "" {
		resolved.Cause = text
	}
	if text := localizer.Message(finding.Messages.Action); text != "" {
		resolved.Action = text
	}
	// Details は呼び出し側の slice を共有しないよう、訳す要素がなくても複製する。
	if finding.Details != nil {
		resolved.Details = make([]string, len(finding.Details))
		copy(resolved.Details, finding.Details)
		for index, message := range finding.Messages.Details {
			if index >= len(resolved.Details) {
				break
			}
			if text := localizer.Message(message); text != "" {
				resolved.Details[index] = text
			}
		}
	}
	return resolved
}

// RenderLanguage は doctor の人間向け表示を書き出す。本文は Resolve が解決済みで、
// ここで訳すのは行のラベルと severity だけである。check・target とその値、
// 外部コマンドのエラー本文は機械値・原文として残す。
func RenderLanguage(w io.Writer, reply Reply, verbose bool, lang i18n.Language) {
	localizer := i18n.New(string(lang))
	findings := visible(reply, verbose)
	if !hasReportable(findings) {
		_, _ = fmt.Fprintln(w, localizer.Localize("diag.no_problems", nil))
	}
	for index, finding := range findings {
		if index > 0 {
			_, _ = fmt.Fprintln(w)
		}
		renderFindingLanguage(w, localizer, finding, verbose)
	}
}

// severityMessageIDs は 1 行目のラベルを表示言語へ解決するための ID である。
// JSON へ出る severity の値そのものは Severity 定数であり、ここでは表示だけを扱う。
var severityMessageIDs = map[string]string{
	"error":     "diag.severity.error",
	"unchecked": "diag.severity.unchecked",
	"note":      "diag.severity.note",
	"ok":        "diag.severity.ok",
	"unknown":   "diag.severity.unknown",
}

// rowLabelIDs は finding の各行の見出しである。
var rowLabelIDs = map[string]string{
	"check":  "diag.label.check",
	"target": "diag.label.target",
	"cause":  "diag.label.cause",
	"action": "diag.label.action",
	"detail": "diag.label.detail",
}

func renderFindingLanguage(w io.Writer, localizer *i18n.Localizer, finding Finding, verbose bool) {
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
		_, _ = fmt.Fprintf(w, "%-*s %s\n", width+1, diagnosticLabel(localizer, row[0])+":", singleLine(row[1]))
	}
}

// diagnosticLabel は severity ラベルと行の見出しを表示言語へ解決する。
// 幅の計算は英語のラベル長で行うため、訳語の幅はここでは扱わない。
func diagnosticLabel(localizer *i18n.Localizer, value string) string {
	if id, known := severityMessageIDs[value]; known {
		return localizer.Localize(id, nil)
	}
	if id, known := rowLabelIDs[value]; known {
		return localizer.Localize(id, nil)
	}
	return value
}
