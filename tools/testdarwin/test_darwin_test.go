// testdarwin は scripts/test-darwin.sh の前提確認と終了状態を検査するテスト専用パッケージである。
// 実装はshell scriptであり、macOS実機がなくても契約を確かめられるよう前提確認のコマンドを差し替える。
package testdarwin

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	scriptPath        = "../../scripts/test-darwin.sh"
	workspacePackage  = "./internal/workspace"
	argumentSeparator = "\x00"
)

// 前提確認で呼ばれるmacOSのコマンドとgoの代役。応答と終了コードは環境変数で決め、
// goは env の問い合わせに答え、test のときだけ引数とTMPDIRを記録する。
const (
	fakeUname = `[ "${1:-}" = -s ] || exit 64
printf '%s\n' "${FAKE_KERNEL:-Darwin}"
`
	fakeSwVers = `case "${1:-}" in
-productVersion) printf '%s\n' "${FAKE_OS_VERSION:-15.6}" ;;
-buildVersion) printf '%s\n' "${FAKE_OS_BUILD:-24G84}" ;;
*) exit 64 ;;
esac
`
	fakeDf = `printf 'Filesystem 512-blocks Used Available Capacity Mounted on\n'
[ -n "${FAKE_DF_HEADER_ONLY:-}" ] && exit 0
printf '%s 1 1 1 1%% /\n' "${FAKE_DEVICE:-/dev/disk3s5}"
`
	fakeDiskutil = `[ -n "${FAKE_DISKUTIL_FAILS:-}" ] && exit 1
printf 'plist for %s\n' "$3"
`
	fakePlutil = `cat >/dev/null
[ -n "${FAKE_PLUTIL_FAILS:-}" ] && exit 1
printf '%s\n' "${FAKE_FILESYSTEM-apfs}"
`
	fakeGo = `if [ "${1:-}" = env ]; then
  eval "printf '%s\\n' \"\${FAKE_GO_$2:-}\""
  exit 0
fi
[ "${1:-}" = test ] || exit 64
: > "$FAKE_GO_ARGS"
for argument in "$@"; do printf '%s\0' "$argument" >> "$FAKE_GO_ARGS"; done
printf '%s' "$TMPDIR" > "$FAKE_GO_TMPDIR"
exit "${FAKE_GO_STATUS:-0}"
`
)

var fakeTools = map[string]string{
	"uname":    fakeUname,
	"sw_vers":  fakeSwVers,
	"df":       fakeDf,
	"diskutil": fakeDiskutil,
	"plutil":   fakePlutil,
	"go":       fakeGo,
}

type result struct {
	status  int
	output  string
	args    []string
	tempDir string
}

// writeFakeTools はPATHへ置く代役を作り、そのディレクトリを返す。
func writeFakeTools(t *testing.T, directory string) string {
	t.Helper()
	binDirectory := filepath.Join(directory, "bin")
	if err := os.MkdirAll(binDirectory, 0o755); err != nil {
		t.Fatalf("create fake bin: %v", err)
	}
	for name, body := range fakeTools {
		path := filepath.Join(binDirectory, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	return binDirectory
}

// runDarwin はscriptを走らせ、goへ渡った引数・TMPDIR・終了状態を返す。
// environmentは FAKE_KERNEL=... のような追加の環境変数で、既定値は代役側が持つ。
func runDarwin(t *testing.T, environment ...string) result {
	t.Helper()
	directory := t.TempDir()
	binDirectory := writeFakeTools(t, directory)
	argsPath := filepath.Join(directory, "args")
	tempDirPath := filepath.Join(directory, "tmpdir")

	command := exec.Command("/bin/sh", scriptPath)
	command.Env = []string{
		"PATH=" + binDirectory + ":/usr/bin:/bin",
		"PKG=" + workspacePackage,
		"RUN=",
		"VERBOSE=1",
		"TMPDIR=" + directory,
		"FAKE_GO_ARGS=" + argsPath,
		"FAKE_GO_TMPDIR=" + tempDirPath,
		"FAKE_GO_GOHOSTOS=darwin",
		"FAKE_GO_GOHOSTARCH=arm64",
		"FAKE_GO_GOOS=darwin",
		"FAKE_GO_GOARCH=arm64",
		"FAKE_GO_GOVERSION=go1.25.1",
	}
	command.Env = append(command.Env, environment...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output

	status := 0
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run script: %v (output %q)", err, output.String())
		}
		status = exitError.ExitCode()
	}

	recorded, err := os.ReadFile(argsPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read recorded arguments: %v", err)
	}
	var args []string
	if trimmed := strings.TrimSuffix(string(recorded), argumentSeparator); trimmed != "" {
		args = strings.Split(trimmed, argumentSeparator)
	}
	temporary, err := os.ReadFile(tempDirPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read recorded TMPDIR: %v", err)
	}
	return result{status: status, output: output.String(), args: args, tempDir: string(temporary)}
}

func TestScriptRunsTheWorkspacePackageVerboselyOnAPFS(t *testing.T) {
	t.Parallel()
	got := runDarwin(t)
	if got.status != 0 {
		t.Fatalf("status=%d, want 0 (output %q)", got.status, got.output)
	}
	want := []string{"test", "-count=1", "-shuffle=on", "-v", workspacePackage}
	if strings.Join(got.args, argumentSeparator) != strings.Join(want, argumentSeparator) {
		t.Fatalf("args=%q, want %q", got.args, want)
	}
	// 実行環境とfilesystemの表示は、どのホストで確認したかを後から読み取るために必要である。
	for _, want := range []string{"macOS 15.6 (24G84)", "arch=arm64", "go=go1.25.1", "filesystem=apfs", "/dev/disk3s5"} {
		if !strings.Contains(got.output, want) {
			t.Fatalf("output %q does not mention %q", got.output, want)
		}
	}
	if !strings.Contains(got.output, "the final gate is make ci") {
		t.Fatalf("output %q does not point at the final gate", got.output)
	}
}

// filesystemを判定した場所とテストが使う場所が一致し、成功しても残らないことを確かめる。
func TestScriptRunsTheTestsInTheInspectedTemporaryDirectory(t *testing.T) {
	t.Parallel()
	got := runDarwin(t)
	if got.tempDir == "" {
		t.Fatalf("go test received no TMPDIR (output %q)", got.output)
	}
	if !strings.Contains(got.output, "tmpdir="+got.tempDir) {
		t.Fatalf("output %q does not report the inspected directory %q", got.output, got.tempDir)
	}
	if _, err := os.Stat(got.tempDir); !os.IsNotExist(err) {
		t.Fatalf("stat %q after the run: %v, want the directory to be removed", got.tempDir, err)
	}
}

func TestScriptRejectsHostsThatCannotRunTheDarwinTests(t *testing.T) {
	t.Parallel()
	for name, environment := range map[string][]string{
		"non-darwin host":     {"FAKE_KERNEL=Linux"},
		"cross-compiled GOOS": {"FAKE_GO_GOOS=linux"},
		"cross-compiled arch": {"FAKE_GO_GOARCH=amd64"},
		"non-darwin toolchain": {
			"FAKE_KERNEL=Darwin", "FAKE_GO_GOHOSTOS=linux", "FAKE_GO_GOOS=linux",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runDarwin(t, environment...)
			if got.status == 0 {
				t.Fatalf("status=0, want non-zero (output %q)", got.output)
			}
			if len(got.args) != 0 {
				t.Fatalf("go test was invoked with %q", got.args)
			}
		})
	}
}

