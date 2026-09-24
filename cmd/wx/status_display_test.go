package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	xansi "github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
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
	printStatusDisplay(&output, payload, false, i18n.English)
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
	// policy を返さない schema では方針を判定できないため、POLICY は unknown で、絞り込みも効かず全件が残る。
	for _, want := range []struct{ path, columns string }{
		{path: "~/.local/share/chezmoi", columns: "unknown 1 0 09/05 08:09"},
		// 複数 repository でも、root と一致する repository が無くても時刻が出る。
		{path: "~/dev/cs", columns: "unknown 1 1 09/04 23:47"},
		{path: "~/dev/prx", columns: "unknown 1 0 09/05 07:37"},
		// 利用実績が無い workspace だけが「—」になる。
		{path: "~/dev/unused", columns: "unknown 0 0 —"},
		{path: "~/dev/wx *", columns: "unknown 1 2 09/05 07:16"},
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
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	if !statusRowMatches(got, "~/dev/old", "unknown 1 0 unknown") {
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
		"schema_version": 14,
		// 補充停止は hot の workspace でしか起きないため、READY が無くても表に残る。
		"workspace_details": []map[string]any{
			{"id": "w1", "root": "/repo", "policy": "hot", "ready": 0, "leased": 0},
			{"id": "w2", "root": "/other", "policy": "hot", "ready": 1, "leased": 0},
		},
		"job_details":    map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots": []map[string]any{},
		"standby_replenishment": []map[string]any{
			{"root": "/repo", "reason": "STANDBY_PREPARE_FAILED", "detail": "job-1", "action": `wx retry-standby "/repo"`},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.English)
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

// 補充計画の失敗は補充を止めないので、停止とは違う注記で workspace 行に出す。
func TestPrintStatusSummaryMarksThePlanFailureRow(t *testing.T) {
	payload := map[string]any{
		"workspace_details": []map[string]any{{"id": "w1", "root": "/repo", "policy": "hot", "ready": 0, "leased": 0}},
		"job_details":       map[string]any{"pending": 0, "running": 0, "failed": 1},
		"worktree_roots":    []map[string]any{},
		"standby_replenishment": []map[string]any{
			{"root": "/repo", "reason": "STANDBY_PLAN_FAILED", "detail": "job-9", "failed_at": "2026-09-11T00:00:00Z", "action": `wx retry-standby "/repo"`},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	if !strings.Contains(got, `! standby replenishment failed to plan new worktrees; run wx retry-standby "/repo"`) {
		t.Fatalf("plan failure note missing:\n%s", got)
	}
	// -v では失敗時刻まで出し、未知キーとして Additional へ落とさない。
	var verbose bytes.Buffer
	printStatusDisplay(&verbose, payload, true, i18n.English)
	detailed := verbose.String()
	if !strings.Contains(detailed, "Failed: 2026-09-11T00:00:00Z") {
		t.Fatalf("verbose plan failure output=%s", detailed)
	}
	if strings.Contains(detailed, "failed_at") {
		t.Fatalf("verbose output reported failed_at as an unknown key:\n%s", detailed)
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
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	if !strings.Contains(got, `/gone ! standby replenishment stopped after wx clear; run wx retry-standby "/gone"`) {
		t.Fatalf("leftover suspension missing:\n%s", got)
	}
}

func TestPrintDegradedStatusDoesNotInventCounts(t *testing.T) {
	var output bytes.Buffer
	printStatusDisplay(&output, map[string]any{"degraded": true, "database_path": "/state.db", "error": "SQLite is unavailable"}, false, i18n.English)
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
		if got := statusDiskSummary(newTextRenderer(io.Discard, i18n.English), testCase.root); got != testCase.want {
			t.Fatalf("%s: disk summary=%q, want %q", testCase.name, got, testCase.want)
		}
	}
}

// 要約は worktree を作る方針の workspace と、実際に worktree を持つ workspace だけを載せる。
func TestPrintStatusSummaryListsOnlyWorkspacesThatUseAWorktree(t *testing.T) {
	payload := map[string]any{
		"schema_version": 14,
		"workspace_details": []map[string]any{
			{"id": "hot", "root": "/hot", "policy": "hot", "ready": 0, "leased": 0},
			{"id": "cold", "root": "/cold", "policy": "cold", "ready": 0, "leased": 0},
			// 方針を off・ask にしても、残っている worktree は回収の判断に要るので隠さない。
			{"id": "off-ready", "root": "/off-ready", "policy": "off", "ready": 1, "leased": 0},
			{"id": "ask-leased", "root": "/ask-leased", "policy": "ask", "ready": 0, "leased": 2},
			{"id": "off", "root": "/off", "policy": "off", "ready": 0, "leased": 0},
			{"id": "ask", "root": "/ask", "policy": "ask", "ready": 0, "leased": 0},
		},
		"job_details":    map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots": []map[string]any{},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	for _, want := range []struct{ path, columns string }{
		{path: "/hot", columns: "HOT 0 0 —"},
		{path: "/cold", columns: "COLD 0 0 —"},
		{path: "/off-ready", columns: "OFF 1 0 —"},
		{path: "/ask-leased", columns: "ASK 0 2 —"},
	} {
		if !statusRowMatches(got, want.path, want.columns) {
			t.Fatalf("summary row %q missing columns %q:\n%s", want.path, want.columns, got)
		}
	}
	for _, hidden := range []string{"/off ", "/ask "} {
		if strings.Contains(got, hidden) {
			t.Fatalf("summary listed workspace %q without a worktree:\n%s", hidden, got)
		}
	}
	if strings.Contains(got, "POLICY unavailable") {
		t.Fatalf("summary reported the policy as unavailable:\n%s", got)
	}
}

// 全行が絞り込みで消えたときは、(none) を登録ゼロと読み違えないよう件数と確認手段を添える。
func TestPrintStatusSummaryNotesHiddenWorkspacesWhenNoRowRemains(t *testing.T) {
	payload := map[string]any{
		"schema_version": 14,
		"workspace_details": []map[string]any{
			{"id": "off", "root": "/off", "policy": "off", "ready": 0, "leased": 0},
			{"id": "ask", "root": "/ask", "policy": "ask", "ready": 0, "leased": 0},
		},
		"job_details":    map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots": []map[string]any{},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	for _, want := range []string{"(none)", "2 registered workspaces use no worktree; run wx status --verbose to list them"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
}

// 登録がそもそも無いときは、隠した件数の案内を出さない。
func TestPrintStatusSummaryOmitsHiddenNoticeForAnEmptyRegistry(t *testing.T) {
	payload := map[string]any{
		"schema_version":    14,
		"workspace_details": []map[string]any{},
		"job_details":       map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots":    []map[string]any{},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.English)
	got := output.String()
	if !strings.Contains(got, "(none)") {
		t.Fatalf("summary missing %q:\n%s", "(none)", got)
	}
	if strings.Contains(got, "use no worktree") {
		t.Fatalf("empty registry reported hidden workspaces:\n%s", got)
	}
}

// TestStatusArchivedSessionNoticeTracksTheSchemaVersion は集計を返さない daemon の判定境界を固定する。
func TestStatusArchivedSessionNoticeTracksTheSchemaVersion(t *testing.T) {
	r := newTextRenderer(io.Discard, i18n.English)
	if notice := statusArchivedSessionNotice(r, map[string]any{"schema_version": archivedSessionSchemaVersion}); notice != "" {
		t.Fatalf("current schema notice=%q, want empty", notice)
	}
	notice := statusArchivedSessionNotice(r, map[string]any{"schema_version": archivedSessionSchemaVersion - 1})
	if !strings.Contains(notice, "18") || !strings.Contains(notice, "still lists archived sessions") {
		t.Fatalf("legacy schema notice=%q", notice)
	}
	// schema_version を返さない応答では版を判定できないため、注記も劣化表示も出さない。
	if notice := statusArchivedSessionNotice(r, map[string]any{}); notice != "" {
		t.Fatalf("notice without a schema version=%q, want empty", notice)
	}
}

// TestStatusDaemonSummarySeparatesDiscardedJobs は、既定の 1 行が取り消しを失敗と混ぜないことを固定する。
// `wx clear` の取り消しは retention.failed_job まで残るので、混ぜると対処の要らない件数が失敗として読める。
func TestStatusDaemonSummarySeparatesDiscardedJobs(t *testing.T) {
	r := newTextRenderer(io.Discard, i18n.English)
	line := statusDaemonSummary(r, map[string]any{
		"job_details": map[string]any{"pending": 0, "running": 0, "failed": 5, "discarded": 80},
	})
	if want := "Daemon running · Jobs 0 pending / 0 running / 5 failed / 80 discarded"; line != want {
		t.Fatalf("daemon summary=%q, want %q", line, want)
	}
	// discarded を返さない daemon では件数を作らず、失敗側の値もそのまま出す。
	legacy := statusDaemonSummary(r, map[string]any{"job_details": map[string]any{"pending": 0, "running": 0, "failed": 85}})
	if want := "Daemon running · Jobs 0 pending / 0 running / 85 failed / — discarded"; legacy != want {
		t.Fatalf("legacy daemon summary=%q, want %q", legacy, want)
	}
	// job_details が無い応答では pending だけが queued_jobs から分かる。
	queued := statusDaemonSummary(r, map[string]any{"queued_jobs": 2})
	if want := "Daemon running · Jobs 2 pending / — running / — failed / — discarded"; queued != want {
		t.Fatalf("queued-only daemon summary=%q, want %q", queued, want)
	}
}

// TestPrintStatusSummaryJapaneseHeaderAlignsWithTheRows は、見出しを桁計算より前に
// 解決していることを検査する。英語の幅で桁を決めた後に訳すと、見出しだけが行の値からずれる。
func TestPrintStatusSummaryJapaneseHeaderAlignsWithTheRows(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	payload := map[string]any{
		"schema_version": 14,
		"workspace_details": []map[string]any{
			{"id": "wx", "root": home + "/dev/wx", "policy": "hot", "repositories": 1, "ready": 1, "leased": 0, "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"job_details":    map[string]any{"pending": 0, "running": 0, "failed": 0},
		"worktree_roots": []map[string]any{{"path": home + "/wx", "active": true}},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.Japanese)
	rendered := output.String()
	if !strings.Contains(rendered, "ワークスペース 方針") {
		t.Fatalf("summary did not resolve the header before the table:\n%s", rendered)
	}
	header := statusHeaderLine(rendered, "ワークスペース")
	// 見出しの次の行は workspace の行で、path の列は見出しと同じ桁で始まる。
	row := statusLineAfter(rendered, header)
	if want := xansi.StringWidth("ワークスペース") + 1; !strings.HasPrefix(row, "~/dev/wx"+strings.Repeat(" ", want-xansi.StringWidth("~/dev/wx")-1)+" ") {
		t.Fatalf("row does not start the second column at %d:\nheader=%q\nrow=%q", want, header, row)
	}
}

func statusHeaderLine(text, prefix string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

func statusLineAfter(text, line string) string {
	lines := strings.Split(text, "\n")
	for index, candidate := range lines {
		if candidate == line && index+1 < len(lines) {
			return lines[index+1]
		}
	}
	return ""
}

// TestPrintStatusSummaryJapaneseKeepsPathsAndTimesVerbatim は本件の回帰テストである。
// 出力全文へ辞書を当てていたときは `~/dev/ReleaseActions` が `~/dev/Release操作s` になった。
// 固定文は描画時に解決し、payload の path・時刻・zone は訳を通さないことを固定する。
func TestPrintStatusSummaryJapaneseKeepsPathsAndTimesVerbatim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	previousLocation := statusDisplayLocation
	statusDisplayLocation = time.FixedZone("JST", 9*60*60)
	t.Cleanup(func() { statusDisplayLocation = previousLocation })

	payload := map[string]any{
		"schema_version": 20,
		"workspace_details": []map[string]any{
			{"id": "a", "root": home + "/dev/ReleaseActions", "policy": "hot", "ready": 1, "leased": 0, "last_used_at": "2026-09-04T22:16:00Z"},
			{"id": "b", "root": home + "/dev/StopTotalRunning", "policy": "cold", "ready": 0, "leased": 1, "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"job_details": map[string]any{"pending": 0, "running": 0, "failed": 0, "discarded": 0},
		"worktree_roots": []map[string]any{
			{"path": home + "/wx/Action", "active": true, "exclusive_bytes": 65 * 1024 * 1024, "measured_at": "2026-09-13T01:57:00Z"},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, false, i18n.Japanese)
	rendered := output.String()
	for _, want := range []string{"~/dev/ReleaseActions", "~/dev/StopTotalRunning", "~/wx/Action", "09/13 10:57", "JST"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("Japanese summary lost the opaque value %q:\n%s", want, rendered)
		}
	}
	// 辞書による全文置換が戻ると、path・状態値の中の語がこれらへ化ける。
	for _, unwanted := range []string{"操作", "停止", "合計", "実行中"} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("Japanese summary translated a data value into %q:\n%s", unwanted, rendered)
		}
	}
}

// TestStatusDiskSummaryAlignsTheLabelWithTheDaemonLine は、桁合わせの空白が訳文ではなく
// 表示幅の計算で決まることを固定する。訳文へ空白を埋めると訳語の変更で桁がずれる。
func TestStatusDiskSummaryAlignsTheLabelWithTheDaemonLine(t *testing.T) {
	for _, lang := range []i18n.Language{i18n.English, i18n.Japanese} {
		r := newTextRenderer(io.Discard, lang)
		daemon := statusSummaryLabel(r, "status.label.daemon")
		disk := statusSummaryLabel(r, "status.label.disk")
		if xansi.StringWidth(daemon) != xansi.StringWidth(disk) {
			t.Fatalf("%s: daemon label %q and disk label %q have different widths", lang, daemon, disk)
		}
	}
	// 英語の出力は移行前とバイト一致でなければならない。
	if got := statusSummaryLabel(newTextRenderer(io.Discard, i18n.English), "status.label.disk"); got != "Disk   " {
		t.Fatalf("English disk label=%q", got)
	}
}

// TestPrintVerboseStatusJapaneseKeepsPayloadValues は、詳細表示でも節見出しだけが訳され、
// ID・path・時刻・reason コードが原文のまま残ることを固定する。
func TestPrintVerboseStatusJapaneseKeepsPayloadValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	payload := map[string]any{
		"schema_version": 20,
		"workspace_details": []map[string]any{
			{"id": "wsActionTotal", "root": home + "/dev/ReleaseActions", "policy": "hot", "ready": 1, "leased": 0, "last_used_at": "2026-09-04T22:16:00Z"},
		},
		"quarantine": []map[string]any{
			{"id": "slotStop", "kind": "slot", "failure_code": "STANDBY_PREPARE_FAILED", "path": home + "/wx/Action/slot"},
		},
		"standby_replenishment": []map[string]any{
			{"root": home + "/dev/ReleaseActions", "reason": "STANDBY_PREPARE_FAILED", "action": "wx retry-standby ~/dev/ReleaseActions"},
		},
	}
	var output bytes.Buffer
	printStatusDisplay(&output, payload, true, i18n.Japanese)
	rendered := output.String()
	if !strings.Contains(rendered, "ワークスペース") || !strings.Contains(rendered, "隔離") {
		t.Fatalf("verbose Japanese did not translate the section headings:\n%s", rendered)
	}
	for _, want := range []string{
		"wsActionTotal", "~/dev/ReleaseActions", "slotStop", "STANDBY_PREPARE_FAILED",
		"2026-09-04T22:16:00Z", "wx retry-standby ~/dev/ReleaseActions",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("verbose Japanese lost the opaque value %q:\n%s", want, rendered)
		}
	}
}
