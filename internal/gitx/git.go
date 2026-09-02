package gitx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Result struct {
	Stdout, Stderr string
	ExitCode       int
	Elapsed        time.Duration
}
type Error struct {
	Args      []string
	Result    Result
	FailureID string
}

func (e *Error) Error() string {
	command := "command"
	if len(e.Args) > 0 {
		command = e.Args[0]
	}
	return fmt.Sprintf("git %s failed with exit %d (failure %s)", command, e.Result.ExitCode, e.FailureID)
}

type Runner struct {
	Timeout   time.Duration
	DetailDir string
	timeout   sync.RWMutex
	detail    sync.RWMutex
	locks     sync.Map
}

func (r *Runner) SetDetailDir(path string) {
	r.detail.Lock()
	r.DetailDir = path
	r.detail.Unlock()
}

func (r *Runner) getDetailDir() string {
	r.detail.RLock()
	defer r.detail.RUnlock()
	return r.DetailDir
}

func (r *Runner) SetTimeout(timeout time.Duration) {
	r.timeout.Lock()
	r.Timeout = timeout
	r.timeout.Unlock()
}

func (r *Runner) GetTimeout() time.Duration {
	r.timeout.RLock()
	defer r.timeout.RUnlock()
	return r.Timeout
}

func (r *Runner) Run(ctx context.Context, dir string, args ...string) (Result, error) {
	return r.RunEnvInput(ctx, dir, nil, nil, args...)
}

func (r *Runner) RunEnv(ctx context.Context, dir string, env []string, args ...string) (Result, error) {
	return r.RunEnvInput(ctx, dir, env, nil, args...)
}

func (r *Runner) RunEnvInput(ctx context.Context, dir string, env []string, input []byte, args ...string) (Result, error) {
	if timeout := r.GetTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if input != nil {
			cmd.Stdin = bytes.NewReader(input)
		}
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		res := Result{Stdout: out.String(), Stderr: stderr.String(), Elapsed: time.Since(start)}
		if err == nil {
			return res, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
		if attempt >= 3 || !isLockConflict(res.Stderr) {
			failureID := newFailureID()
			r.writeFailureDetail(failureID, args, res)
			return res, &Error{Args: append([]string(nil), args...), Result: res, FailureID: failureID}
		}
		delay := time.Duration(25*(1<<attempt)) * time.Millisecond
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func newFailureID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

func (r *Runner) writeFailureDetail(id string, args []string, result Result) {
	detailDir := r.getDetailDir()
	if detailDir == "" {
		return
	}
	if err := os.MkdirAll(detailDir, 0o700); err != nil {
		return
	}
	_ = os.Chmod(detailDir, 0o700)
	quotedArgs := make([]string, len(args))
	for index, argument := range args {
		quotedArgs[index] = strconv.Quote(argument)
	}
	detail := fmt.Sprintf("command: git %s\nexit_status: %d\nelapsed: %s\nstderr:\n%s", strings.Join(quotedArgs, " "), result.ExitCode, result.Elapsed, result.Stderr)
	_ = os.WriteFile(filepath.Join(detailDir, id+".log"), []byte(detail), 0o600)
}

func isLockConflict(stderr string) bool {
	message := strings.ToLower(stderr)
	return strings.Contains(message, "could not lock") || strings.Contains(message, "unable to create") && strings.Contains(message, ".lock") || strings.Contains(message, "another git process")
}

func (r *Runner) WithCommonDirLock(common string, fn func() error) error {
	v, _ := r.locks.LoadOrStore(common, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	return fn()
}

func ResolveRef(ctx context.Context, r *Runner, repo, branch string) (string, bool, error) {
	for _, ref := range []string{"refs/heads/" + branch, "refs/remotes/origin/" + branch} {
		res, err := r.Run(ctx, repo, "rev-parse", "--verify", ref+"^{commit}")
		if err == nil {
			return strings.TrimSpace(res.Stdout), true, nil
		}
		var ge *Error
		if !errors.As(err, &ge) {
			return "", false, err
		}
	}
	return "", false, nil
}

func IsDetached(ctx context.Context, r *Runner, dir string) bool {
	_, err := r.Run(ctx, dir, "symbolic-ref", "-q", "HEAD")
	return err != nil
}
