// citest は通常のGoテストを観測し、名前付き失敗だけを1回再実行する。
package main

import "time"

type config struct {
	Profile         string
	ReportDir       string
	CoverageProfile string
	RepoRoot        string
	GoCommand       string
	Command         []string
}

type testEvent struct {
	Time        string  `json:"Time,omitempty"`
	Action      string  `json:"Action,omitempty"`
	Package     string  `json:"Package,omitempty"`
	Test        string  `json:"Test,omitempty"`
	Elapsed     float64 `json:"Elapsed,omitempty"`
	Output      string  `json:"Output,omitempty"`
	FailedBuild string  `json:"FailedBuild,omitempty"`
}

type testResult struct {
	Package          string
	Events           []testEvent
	Tests            map[string][]testEvent
	StartedAt        time.Time
	FinishedAt       time.Time
	Status           string
	Exit             int
	Signal           string
	Malformed        bool
	Anomaly          string
	Shuffle          string
	ShuffleByPackage map[string]string
	LogExcerpt       string
}

type packageDeclaration struct {
	ImportPath   string   `json:"ImportPath"`
	Dir          string   `json:"Dir"`
	GoFiles      []string `json:"GoFiles"`
	TestGoFiles  []string `json:"TestGoFiles"`
	XTestGoFiles []string `json:"XTestGoFiles"`
}

type declaration struct {
	Package  string `json:"package"`
	Path     string `json:"path"`
	Function string `json:"function"`
	Line     int    `json:"line"`
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
