package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// statusRowMatches は表の行を path で探し、空白を 1 つに畳んだ残りの列が columns と一致するかを返す。
// 列幅は他の行の内容で変わるため、桁揃えを期待値に含めない。
func statusRowMatches(output, path, columns string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		trimmed := strings.Join(strings.Fields(line), " ")
		if rest, ok := strings.CutPrefix(trimmed, path+" "); ok {
			return rest == columns
		}
	}
	return false
}

func TestPrintStatusSummaryReadsWorkspaceLastUsedAndRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current := home + "/dev/wx"
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(current)
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	// cs は複数 repository の workspace で、root と一致する repository が無い。それでも workspace 側の last_used_at が時刻として出ることを検査する。
	payload := map[string]any{
		"schema_version": 7,
		"workspace_details": []map[string]any{
			{"id": "chez", "root": home + "/.local/share/chezmoi", "repositories": 1, "ready": 1, "leased": 0, "last_used_at": "2026-09-04T23:09:00Z"},
			{"id": "cs", "root": home + "/dev/cs", "repositories": 5, "ready": 1, "leased": 1, "last_used_at": "2026-09-04T14:47:00Z"},
			{"id": "prx", "root": home + "/dev/prx", "repositories": 1, "ready": 1, "leased": 0, "last_used_at": "2026-09-04T22:37:00Z"},
			{"id": "unused", "root": home + "/dev/unused", "repositories": 1, "ready": 0, "leased": 0},
			{"id": "wx", "root": current, "repositories": 1, "ready": 1, "leased": 2, "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"repository_details": []map[string]any{
			{"id": "chez-repo", "main_path": home + "/.local/share/chezmoi", "last_used_at": "2026-09-04T23:09:00Z"},
			{"id": "prx-repo", "main_path": home + "/dev/prx", "last_used_at": "2026-09-04T22:37:00Z"},
			{"id": "wx-repo", "main_path": home + "/elsewhere", "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"job_details":     map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots":  []map[string]any{{"path": home + "/wx", "active": true, "bytes": 1, "allocated_bytes": int64(365 * 1024 * 1024), "shared_bytes": int64(300 * 1024 * 1024), "exclusive_bytes": int64(65 * 1024 * 1024)}},
		"restart_pending": false,
		"stop_pending":    false,
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false)
	got := output.String()
	for _, want := range []string{
		"WORKSPACE",
		"LAST USED (JST)",
		"Daemon running · Jobs 0 pending / 0 running / 0 failed",
		"Disk   65 MiB managed · ~/wx",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	for _, want := range []struct{ path, columns string }{
		{path: "~/.local/share/chezmoi", columns: "1 0 09/05 08:09"},
		// 複数 repository でも、root と一致する repository が無くても時刻が出る。
		{path: "~/dev/cs", columns: "1 1 09/04 23:47"},
		{path: "~/dev/prx", columns: "1 0 09/05 07:37"},
		// 利用実績が無い workspace だけが「—」になる。
		{path: "~/dev/unused", columns: "0 0 —"},
		{path: "~/dev/wx *", columns: "1 2 09/05 07:16"},
	} {
		if !statusRowMatches(got, want.path, want.columns) {
			t.Fatalf("summary row %q missing columns %q:\n%s", want.path, want.columns, got)
		}
	}
	if strings.Contains(got, "HOT UNTIL") || strings.Contains(got, "quarantine") || strings.Contains(got, "session_details") {
		t.Fatalf("summary leaked detail-only output:\n%s", got)
	}
}

func TestPrintStatusSummaryDistinguishesLegacyWorkspaceLastUsed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	payload := map[string]any{
		"schema_version": 5,
		"workspace_details": []map[string]any{{
			"id": "old", "root": home + "/dev/old", "repositories": 1, "ready": 1, "leased": 0,
		}},
		// 旧 daemon が返す repository 単位の時刻を workspace の値へ補完してはいけない。
		"repository_details": []map[string]any{{"id": "old-repo", "main_path": home + "/dev/old", "last_used_at": "2026-09-04T23:09:00Z"}},
		"job_details":        map[string]any{"pending": 0, "running": 0, "failed": 0},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false)
	got := output.String()
	if !statusRowMatches(got, "~/dev/old", "1 0 unknown") {
		t.Fatalf("legacy workspace did not show unavailable LAST USED:\n%s", got)
	}
	if strings.Contains(got, "09/05 08:09") {
		t.Fatalf("legacy repository timestamp was used as workspace LAST USED:\n%s", got)
	}
	for _, want := range []string{
		"LAST USED unavailable",
		"daemon JSON schema 5",
		"update the daemon",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("legacy notice missing %q:\n%s", want, got)
		}
	}
}

// 補充停止は表の外の注記ではなく、該当 workspace の行そのものに出す。正常な行と同じ見た目だと見落とす。
func TestPrintStatusSummaryMarksTheStoppedWorkspaceRow(t *testing.T) {
	payload := map[string]any{
		"workspace_details": []map[string]any{
			{"id": "w1", "root": "/repo", "ready": 0, "leased": 0},
			{"id": "w2", "root": "/other", "ready": 1, "leased": 0},
		},
		"job_details":    map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots": []map[string]any{},
		"standby_replenishment": []map[string]any{
			{"root": "/repo", "reason": "STANDBY_PREPARE_FAILED", "detail": "job-1", "action": `wx retry-standby "/repo"`},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false)
	got := output.String()
	if !strings.Contains(got, "NOTE") {
		t.Fatalf("note column missing:\n%s", got)
	}
	var stopped, healthy string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "/repo") {
			stopped = line
		}
		if strings.HasPrefix(line, "/other") {
			healthy = line
		}
	}
	if !strings.Contains(stopped, `! standby replenishment stopped after a preparation failure; run wx retry-standby "/repo"`) {
		t.Fatalf("stopped workspace row=%q\n%s", stopped, got)
	}
	if !strings.Contains(healthy, "—") || strings.Contains(healthy, "!") {
		t.Fatalf("healthy workspace row=%q\n%s", healthy, got)
	}
}

