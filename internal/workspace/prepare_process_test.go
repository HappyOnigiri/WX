package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// descendantLifetime は descendant が出力 pipe を保持し続ける時間である。
// 返却が descendant の生存に引きずられているかを、待ち時間の大小だけで判別できる長さにする。
const descendantLifetime = "30"

// prepareReturnBound は timeout/cancel 後の返却に許す上限である。
// 負荷のかかった実行環境でも余裕を持たせつつ、descendantLifetime とは桁で離す。
const prepareReturnBound = 5 * time.Second

func newDescendantPrepare(t *testing.T, command []string, timeout time.Duration) (Preparer, discovery.Repository, string) {
	t.Helper()
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	target := filepath.Join(root, "target")
	if err := os.Mkdir(repository, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Storage.WorktreeRoot = root
	cfg.Repositories = map[string]config.Repository{repository: {Prepare: config.Prepare{Command: command, Timeout: &config.Duration{Duration: timeout}}}}
	owner, _, err := domain.OpenOwnedRoot(root, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	preparer := Preparer{Git: &gitx.Runner{Timeout: time.Second}, Config: cfg, DetailDir: t.TempDir(), OwnedRoot: owner, RootPath: root}
	return preparer, discovery.Repository{MainPath: domain.CanonicalPath(repository)}, target
}

// 継承した出力 pipe を保持する descendant が残っていても、timeout は有界な時間で返らなければならない。
func TestPrepareCommandTimeoutReturnsWhileDescendantHoldsOutputPipe(t *testing.T) {
	t.Parallel()
	preparer, repo, target := newDescendantPrepare(t, []string{"/bin/sh", "-c", "sleep " + descendantLifetime + " & sleep " + descendantLifetime}, 25*time.Millisecond)
	started := time.Now()
	err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	elapsed := time.Since(started)
	var failure *PrepareCommandError
	if !errors.As(err, &failure) || !failure.TimedOut || failure.Canceled {
		t.Fatalf("timed out prepare error=%v typed=%+v", err, failure)
	}
	if elapsed > prepareReturnBound {
		t.Fatalf("timeout waited for the descendant holding the output pipe: elapsed=%v", elapsed)
	}
}

// 親 context の cancel も同様に、descendant の生存へ引きずられずに返らなければならない。
func TestPrepareCommandCancelReturnsWhileDescendantHoldsOutputPipe(t *testing.T) {
	t.Parallel()
	preparer, repo, target := newDescendantPrepare(t, []string{"/bin/sh", "-c", "sleep " + descendantLifetime + " & sleep " + descendantLifetime}, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	started := time.Now()
	err := preparer.runPrepareWithIdentity(ctx, repo, target, "")
	elapsed := time.Since(started)
	var failure *PrepareCommandError
	if !errors.As(err, &failure) || failure.TimedOut || !failure.Canceled {
		t.Fatalf("canceled prepare error=%v typed=%+v", err, failure)
	}
	if elapsed > prepareReturnBound {
		t.Fatalf("cancel waited for the descendant holding the output pipe: elapsed=%v", elapsed)
	}
}

// 起動前に context が中断されていて process state が無い場合も、exit code は未知値として診断する。
func TestPrepareCommandCanceledBeforeStartReportsUnknownExitCode(t *testing.T) {
	t.Parallel()
	preparer, repo, target := newDescendantPrepare(t, []string{"/usr/bin/true"}, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := preparer.runPrepareWithIdentity(ctx, repo, target, "")
	var failure *PrepareCommandError
	if !errors.As(err, &failure) || !failure.Canceled || failure.TimedOut || failure.ExitCode != -1 {
		t.Fatalf("pre-start cancellation error=%v typed=%+v", err, failure)
	}
	assertPrepareDetailExitCode(t, failure.DetailPath, -1)
}

// timeout では command process group ごと終了させ、descendant を worktree に残さない。
func TestPrepareCommandTimeoutTerminatesDescendants(t *testing.T) {
	t.Parallel()
	preparer, repo, target := newDescendantPrepare(t, []string{"/bin/sh", "-c", "(sleep 0.5; : > survivor) & sleep " + descendantLifetime}, 25*time.Millisecond)
	err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	var failure *PrepareCommandError
	if !errors.As(err, &failure) || !failure.TimedOut {
		t.Fatalf("timed out prepare error=%v typed=%+v", err, failure)
	}
	// descendant が生き残っていれば 0.5 秒後に marker を作る。十分に過ぎてから不在を確かめる。
	time.Sleep(2 * time.Second)
	if _, statErr := os.Stat(filepath.Join(target, "survivor")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descendant survived the prepare timeout: err=%v", statErr)
	}
}

// process group を抜けた descendant が write 端を保持し続けても、回収は猶予で打ち切る。
// 保持者を残したまま finish を呼ぶことで、その状況を process を使わずに再現する。
func TestPrepareOutputCaptureFinishStopsAtGrace(t *testing.T) {
	t.Parallel()
	details := t.TempDir()
	temporary := filepath.Join(details, "prepare.tmp")
	file, err := os.Create(temporary)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic := &prepareDiagnostic{file: file, temporary: temporary, final: filepath.Join(details, "prepare.log"), truncated: map[string]bool{}}
	capture, err := newPrepareOutputCapture(&exec.Cmd{}, diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.closeWriters()
	if _, err := capture.writers[0].Write([]byte("captured before the grace\n")); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if capture.finish(20 * time.Millisecond) {
		t.Fatal("finish reported a complete drain while a writer was still open")
	}
	if elapsed := time.Since(started); elapsed > prepareReturnBound {
		t.Fatalf("finish waited past the grace: elapsed=%v", elapsed)
	}
	diagnostic.markCaptureIncomplete()
	if path := diagnostic.finish(false, -1, true, false); path == "" {
		t.Fatal("diagnostic finish returned no path")
	} else if detail, readErr := os.ReadFile(path); readErr != nil || !strings.Contains(string(detail), "output_truncated: true") {
		t.Fatalf("incomplete capture was not recorded detail=%q err=%v", detail, readErr)
	}
}

// 正常終了した command の出力は、回収を cmd.Wait から切り離した後も診断ログへ残る。
func TestPrepareCommandRetainsOutputWithDetachedCapture(t *testing.T) {
	t.Parallel()
	preparer, repo, target := newDescendantPrepare(t, []string{"/bin/sh", "-c", "printf 'unique stdout line\\n'; printf 'unique stderr line\\n' >&2; exit 5"}, time.Minute)
	err := preparer.runPrepareWithIdentity(context.Background(), repo, target, "")
	var failure *PrepareCommandError
	if !errors.As(err, &failure) || failure.ExitCode != 5 {
		t.Fatalf("prepare error=%v typed=%+v", err, failure)
	}
	detail, readErr := os.ReadFile(failure.DetailPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, want := range []string{"[stdout] unique stdout line", "[stderr] unique stderr line", "output_truncated: false"} {
		if !strings.Contains(string(detail), want) {
			t.Fatalf("detail did not retain %q: %s", want, detail)
		}
	}
}
