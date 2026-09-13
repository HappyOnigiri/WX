package main

import (
	"fmt"
	"io"
	"strings"

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
	probeLabel, errorLabel, leaseLabel, earlyLabel, fullLabel, prepareLabel, diskLabel := "probe", "error", "lease", "EARLY READY", "FULL READY", "prepare job", "disk"
	if lang == i18n.Japanese {
		probeLabel, errorLabel, leaseLabel, earlyLabel, fullLabel, prepareLabel, diskLabel = "検査", "エラー", "貸出", "早期準備完了", "準備完了", "準備 job", "ディスク"
	}
	for _, probe := range probes {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, probeLabel+" "+probe.Workspace)
		if probe.Error != "" {
			_, _ = fmt.Fprintf(w, "  %-13s %s\n", errorLabel, localizeProbeError(probe.Error, lang))
		}
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", leaseLabel, formatProbeDuration(probe.LeaseMS))
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", earlyLabel, formatProbeDuration(probe.EarlyReadyMS))
		_, _ = fmt.Fprintf(w, "  %-13s %s\n", fullLabel, formatProbeDuration(probe.FullReadyMS))
		printProbeUsageLanguage(w, probe, lang, diskLabel)
		if probe.PhasesUnavailable {
			// 計測は daemon のメモリにしか残らないため、引けなかったことを黙って内訳なしにしない。
			if lang == i18n.Japanese {
				_, _ = fmt.Fprintf(w, "  %-13s 準備の内訳を利用できません。daemon に計測値が残っていません\n", prepareLabel)
			} else {
				_, _ = fmt.Fprintf(w, "  %-13s breakdown unavailable; the daemon no longer holds the measurement\n", prepareLabel)
			}
		}
		if verbose {
			printProbePhases(w, probe.Phases)
		}
	}
}

func localizeProbeError(value string, lang i18n.Language) string {
	if lang != i18n.Japanese {
		return value
	}
	for _, replacement := range []struct{ en, ja string }{
		{"retire standby:", "standby を退役:"},
		{"lease:", "貸出:"},
		{"early ready:", "早期準備完了:"},
		{"full ready:", "準備完了:"},
	} {
		if strings.HasPrefix(value, replacement.en) {
			return replacement.ja + strings.TrimPrefix(value, replacement.en)
		}
	}
	return value
}

func printProbeUsageLanguage(w io.Writer, probe diag.Probe, lang i18n.Language, diskLabel string) {
	exclusiveLabel, sharedLabel := "exclusive", "shared"
	if lang == i18n.Japanese {
		exclusiveLabel, sharedLabel = "専有", "共有"
	}
	switch {
	case probe.Usage == diag.ProbeUsageMeasured && len(probe.Repositories) == 0:
		if lang == i18n.Japanese {
			_, _ = fmt.Fprintf(w, "  %-13s 準備済み slot で測定された repository はありません\n", diskLabel)
		} else {
			_, _ = fmt.Fprintf(w, "  %-13s no repository was measured in the prepared slot\n", diskLabel)
		}
	case probe.Usage != diag.ProbeUsageMeasured:
		if lang == i18n.Japanese {
			_, _ = fmt.Fprintf(w, "  %-13s %s。daemon は準備済み slot の測定を完了していません\n", diskLabel, probe.Usage)
		} else {
			_, _ = fmt.Fprintf(w, "  %-13s %s; the daemon had not finished measuring the prepared slot\n", diskLabel, probe.Usage)
		}
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