// 登録が消えた workspace の停止は表に載らないので、残余として別行で出す。
func TestPrintStatusSummaryReportsStopsWithoutAWorkspaceRow(t *testing.T) {
	payload := map[string]any{
		"workspace_details":     []map[string]any{},
		"job_details":           map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots":        []map[string]any{},
		"standby_replenishment": []map[string]any{{"root": "/gone", "reason": "CLEAN", "detail": "run-1", "action": `wx retry-standby "/gone"`}},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false)
	got := output.String()
	if !strings.Contains(got, `/gone ! standby replenishment stopped after wx clear; run wx retry-standby "/gone"`) {
		t.Fatalf("leftover suspension missing:\n%s", got)
	}
}

func TestPrintDegradedStatusDoesNotInventCounts(t *testing.T) {
	var output bytes.Buffer
	printStatusDisplay(&output, map[string]any{"degraded": true, "database_path": "/state.db", "error": "SQLite is unavailable"}, false)
	got := output.String()
	if !strings.Contains(got, "Daemon degraded · SQLite is unavailable") || !strings.Contains(got, "Database: /state.db") {
		t.Fatalf("degraded output=%q", got)
	}
	if strings.Contains(got, "Jobs 0") || strings.Contains(got, "Workspaces: 0") {
		t.Fatalf("degraded output invented zero counts: %q", got)
	}
}

// 使用量は daemon の周期処理が測った値なので、未測定の 0 を実測値として出さず、測定済みは測定時刻を添える。
func TestStatusDiskSummaryDistinguishesPendingFromMeasuredUsage(t *testing.T) {
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })
	for _, testCase := range []struct {
		name string
		root map[string]any
		want string
	}{
		{
			name: "pending",
			root: map[string]any{"path": "/repo/wx", "bytes": int64(0), "allocated_bytes": int64(0), "exclusive_bytes": int64(0), "measurement": "pending"},
			want: "Disk   measuring · /repo/wx",
		},
		{
			// 満額の allocated_bytes ではなく共有分を除いた exclusive_bytes を出すことを、両者が異なる値で確かめる。
			name: "measured",
			root: map[string]any{"path": "/repo/wx", "bytes": int64(1), "allocated_bytes": int64(365 * 1024 * 1024), "shared_bytes": int64(300 * 1024 * 1024), "exclusive_bytes": int64(65 * 1024 * 1024), "measurement": "st_blocks_x_512", "measured_at": "2026-09-04T22:16:00Z"},
			want: "Disk   65 MiB managed · /repo/wx · measured 09/05 07:16 JST",
		},
		{
			// 旧 schema の daemon が返す payload には exclusive_bytes が無く、allocated_bytes を代わりに出すと単位が混ざる。
			name: "exclusive missing",
			root: map[string]any{"path": "/repo/wx", "bytes": int64(1), "allocated_bytes": int64(365 * 1024 * 1024), "measurement": "st_blocks_x_512"},
			want: "Disk   measurement unavailable · /repo/wx",
		},
		{
			name: "failed",
			root: map[string]any{"path": "/repo/wx", "measurement": "st_blocks_x_512", "error": "root is not registered"},
			want: "Disk   measurement failed · /repo/wx · root is not registered",
		},
	} {
		if got := statusDiskSummary(testCase.root); got != testCase.want {
			t.Fatalf("%s: disk summary=%q, want %q", testCase.name, got, testCase.want)
		}
	}
}
