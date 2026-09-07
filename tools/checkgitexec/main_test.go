package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// repository は検査対象の最小構成を組み立てる。キーはrepository-relative pathである。
func repository(sources map[string]string) fstest.MapFS {
	files := fstest.MapFS{}
	for path, source := range sources {
		files[path] = &fstest.MapFile{Data: []byte(source)}
	}
	return files
}

// clean は違反を出さないことを確認する。scannedはこの検査が読んだ本番ファイル数の期待値。
func clean(t *testing.T, scanned int, sources map[string]string) {
	t.Helper()
	var out bytes.Buffer
	if err := run(repository(sources), &out); err != nil {
		t.Fatalf("run: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "checkgitexec: "+strconv.Itoa(scanned)+" production file(s)") {
		t.Fatalf("output=%q, want %d scanned file(s)", out.String(), scanned)
	}
}

// dirty は違反を出し、報告に期待する語句が含まれることを確認する。
func dirty(t *testing.T, sources map[string]string, wants ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := run(repository(sources), &out); err == nil {
		t.Fatalf("direct Git launch accepted (output %q)", out.String())
	}
	for _, want := range wants {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not mention %q", out.String(), want)
		}
	}
	return out.String()
}

const plainViolation = `package pool

import (
	"context"
	"os/exec"
)

func Prune(ctx context.Context) error {
	return exec.CommandContext(ctx, "git", "worktree", "prune").Run()
}
`

func TestRunReportsDirectGitLaunch(t *testing.T) {
	t.Parallel()
	output := dirty(t, map[string]string{"internal/pool/prune.go": plainViolation},
		"internal/pool/prune.go:9:", `exec.CommandContext starts "git" directly`, "Git must run through internal/gitx",
		"git-exec guidance:", "runner.Run(ctx, dir, args...)")
	if strings.Contains(output, "production file(s) launch Git only") {
		t.Fatalf("output %q reports success alongside a violation", output)
	}
}

func TestRunResolvesAliasedImport(t *testing.T) {
	t.Parallel()
	source := `package pool

import osexec "os/exec"

func Prune() error { return osexec.Command("git", "gc").Run() }
`
	dirty(t, map[string]string{"internal/pool/prune.go": source}, `osexec.Command starts "git" directly`)
}

func TestRunReportsDotImportInsteadOfGuessing(t *testing.T) {
	t.Parallel()
	source := `package pool

import . "os/exec"

func Prune() error { return Command("git", "gc").Run() }
`
	dirty(t, map[string]string{"internal/pool/prune.go": source},
		"internal/pool/prune.go:3:", "os/exec is dot-imported", "import it under a name such as exec")
}

// blank importは参照名を持たないため、この検査の対象にならない。
func TestRunIgnoresBlankImport(t *testing.T) {
	t.Parallel()
	clean(t, 1, map[string]string{"internal/pool/prune.go": "package pool\n\nimport _ \"os/exec\"\n"})
}

func TestRunAcceptsShadowedIdentifier(t *testing.T) {
	t.Parallel()
	source := `package pool

import "os/exec"

type stub struct{ Command func(string, ...string) error }

func Prune(run func() error) error {
	exec := stub{}
	if exec.Command != nil {
		return exec.Command("git", "gc")
	}
	return run()
}

var _ = exec.ErrNotFound
`
	clean(t, 1, map[string]string{"internal/pool/prune.go": source})
}

func TestRunAcceptsShadowingParameterAndRangeVariable(t *testing.T) {
	t.Parallel()
	source := `package pool

import "os/exec"

type runner interface {
	Command(string, ...string) error
}

func Prune(exec runner) error { return exec.Command("git", "gc") }

func PruneAll(runners []runner) error {
	for _, exec := range runners {
		if err := exec.Command("git", "gc"); err != nil {
			return err
		}
	}
	return nil
}

var _ = exec.ErrNotFound
`
	clean(t, 1, map[string]string{"internal/pool/prune.go": source})
}

func TestRunDetectsAbsolutePathAndConstantConcatenation(t *testing.T) {
	t.Parallel()
	source := `package pool

import (
	"context"
	"os/exec"
)

const (
	gitPrefix  = "/opt/homebrew/bin/"
	gitProgram = gitPrefix + "git"
)

func Absolute() error { return exec.Command("/usr/bin/git", "gc").Run() }

func Concatenated(ctx context.Context) error { return exec.CommandContext(ctx, gitProgram, "gc").Run() }
`
	dirty(t, map[string]string{"internal/pool/prune.go": source},
		`starts "/usr/bin/git" directly`, `starts "/opt/homebrew/bin/git" directly`)
}