// APFSでなければclonefileの前提が成り立たないため、テストを走らせずに失敗する必要がある。
func TestScriptRejectsATemporaryDirectoryThatIsNotAPFS(t *testing.T) {
	t.Parallel()
	got := runDarwin(t, "FAKE_FILESYSTEM=Journaled HFS+")
	if got.status == 0 {
		t.Fatalf("status=0, want non-zero (output %q)", got.output)
	}
	if len(got.args) != 0 {
		t.Fatalf("go test was invoked with %q", got.args)
	}
	if !strings.Contains(got.output, "APFS") || !strings.Contains(got.output, "Journaled HFS+") {
		t.Fatalf("output %q does not explain the unmet APFS prerequisite", got.output)
	}
}

func TestScriptRejectsAMissingFilesystemType(t *testing.T) {
	t.Parallel()
	for name, environment := range map[string][]string{
		"diskutil fails":    {"FAKE_DISKUTIL_FAILS=1"},
		"plutil fails":      {"FAKE_PLUTIL_FAILS=1"},
		"empty filesystem":  {"FAKE_FILESYSTEM="},
		"no device from df": {"FAKE_DF_HEADER_ONLY=1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := runDarwin(t, environment...)
			if got.status == 0 {
				t.Fatalf("status=0, want non-zero (output %q)", got.output)
			}
			if len(got.args) != 0 {
				t.Fatalf("go test was invoked with %q", got.args)
			}
		})
	}
}

// 表示処理や後片付けが失敗を成功へ変えないことを確かめる。
func TestScriptPropagatesTheGoExitStatusAndStillCleansUp(t *testing.T) {
	t.Parallel()
	got := runDarwin(t, "FAKE_GO_STATUS=3")
	if got.status != 3 {
		t.Fatalf("status=%d, want 3 (output %q)", got.status, got.output)
	}
	if got.tempDir == "" {
		t.Fatalf("go test received no TMPDIR (output %q)", got.output)
	}
	if _, err := os.Stat(got.tempDir); !os.IsNotExist(err) {
		t.Fatalf("stat %q after a failing run: %v, want the directory to be removed", got.tempDir, err)
	}
}

func TestScriptRejectsAMissingPackage(t *testing.T) {
	t.Parallel()
	got := runDarwin(t, "PKG=")
	if got.status == 0 {
		t.Fatalf("status=0, want non-zero (output %q)", got.output)
	}
	if !strings.Contains(got.output, "make test-darwin") {
		t.Fatalf("output %q does not point at the make target", got.output)
	}
}

// Makefileのtargetが、対象パッケージとverboseをこのscriptへ渡す形のままであることを確かめる。
func TestMakefileWiresTheDarwinTarget(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	for _, want := range []string{
		"DARWIN_TEST_PACKAGE := " + workspacePackage,
		"PKG=$(DARWIN_TEST_PACKAGE) VERBOSE=1 scripts/test-darwin.sh",
		"export VERBOSE",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("Makefile does not contain %q", want)
		}
	}
}
