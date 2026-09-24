package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/diag"
	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	"github.com/HappyOnigiri/WorktreeX/internal/textfmt"
)

// doctorProbeHint は静的検査だけを終えたときに、何をまだ見ていないかを伝える 1 行である。
const doctorProbeHint = "Nothing above was checked by preparing a worktree; run wx doctor --probe to prepare one in each registered workspace and inspect it."

// printDoctorProbes は実地検査が測った値を workspace ごとのブロックで出す。
// ここに出るのはすべて計測値で、良し悪しの判定は finding が持つ。
func printDoctorProbes(w io.Writer, probes []diag.Probe, verbose bool) {
	printDoctorProbesLanguage(w, probes, verbose, i18n.English)
}

func printDoctorProbesLanguage(w io.Writer, probes []diag.Probe, verbose bool, lang i18n.Language) {
	localizer := i18n.New(string(lang))
	probeLabel := localizer.Localize("wx.probe.label", nil)
	errorLabel := localizer.Localize("wx.probe.error", nil)
	leaseLabel := localizer.Localize("wx.probe.lease", nil)
	earlyLabel := localizer.Localize("wx.probe.early_ready", nil)
	fullLabel := localizer.Localize("wx.probe.full_ready", nil)
	prepareLabel := localizer.Localize("wx.probe.prepare_job", nil)
	diskLabel := localizer.Localize("wx.probe.disk", nil)
	for _, probe := range probes {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, probeLabel+" "+probe.Workspace)
		if probe.Error != "" {
			_, _ = fmt.Fprintf(w, "  %-13s %s\n", errorLabel, probeErrorText(localizer, probe))
		}
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", leaseLabel, formatProbeDuration(probe.LeaseMS))
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", earlyLabel, formatProbeDuration(probe.EarlyReadyMS))
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", fullLabel, formatProbeDuration(probe.FullReadyMS))
		printProbeUsageLanguage(w, probe, localizer, diskLabel)
		if probe.PhasesUnavailable {
			// 計測は daemon のメモリにしか残らないため、引けなかったことを黙って内訳なしにしない。
			_, _ = fmt.Fprintf(w, "  %-13s %s\n", prepareLabel, localizer.Localize("wx.probe.breakdown_unavailable", nil))
		}
		if verbose {
			printProbePhases(w, probe.Phases)
			printProbeSubmodulesLanguage(w, probe.Submodules, localizer)
		}
	}
}

func printProbeSubmodulesLanguage(w io.Writer, report *diag.ProbeSubmoduleReport, localizer *i18n.Localizer) {
	if report == nil {
		return
	}
	_, _ = fmt.Fprintf(w, "    %s\n", localizer.Localize("wx.probe.submodules", nil))
	for _, summary := range report.Summaries {
		_, _ = fmt.Fprintf(w, "      %s\n", localizer.Localize("wx.probe.submodule_summary", map[string]any{
			"Repository": summary.Repository, "Depth": summary.Depth, "Materialized": summary.Materialized,
			"OutOfScope": summary.OutOfScope, "Skipped": summary.Skipped, "Unreachable": summary.Unreachable,
		}))
	}
	for _, detail := range report.Details {
		if detail.Action != "skipped" && detail.Action != "unreachable" {
			continue
		}
		_, _ = fmt.Fprintf(w, "      %s\n", localizer.Localize("wx.probe.submodule_detail", map[string]any{
			"Action": submoduleActionLabel(localizer, detail.Action), "Path": detail.Path, "Reason": detail.Reason,
		}))
	}
	if report.Truncated {
		_, _ = fmt.Fprintln(w, localizer.Localize("wx.probe.submodule_truncated", nil))
	}
}

func submoduleActionLabel(localizer *i18n.Localizer, action string) string {
	return localizer.LocalizeOr("wx.probe.submodule_action."+action, action)
}

// probeErrorText は失敗した区間だけを訳す。原因は外部由来の本文なので原文のまま残す。
func probeErrorText(localizer *i18n.Localizer, probe diag.Probe) string {
	if text := localizer.Message(probe.ErrorMessage); text != "" {
		return text
	}
	return probe.Error
}

func printProbeUsageLanguage(w io.Writer, probe diag.Probe, localizer *i18n.Localizer, diskLabel string) {
	exclusiveLabel := localizer.Localize("wx.probe.exclusive", nil)
	sharedLabel := localizer.Localize("wx.probe.shared", nil)
	switch {
	case probe.Usage == diag.ProbeUsageMeasured && len(probe.Repositories) == 0:
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", diskLabel, localizer.Localize("wx.probe.no_repository", nil))
	case probe.Usage != diag.ProbeUsageMeasured:
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", diskLabel,
			localizer.Localize("wx.probe.usage_incomplete", map[string]any{"Usage": probe.Usage}))
	}
	for _, repository := range probe.Repositories {
		_, _ = fmt.Fprintf(w, "    %-24s %9s %-4s  %9s %s\n", repository.Name,
			textfmt.HumanBytes(repository.ExclusiveBytes), exclusiveLabel, textfmt.HumanBytes(repository.SharedBytes), sharedLabel)
	}
}

// printProbePhases は準備の区間内訳を `wx bench` と同じ規則で出す。
func printProbePhases(w io.Writer, phases []diag.ProbePhase) {
	for _, phase := range phases {
		indent := "    "
		if strings.Contains(phase.Name, ".") {
			indent = "      "
		}
		// 時間を持たない区間は件数だけの記録なので、0秒を並べて時間の内訳と読み違えられないようにする。
		elapsed := formatProbeDuration(phase.MS)
		if phase.MS == 0 && phase.Count > 1 {
			elapsed = "-"
		}
		_, _ = fmt.Fprintf(w, "%s%-24s %9s  x%d\n", indent, phase.Name, elapsed, phase.Count)
	}
}

func formatProbeDuration(ms int64) string {
	return fmt.Sprintf("%.3fs", float64(ms)/1000)
}
