// citest は通常のGoテストを観測し、名前付き失敗だけを1回再実行する。
package main

import (
	"time"

	"github.com/HappyOnigiri/WorktreeX/tools/internal/gotest"
)

// citest は go test -json の解析と宣言解決を tools/internal/gotest と共有する。
// manifest の JSON 契約は gotest.Declaration 側のタグで決まる。
type (
	testEvent   = gotest.Event
	testResult  = gotest.Result
	declaration = gotest.Declaration
)

type config struct {
	Profile         string
	ReportDir       string
	CoverageProfile string
	RepoRoot        string
	GoCommand       string
	Command         []string
}

type retryRecord struct {
	Package          string            `json:"package"`
	Functions        []string          `json:"functions"`
	Command          []string          `json:"command"`
	Exit             int               `json:"exit"`
	Status           string            `json:"status"`
	Recovered        bool              `json:"recovered"`
	Reason           string            `json:"reason,omitempty"`
	JSONL            string            `json:"jsonl"`
	Log              string            `json:"log"`
	Stderr           string            `json:"stderr"`
	Coverage         string            `json:"coverage,omitempty"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at"`
	DurationMS       int64             `json:"duration_ms"`
	FailedTests      []string          `json:"failed_tests"`
	PassedTests      []string          `json:"passed_tests"`
	Shuffle          string            `json:"shuffle,omitempty"`
	ShuffleByPackage map[string]string `json:"shuffle_by_package,omitempty"`
	LogExcerpt       string            `json:"log_excerpt,omitempty"`
}

type recovery struct {
	Package       string      `json:"package"`
	Declaration   declaration `json:"declaration"`
	FailedTests   []string    `json:"failed_tests"`
	InitialResult string      `json:"initial_result"`
	RetryResult   string      `json:"retry_result"`
	RetryIndex    int         `json:"retry_index"`
}

type manifest struct {
	SchemaVersion int               `json:"schema_version"`
	Profile       string            `json:"profile"`
	Status        string            `json:"status"`
	Recovered     bool              `json:"recovered"`
	Repository    string            `json:"repository,omitempty"`
	RunID         string            `json:"run_id,omitempty"`
	RunAttempt    string            `json:"run_attempt,omitempty"`
	Event         string            `json:"event,omitempty"`
	Ref           string            `json:"ref,omitempty"`
	APISHA        string            `json:"api_head_sha,omitempty"`
	TestSHA       string            `json:"test_sha,omitempty"`
	GoVersion     string            `json:"go_version,omitempty"`
	OS            string            `json:"os,omitempty"`
	Arch          string            `json:"arch,omitempty"`
	Initial       runRecord         `json:"initial"`
	Retries       []retryRecord     `json:"retries,omitempty"`
	Recoveries    []recovery        `json:"recoveries,omitempty"`
	Diagnostics   []string          `json:"diagnostics,omitempty"`
	Conditions    map[string]string `json:"conditions,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

type runRecord struct {
	Command          []string          `json:"command"`
	Exit             int               `json:"exit"`
	Status           string            `json:"status"`
	Signal           string            `json:"signal,omitempty"`
	JSONL            string            `json:"jsonl"`
	Log              string            `json:"log"`
	Stderr           string            `json:"stderr"`
	Coverage         string            `json:"coverage,omitempty"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at"`
	DurationMS       int64             `json:"duration_ms"`
	Shuffle          string            `json:"shuffle,omitempty"`
	ShuffleByPackage map[string]string `json:"shuffle_by_package,omitempty"`
	LogExcerpt       string            `json:"log_excerpt,omitempty"`
}

type coverageBlock struct {
	Location   string
	Statements int
	Count      int64
}

type coverageProfile struct {
	Mode   string
	Blocks []coverageBlock
}
