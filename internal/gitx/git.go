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

	"github.com/HappyOnigiri/WX/internal/fdexec"
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
	// FDHelper is the wx executable used to fchdir to a descriptor before
	// execing Git. Production managers set it to their own executable. Keeping
	// it injectable lets tests use a built helper without weakening the
	// descriptor-bound path.
	FDHelper    string
	timeout     sync.RWMutex
	detail      sync.RWMutex
	runAt       sync.RWMutex
	beforeRunAt func([]string)
	locks       sync.Map
}

// SetBeforeRunAtHook installs a test/diagnostic barrier invoked after a
// descriptor-bound command has been constructed and immediately before its
// child starts. Production callers leave it nil. The hook is intentionally
// synchronized because descriptor-bound commands may run concurrently across
// preparation workers.
func (r *Runner) SetBeforeRunAtHook(hook func([]string)) {
	r.runAt.Lock()
	r.beforeRunAt = hook
	r.runAt.Unlock()
}

func (r *Runner) invokeBeforeRunAt(args []string) {
	r.runAt.RLock()
	hook := r.beforeRunAt
	r.runAt.RUnlock()
	if hook != nil {
		hook(append([]string(nil), args...))
	}
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
	return r.runEnvInput(ctx, dir, nil, env, input, args...)
}

// RunAt executes Git with dirFile as its current directory. The descriptor is
// inherited by a tiny wx trampoline which calls fchdir before exec. Relative
// arguments therefore resolve in the pinned inode even if its pathname is
// renamed or replaced while Git is starting.
func (r *Runner) RunAt(ctx context.Context, dirFile *os.File, env []string, input []byte, args ...string) (Result, error) {
	return r.runEnvInput(ctx, string(filepath.Separator), dirFile, env, input, args...)
}

func (r *Runner) runEnvInput(ctx context.Context, dir string, dirFile *os.File, env []string, input []byte, args ...string) (Result, error) {
	if timeout := r.GetTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		var cmd *exec.Cmd
		if dirFile == nil {
			cmd = exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dir
			cmd.Env = append(sanitizedEnviron(), env...)
		} else {
			var err error
			cmd, err = fdexec.Start(ctx, r.FDHelper, dirFile, append(sanitizedEnviron(), env...), append([]string{"git"}, args...)...)
			if err != nil {
				res := Result{ExitCode: -1, Elapsed: time.Since(start)}
				failureID := newFailureID()
				r.writeFailureDetail(failureID, args, res)
				return res, &Error{Args: append([]string(nil), args...), Result: res, FailureID: failureID}
			}
			r.invokeBeforeRunAt(args)
		}
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

// gitEnvDenylist names repository-scoped Git environment variables that must
// never leak from the daemon's own process environment into a Git child
// process. daemon startup paths (launchd session env via `launchctl setenv`,
// a manual `wx daemon serve`, or invocation from inside a Git hook) can carry
// GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE pointed at an unrelated repository.
// Without stripping them, a snapshot's `git add -A` / `write-tree` would
// silently operate on the wrong worktree or index, report success, and let
// the caller remove a dirty worktree it never actually captured. Callers that
// legitimately need one of these (for example archive's temporary
// GIT_INDEX_FILE) still set it explicitly through the env parameter, which is
// appended after this sanitized base and therefore wins.
//
// The first group is exactly what `git rev-parse --local-env-vars` reports,
// which is the same list scripts/hook-check.sh unsets before running its own
// child Git commands. It is spelled out statically here so that no Git
// invocation is needed to build a child environment;
// TestSanitizedEnvironCoversEveryLocalGitEnvironmentVariable derives the list
// from the Git binary in use and fails if an upgrade adds a name, so the two
// cannot drift apart silently. GIT_CONFIG_PARAMETERS matters as much as
// GIT_WORK_TREE does: it is how `git -c <key>=<value>` reaches child
// processes, so an inherited one can re-point core.excludesFile or
// status.showUntrackedFiles and make `git add -A` omit untracked work while
// still exiting 0.
//
// The second group is not repository-local in Git's own sense, so
// --local-env-vars does not list it, but each one still redirects where a
// child Git reads objects, refs, or configuration from.
var gitEnvDenylist = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_CONFIG":                       true,
	"GIT_CONFIG_COUNT":                 true,
	"GIT_CONFIG_PARAMETERS":            true,
	"GIT_DIR":                          true,
	"GIT_GRAFT_FILE":                   true,
	"GIT_IMPLICIT_WORK_TREE":           true,
	"GIT_INDEX_FILE":                   true,
	"GIT_NO_REPLACE_OBJECTS":           true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_PREFIX":                       true,
	"GIT_REPLACE_REF_BASE":             true,
	"GIT_SHALLOW_FILE":                 true,
	"GIT_WORK_TREE":                    true,

	"GIT_CEILING_DIRECTORIES": true,
	"GIT_CONFIG_GLOBAL":       true,
	"GIT_CONFIG_SYSTEM":       true,
	"GIT_NAMESPACE":           true,
}

// isGitConfigKeyValueVar reports whether name is one of Git's indexed
// GIT_CONFIG_KEY_<n>/GIT_CONFIG_VALUE_<n> pair used to inject config entries
// without a config file. These are numbered, so they cannot be listed in
// gitEnvDenylist by exact name.
func isGitConfigKeyValueVar(name string) bool {
	return strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_")
}

// sanitizedEnviron returns the current process environment with
// repository-scoped Git variables removed. See gitEnvDenylist for why this
// matters; it mirrors the unset loop scripts/hook-check.sh already applies to
// its own child Git invocations for the same reason.
func sanitizedEnviron() []string {
	environ := os.Environ()
	sanitized := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && (gitEnvDenylist[name] || isGitConfigKeyValueVar(name)) {
			continue
		}
		sanitized = append(sanitized, kv)
	}
	return sanitized
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
