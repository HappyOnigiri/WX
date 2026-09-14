// huntreport は Flake Hunt の1コンテナ分を実行し、テスト単位の pass/fail を数えた manifest を残す。
// 通常CIのcitestと同じ go test -json の解析を使い、報告は report-flaky-tests.cjs が引き継ぐ。
package main

import "time"

// huntSchemaVersion は manifest の形状の版で、通常CIのmanifestとは独立に上げる。
// 未知の版を読んだ reporting 側は warning を出して読み飛ばす。
const huntSchemaVersion = 1

// huntKind は manifest の種別で、通常CIのmanifestと取り違えないための印である。
const huntKind = "flake-hunt"

type config struct {
	HuntID    string
	ReportDir string
	LogDir    string
	RepoRoot  string
	GoCommand string
	Deadline  time.Duration
	Command   []string
}

// roundRecord は1ラウンドの実行結果で、失敗したラウンドだけログを残す。
// Anomaly が空でないラウンドは、テスト単位の集計を信じてよい実行ではない。
type roundRecord struct {
	Round      int    `json:"round"`
	Status     string `json:"status"`
	Exit       int    `json:"exit"`
	Signal     string `json:"signal,omitempty"`
	Anomaly    string `json:"anomaly,omitempty"`
	Shuffle    string `json:"shuffle,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Log        string `json:"log,omitempty"`
	TestEvents int    `json:"test_events"`
}

// testTally は1つのテスト関数について、ラウンドをまたいだ終端イベントの回数をまとめる。
// PassCount と FailCount がどちらも正のときだけ flaky とみなせる。判定は reporting 側が行う。
type testTally struct {
	Package     string      `json:"package"`
	Declaration declaration `json:"declaration"`
	Subtests    []string    `json:"subtests,omitempty"`
	PassCount   int         `json:"pass_count"`
	FailCount   int         `json:"fail_count"`
	SkipCount   int         `json:"skip_count"`
	LogExcerpt  string      `json:"log_excerpt,omitempty"`
}

type huntManifest struct {
	SchemaVersion int           `json:"schema_version"`
	Kind          string        `json:"kind"`
	HuntID        string        `json:"hunt_id"`
	Command       []string      `json:"command"`
	RunRegexp     string        `json:"run_regexp,omitempty"`
	Count         string        `json:"count,omitempty"`
	Rounds        int           `json:"rounds"`
	FailedRounds  int           `json:"failed_rounds"`
	AnomalyRounds int           `json:"anomaly_rounds"`
	Repository    string        `json:"repository,omitempty"`
	RunID         string        `json:"run_id,omitempty"`
	RunAttempt    string        `json:"run_attempt,omitempty"`
	Event         string        `json:"event,omitempty"`
	Ref           string        `json:"ref,omitempty"`
	APISHA        string        `json:"api_head_sha,omitempty"`
	TestSHA       string        `json:"test_sha,omitempty"`
	GoVersion     string        `json:"go_version,omitempty"`
	OS            string        `json:"os,omitempty"`
	Arch          string        `json:"arch,omitempty"`
	RoundRecords  []roundRecord `json:"round_records,omitempty"`
	Tests         []testTally   `json:"tests,omitempty"`
	Diagnostics   []string      `json:"diagnostics,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	FinishedAt    time.Time     `json:"finished_at"`
}