// 動的な値は構文だけでは解釈できないため対象外とし、LookPathは探索のみなので違反にしない。
func TestRunIgnoresDynamicProgramAndLookPath(t *testing.T) {
	t.Parallel()
	source := `package pool

import (
	"os"
	"os/exec"
)

func Dynamic(program string) error { return exec.Command(program, "gc").Run() }

func FromEnvironment() error { return exec.Command(os.Getenv("WX_GIT"), "gc").Run() }

func Search() (string, error) { return exec.LookPath("git") }
`
	clean(t, 1, map[string]string{"internal/pool/prune.go": source})
}

// 非絶対の相対パスはこの検査の対象外である。git-lfsのように接頭辞が一致するだけの名前も違反にしない。
func TestRunIgnoresRelativePathAndOtherPrograms(t *testing.T) {
	t.Parallel()
	source := `package pool

import "os/exec"

func Relative() error { return exec.Command("./git", "gc").Run() }

func Other() error { return exec.Command("git-lfs", "version").Run() }

func Launchctl() error { return exec.Command("/bin/launchctl", "list").Run() }
`
	clean(t, 1, map[string]string{"internal/pool/prune.go": source})
}

// GOOS別ファイルはbuild tagで無効な組み合わせでも構文解析し、適用漏れを防ぐ。
func TestRunInspectsPlatformSpecificFiles(t *testing.T) {
	t.Parallel()
	darwin := `//go:build darwin

package pool

import "os/exec"

func Prune() error { return exec.Command("git", "gc").Run() }
`
	dirty(t, map[string]string{"internal/pool/prune_darwin.go": darwin}, "internal/pool/prune_darwin.go:7:")
}

func TestRunExemptsTestFilesAndTheGitAdapter(t *testing.T) {
	t.Parallel()
	sources := map[string]string{
		"internal/gitx/git.go":        plainViolation,
		"internal/pool/prune_test.go": plainViolation,
		"internal/pool/prune.go":      "package pool\n",
	}
	clean(t, 1, sources)
}

// 検査範囲はcmdとinternalに限り、toolsやscripts配下は走査しない。
func TestRunSkipsDirectoriesOutsideProductionRoots(t *testing.T) {
	t.Parallel()
	sources := map[string]string{
		"tools/checkgitexec/main.go": plainViolation,
		"internal/pool/prune.go":     "package pool\n",
		"cmd/wx/main.go":             "package main\n",
	}
	clean(t, 2, sources)
}

func TestRunReportsUnparsableSource(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := run(repository(map[string]string{"internal/pool/prune.go": "package pool\n\nfunc ("}), &out)
	if err == nil || !strings.Contains(err.Error(), "parse internal/pool/prune.go") {
		t.Fatalf("err=%v", err)
	}
}

// 違反は path、次に行番号の順で並べ、報告の順序を実行ごとに揺らさない。
func TestRunSortsViolations(t *testing.T) {
	t.Parallel()
	sources := map[string]string{
		"internal/pool/prune.go": plainViolation,
		"cmd/wx/main.go":         plainViolation,
	}
	output := dirty(t, sources, "cmd/wx/main.go:9:", "internal/pool/prune.go:9:")
	if strings.Index(output, "cmd/wx/main.go") > strings.Index(output, "internal/pool/prune.go") {
		t.Fatalf("output %q is not sorted by path", output)
	}
}

// 循環するconstでも停止し、解決できない値として扱う。
func TestRunStopsOnCircularConstants(t *testing.T) {
	t.Parallel()
	source := `package pool

import "os/exec"

const first = second

const second = first

func Prune() error { return exec.Command(first, "gc").Run() }
`
	clean(t, 1, map[string]string{"internal/pool/prune.go": source})
}

// 引数の足りない呼び出しや、パッケージ以外の同名selectorで落ちない。
func TestRunToleratesUnexpectedCallShapes(t *testing.T) {
	t.Parallel()
	source := `package pool

import (
	"context"
	"os/exec"
)

func Empty() *exec.Cmd { return exec.Command("git") }

func Missing(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx) }

func Nested() error { return exec.Command("git", "gc").Run() }
`
	output := dirty(t, map[string]string{"internal/pool/prune.go": source}, "internal/pool/prune.go:8:", "internal/pool/prune.go:12:")
	if strings.Contains(output, "prune.go:10:") {
		t.Fatalf("output %q reports a call without the program argument", output)
	}
}
