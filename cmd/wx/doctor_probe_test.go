package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/diag"
)

func TestPrintDoctorProbesShowsTimesAndRepositoryUsage(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, []diag.Probe{{
		Workspace: "/repos/app", LeaseMS: 120, EarlyReadyMS: 2100, FullReadyMS: 17300,
		Usage: diag.ProbeUsageMeasured,
		Repositories: []diag.ProbeRepository{
			{Name: "app", ExclusiveBytes: 1536, SharedBytes: 2048},
		},
		Phases: []diag.ProbePhase{{Name: "checkout", Count: 1, MS: 900}},
	}}, false)
	text := out.String()
	for _, want := range []string{"probe /repos/app", "EARLY READY   2.100s", "FULL READY    17.300s", "1.5 KiB exclusive", "2 KiB shared"} {
		if !strings.Contains(text, want) {
			t.Fatalf("output %q lacks %q", text, want)
		}
	}
	// 区間内訳は -v のときだけ出す。
	if strings.Contains(text, "checkout") {
		t.Fatalf("output %q shows phases without --verbose", text)
	}
}

func TestPrintDoctorProbesShowsPhasesWithVerbose(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, []diag.Probe{{
		Workspace: "/repos/app", Usage: diag.ProbeUsageMeasured,
		Phases: []diag.ProbePhase{{Name: "checkout", Count: 1, MS: 900}, {Name: "cow.compare", Count: 4, MS: 0}},
	}}, true)
	text := out.String()
	if !strings.Contains(text, "    checkout") || !strings.Contains(text, "      cow.compare") {
		t.Fatalf("output %q lacks the indented phase breakdown", text)
	}
	// 時間を持たない区間は件数だけの記録なので、0秒を時間の内訳と読み違えさせない。
	if !strings.Contains(text, "-  x4") {
		t.Fatalf("output %q reports a countedonly phase as a duration", text)
	}
}

// 測定を待てなかった回に 0 バイトを並べると、実測値として読まれてしまう。
func TestPrintDoctorProbesSaysWhenUsageIsPending(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, []diag.Probe{{Workspace: "/repos/app", Usage: diag.ProbeUsagePending}}, false)
	if !strings.Contains(out.String(), "pending") {
		t.Fatalf("output %q does not report the missing measurement", out.String())
	}
}

// 内訳が引けなかったことを黙って落とすと、計測が無いのか 0 なのかを読み分けられない。
func TestPrintDoctorProbesSaysWhenPhasesAreUnavailable(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, []diag.Probe{{Workspace: "/repos/app", Usage: diag.ProbeUsageMeasured, PhasesUnavailable: true}}, false)
	if !strings.Contains(out.String(), "breakdown unavailable") {
		t.Fatalf("output %q does not report the missing breakdown", out.String())
	}
}

func TestPrintDoctorProbesShowsProbeError(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, []diag.Probe{{Workspace: "/repos/app", Usage: diag.ProbeUsageUnavailable, Error: "lease: refused"}}, false)
	if !strings.Contains(out.String(), "lease: refused") {
		t.Fatalf("output %q does not report the failure", out.String())
	}
}

func TestPrintDoctorProbesWritesNothingWithoutProbes(t *testing.T) {
	var out bytes.Buffer
	printDoctorProbes(&out, nil, true)
	if out.Len() != 0 {
		t.Fatalf("output = %q, want empty", out.String())
	}
}
