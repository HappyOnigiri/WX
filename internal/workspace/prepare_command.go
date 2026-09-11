package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/fdexec"
	"github.com/HappyOnigiri/WX/internal/state"
)

// PrepareCommandError は prepare command 自身が返した失敗を表す。
// command は副作用を持ち得るため、呼び出し側はこのエラーを根拠なく再試行してはならない。
type PrepareCommandError struct {
	FailureID  string
	DetailPath string
	ExitCode   int
	TimedOut   bool
	Canceled   bool
	Err        error
}

func (e *PrepareCommandError) Error() string {
	if e == nil {
		return "prepare command failed"
	}
	detail := e.DetailPath
	if detail == "" {
		detail = "unavailable"
	}
	return fmt.Sprintf("prepare command failed: failure_id=%s detail_path=%s exit_code=%d timed_out=%t canceled=%t: %v", e.FailureID, detail, e.ExitCode, e.TimedOut, e.Canceled, e.Err)
}

func (e *PrepareCommandError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is は command timeout/cancel を親 context の終了理由として判別できるようにする。
func (e *PrepareCommandError) Is(target error) bool {
	if e == nil {
		return false
	}
	return e.TimedOut && errors.Is(target, context.DeadlineExceeded) || e.Canceled && errors.Is(target, context.Canceled)
}

const maxPrepareDiagnosticOutput = 128 << 10

type prepareDiagnostic struct {
	file       *os.File
	temporary  string
	final      string
	failureID  string
	mu         sync.Mutex
	used       int
	truncated  map[string]bool
	writeError error
}

type prepareDiagnosticWriter struct {
	diagnostic *prepareDiagnostic
	stream     string
}

func (w prepareDiagnosticWriter) Write(data []byte) (int, error) {
	if w.diagnostic == nil {
		return len(data), nil
	}
	w.diagnostic.mu.Lock()
	defer w.diagnostic.mu.Unlock()
	if w.diagnostic.file == nil {
		return len(data), nil
	}
	if w.diagnostic.used >= maxPrepareDiagnosticOutput {
		w.diagnostic.truncated[w.stream] = true
		return len(data), nil
	}
	remaining := maxPrepareDiagnosticOutput - w.diagnostic.used
	prefix := []byte("[" + w.stream + "] ")
	if remaining <= len(prefix) {
		w.diagnostic.truncated[w.stream] = true
		return len(data), nil
	}
	writeData := data
	if len(writeData) > remaining-len(prefix) {
		writeData = writeData[:remaining-len(prefix)]
		w.diagnostic.truncated[w.stream] = true
	}
	if len(writeData) > 0 {
		if _, err := w.diagnostic.file.Write(prefix); err != nil {
			w.diagnostic.writeError = err
			return len(data), nil
		}
		written, err := w.diagnostic.file.Write(writeData)
		w.diagnostic.used += len(prefix) + written
		if err != nil {
			w.diagnostic.writeError = err
		}
	}
	return len(data), nil
}

func newPrepareFailureID() string {
	id, err := domain.NewID()
	if err != nil {
		return "unknown"
	}
	return id
}

func (p *Preparer) startPrepareDiagnostic(command []string) *prepareDiagnostic {
	diagnostic := &prepareDiagnostic{failureID: newPrepareFailureID(), truncated: map[string]bool{}}
	if p.DetailDir == "" || !filepath.IsAbs(p.DetailDir) {
		return diagnostic
	}
	if err := os.MkdirAll(p.DetailDir, 0o700); err != nil {
		return diagnostic
	}
	_ = os.Chmod(p.DetailDir, 0o700)
	diagnostic.temporary = filepath.Join(p.DetailDir, ".prepare-"+diagnostic.failureID+".tmp")
	diagnostic.final = filepath.Join(p.DetailDir, diagnostic.failureID+".log")
	file, err := os.OpenFile(diagnostic.temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return diagnostic
	}
	diagnostic.file = file
	diagnostic.writeHeader(command)
	return diagnostic
}

func (d *prepareDiagnostic) writeHeader(command []string) {
	if d == nil || d.file == nil {
		return
	}
	quoted := make([]string, len(command))
	for i, argument := range command {
		quoted[i] = strconv.Quote(argument)
	}
	_, _ = fmt.Fprintf(d.file, "failure_id: %s\ncommand: %s\n", d.failureID, strings.Join(quoted, " "))
}

func (d *prepareDiagnostic) writeErrorMessage(err error) {
	if d == nil || d.file == nil || err == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = fmt.Fprintf(d.file, "start_error: %v\n", err)
}

func (d *prepareDiagnostic) finish(success bool, exitCode int, timedOut, canceled bool) string {
	if d == nil || d.file == nil {
		return ""
	}
	d.mu.Lock()
	_, _ = fmt.Fprintf(d.file, "exit_code: %d\ntimed_out: %t\ncanceled: %t\noutput_truncated: %t\n", exitCode, timedOut, canceled, d.truncated["stdout"] || d.truncated["stderr"])
	if d.writeError != nil {
		_, _ = fmt.Fprintf(d.file, "capture_error: %v\n", d.writeError)
	}
	_ = d.file.Sync()
	_ = d.file.Close()
	d.file = nil
	d.mu.Unlock()
	if success {
		_ = os.Remove(d.temporary)
		return ""
	}
	_ = os.Chmod(d.temporary, 0o600)
	if err := os.Rename(d.temporary, d.final); err == nil {
		_ = os.Chmod(d.final, 0o600)
		return d.final
	}
	return d.temporary
}

func (p *Preparer) runPrepareWithIdentity(ctx context.Context, repo discovery.Repository, target, expectedIdentity string) error {
	override, ok := p.Config.Repositories[string(repo.MainPath)]
	if !ok || len(override.Prepare.Command) == 0 {
		return nil
	}
	diagnostic := p.startPrepareDiagnostic(override.Prepare.Command)
	timeout := override.Prepare.Timeout.Duration
	if timeout < 0 {
		err := errors.New("prepare timeout must not be negative")
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: err}
	}
	if timeout <= 0 {
		timeout = p.Config.ReadinessForRepository(string(repo.MainPath)).Timeout.Duration
	}
	if timeout <= 0 {
		err := errors.New("prepare timeout must be positive")
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: err}
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	root, err := config.ExpandHome(p.Config.Storage.WorktreeRoot)
	if err != nil {
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: fmt.Errorf("open prepare command directory: %w", err)}
	}
	owner, relative, closeOwner, err := p.openOwnedRoot(root, target)
	if err != nil {
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: fmt.Errorf("open prepare command directory: %w", err)}
	}
	defer closeOwner()
	directory, identity, err := domain.OpenDirectoryAt(owner, relative)
	if err != nil {
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: fmt.Errorf("open prepare command directory: %w", err)}
	}
	defer func() { _ = directory.Close() }()
	if expectedIdentity != "" && identity != expectedIdentity {
		err := fmt.Errorf("%w: worktree target identity changed before prepare command (expected %s, got %s)", state.ErrOwnership, expectedIdentity, identity)
		// ownership failure is not a command failure; preserve the existing quarantine path and do not retain a successful command artifact.
		_ = diagnostic.finish(true, -1, false, false)
		return err
	}
	cmd, err := fdexec.Start(cctx, p.Git.FDHelper, directory, os.Environ(), override.Prepare.Command...)
	if err != nil {
		diagnostic.writeErrorMessage(err)
		path := diagnostic.finish(false, -1, false, false)
		return &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: -1, Err: err}
	}
	cmd.Stdout = prepareDiagnosticWriter{diagnostic: diagnostic, stream: "stdout"}
	cmd.Stderr = prepareDiagnosticWriter{diagnostic: diagnostic, stream: "stderr"}
	runErr := cmd.Run()
	timedOut := errors.Is(cctx.Err(), context.DeadlineExceeded)
	canceled := !timedOut && errors.Is(cctx.Err(), context.Canceled)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if runErr != nil {
		path := diagnostic.finish(false, exitCode, timedOut, canceled)
		failure := &PrepareCommandError{FailureID: diagnostic.failureID, DetailPath: path, ExitCode: exitCode, TimedOut: timedOut, Canceled: canceled, Err: runErr}
		if expectedIdentity != "" {
			if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
				return fmt.Errorf("worktree target identity changed during prepare command: %w", identityErr)
			}
		}
		return failure
	}
	_ = diagnostic.finish(true, exitCode, false, false)
	if expectedIdentity != "" {
		if identityErr := p.VerifyWorktreeIdentity(target, expectedIdentity); identityErr != nil {
			return fmt.Errorf("worktree target identity changed during prepare command: %w", identityErr)
		}
	}
	return nil
}
