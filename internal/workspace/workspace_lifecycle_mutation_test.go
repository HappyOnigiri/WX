package workspace

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// 先行配置と残りの配置は、file だけをそれぞれ一度ずつ記録する。
// directory を記録すると、配置履歴が実体の粒度と一致しなくなる。
func TestLifecycleRecordCopiesKeepsStageAndFileBoundaries(t *testing.T) {
	t.Parallel()
	plan := earlyPlan{
		repositoryID: "repo",
		sourcePath:   "/source",
		copies: []copyEntry{
			{path: "early.txt"},
			{path: "late.txt"},
			{path: "nested", directory: true},
		},
		early: map[string]bool{
			"early.txt": true,
			"late.txt":  false,
			"nested":    true,
		},
	}

	plan.recordCopies(true)
	if len(plan.placed) != 1 {
		t.Fatalf("early placements=%+v, want only the early file", plan.placed)
	}
	if _, ok := plan.placed["early.txt"]; !ok {
		t.Fatalf("early placements=%+v, want early.txt", plan.placed)
	}

	plan.recordCopies(false)
	if len(plan.placed) != 2 {
		t.Fatalf("all placements=%+v, want one file per stage", plan.placed)
	}
	if _, ok := plan.placed["late.txt"]; !ok {
		t.Fatalf("all placements=%+v, want late.txt", plan.placed)
	}
	if _, ok := plan.placed["nested"]; ok {
		t.Fatalf("all placements=%+v, directory must not be recorded", plan.placed)
	}
}

// prepare command の失敗は診断ファイルが作れない場合でも、呼び出し側が読める detail_path を返す。
func TestLifecyclePrepareCommandErrorUsesUnavailableDetail(t *testing.T) {
	t.Parallel()
	err := (&PrepareCommandError{FailureID: "failure", Err: errors.New("cause")}).Error()
	if !strings.Contains(err, "detail_path=unavailable") {
		t.Fatalf("error=%q, want unavailable detail path", err)
	}
}

// 通常の entropy source では failure ID を unknown に置き換えず、診断の識別子として保存する。
func TestLifecyclePrepareFailureIDIsGenerated(t *testing.T) {
	t.Parallel()
	if id := newPrepareFailureID(); id == "" || id == "unknown" {
		t.Fatalf("failure ID=%q, want a generated identifier", id)
	}
}

// 診断ヘッダーと capture_error は、失敗時に後から原因を特定するために同じ詳細へ残す。
func TestLifecyclePrepareDiagnosticWritesHeaderAndCaptureError(t *testing.T) {
	t.Parallel()
	var nilDiagnostic *prepareDiagnostic
	nilDiagnostic.writeHeader([]string{"ignored"})

	file, err := os.CreateTemp(t.TempDir(), ".prepare-*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	temporary := file.Name()
	diagnostic := &prepareDiagnostic{
		file:       file,
		temporary:  temporary,
		final:      temporary + ".log",
		failureID:  "failure",
		writeError: errors.New("capture failed"),
	}
	diagnostic.writeHeader([]string{"wx", "argument with spaces"})
	path := diagnostic.finish(false, 17, false, false)
	if path != diagnostic.final {
		t.Fatalf("diagnostic path=%q, want %q", path, diagnostic.final)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"failure_id: failure",
		`command: "wx" "argument with spaces"`,
		"capture_error: capture failed",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("diagnostic=%q, want %q", content, want)
		}
	}
}
