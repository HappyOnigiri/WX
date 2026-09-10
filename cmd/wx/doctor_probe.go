package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/HappyOnigiri/WX/internal/diag"
)

// doctorProbeHint は静的検査だけを終えたときに、何をまだ見ていないかを伝える 1 行である。
const doctorProbeHint = "Nothing above was checked by preparing a worktree; run wx doctor --probe to prepare one in each registered workspace and inspect it."

// printDoctorProbes は実地検査が測った値を workspace ごとのブロックで出す。
// ここに出るのはすべて計測値で、良し悪しの判定は finding が持つ。
func printDoctorProbes(w io.Writer, probes []diag.Probe, verbose bool) {
	for _, probe := range probes {
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintln(w, "probe "+probe.Workspace)
		if probe.Error != "" {
			_, _ = fmt.Fprintln(w, "  error         "+probe.Error)
		}
		_, _ = fmt.Fprintf(w, "  lease         %s\n", formatProbeDuration(probe.LeaseMS))
		_, _ = fmt.Fprintf(w, "  EARLY READY   %s\n", formatProbeDuration(probe.EarlyReadyMS))
		_, _ = fmt.Fprintf(w, "  FULL READY    %s\n", formatProbeDuration(probe.FullReadyMS))
		printProbeUsage(w, probe)
		if probe.PhasesUnavailable {
			// 計測は daemon のメモリにしか残らないため、引けなかったことを黙って内訳なしにしない。
			_, _ = fmt.Fprintln(w, "  prepare job   breakdown unavailable; the daemon no longer holds the measurement")
		}
		if verbose {
			printProbePhases(w, probe.Phases)
		}
	}
}

// printProbeUsage は準備した slot のディスク使用量をリポジトリ別に出す。
// 測定を待てなかった回は 0 を実測値として読ませないよう、その旨だけを出す。
func printProbeUsage(w io.Writer, probe diag.Probe) {
	switch {
	case probe.Usage == diag.ProbeUsageMeasured && len(probe.Repositories) == 0:
		_, _ = fmt.Fprintln(w, "  disk          no repository was measured in the prepared slot")
	case probe.Usage != diag.ProbeUsageMeasured:
		_, _ = fmt.Fprintln(w, "  disk          "+probe.Usage+"; the daemon had not finished measuring the prepared slot")
	}
	for _, repository := range probe.Repositories {
		_, _ = fmt.Fprintf(w, "    %-24s %9s exclusive  %9s shared\n", repository.Name,
			formatHumanBytes(repository.ExclusiveBytes), formatHumanBytes(repository.SharedBytes))
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
