package main

import (
	"fmt"
	"io"
	"strings"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/textfmt"
)

// doctorProbeHint は静的検査だけを終えたときに、何をまだ見ていないかを伝える 1 行である。
const doctorProbeHint = "Nothing above was checked by preparing a worktree; run wx doctor --probe to prepare one in each registered workspace and inspect it."

// printDoctorProbes は実地検査が測った値を workspace ごとのブロックで出す。
// ここに出るのはすべて計測値で、良し悪しの判定は finding が持つ。
func printDoctorProbes(w io.Writer, probes []diag.Probe, verbose bool) {
	printDoctorProbesLanguage(w, probes, verbose, i18n.English)
}

func printDoctorProbesLanguage(w io.Writer, probes []diag.Probe, verbose bool, lang i18n.Language) {
	loc := i18n.New(string(lang))
	for _, probe := range probes {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, loc.Localize("doctor.probe.label", nil)+" "+probe.Workspace)
		if probe.Error != "" {
			// probe.Error は --json にも出る計測結果の本文なので、訳さずそのまま見せる。
			printProbeField(w, loc, "common.error", probe.Error)
		}
		printProbeField(w, loc, "doctor.probe.lease", formatProbeDuration(probe.LeaseMS))
		printProbeField(w, loc, "doctor.probe.early_ready", formatProbeDuration(probe.EarlyReadyMS))
		printProbeField(w, loc, "doctor.probe.full_ready", formatProbeDuration(probe.FullReadyMS))
		printProbeUsageLanguage(w, probe, loc)
		if probe.PhasesUnavailable {
			// 計測は daemon のメモリにしか残らないため、引けなかったことを黙って内訳なしにしない。
			printProbeField(w, loc, "doctor.probe.prepare_job", loc.Localize("doctor.probe.breakdown_unavailable", nil))
		}
		if verbose {
			printProbePhases(w, probe.Phases)
		}
	}
}

// probeLabelWidth は probe のラベル列の幅である。訳語で桁がずれないよう、幅は表示幅で測る。
const probeLabelWidth = 13

// printProbeField はラベルを message ID で解決し、値は不透明なまま 1 行に出す。
func printProbeField(w io.Writer, loc *i18n.Localizer, id, value string) {
	label := loc.Localize(id, nil)
	if pad := probeLabelWidth - xansi.StringWidth(label); pad > 0 {
		label += strings.Repeat(" ", pad)
	}
	_, _ = fmt.Fprintf(w, "  %s %s\n", label, value)
}

func printProbeUsageLanguage(w io.Writer, probe diag.Probe, loc *i18n.Localizer) {
	switch {
	case probe.Usage == diag.ProbeUsageMeasured && len(probe.Repositories) == 0:
		printProbeField(w, loc, "doctor.probe.disk", loc.Localize("doctor.probe.no_repository", nil))
	case probe.Usage != diag.ProbeUsageMeasured:
		printProbeField(w, loc, "doctor.probe.disk", loc.Localize("doctor.probe.usage_incomplete", map[string]any{"State": probe.Usage}))
	}
	exclusiveLabel := loc.Localize("doctor.probe.exclusive", nil)
	sharedLabel := loc.Localize("doctor.probe.shared", nil)
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
