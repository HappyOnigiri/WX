package daemon

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/launchd"
)

// restartKickstartTimeout bounds the launchctl invocation that replaces this
// process. It runs on a context derived from context.Background rather than the
// manager's: kickstart -k sends SIGTERM to this very process, so a manager
// context would cancel the command with its own shutdown and could kill
// launchctl before it finished asking launchd for the replacement.
const restartKickstartTimeout = 10 * time.Second

// restartQuietPeriod is how long the daemon must have been idle before a
// pending restart is issued. An in-flight count of zero is not by itself a safe
// moment: one wx invocation is a sequence of independent RPCs (Status, then
// ResolveAndLease, then RegisterAgentProcess, then Release at exit) with the
// daemon fully idle in between, so restarting at the first zero lands squarely
// inside a single user-visible operation.
const restartQuietPeriod = 5 * time.Second

// maxRestartAttempts bounds how many times a failing kickstart is retried
// before the daemon stops asking. Each retry costs one launchctl invocation on
// the 10s check, and a failure that survives this many attempts is not the
// transient kind a retry fixes.
const maxRestartAttempts = 3

// executableSnapshot identifies the daemon's own binary. The inode alone would
// miss an in-place overwrite (go build -o over a running binary keeps the
// inode), and mtime plus size alone would miss a replacement that preserved
// both, so all three are compared.
type executableSnapshot struct {
	identity string
	modTime  time.Time
	size     int64
}

func (s executableSnapshot) matches(other executableSnapshot) bool {
	return s.identity == other.identity && s.size == other.size && s.modTime.Equal(other.modTime)
}

func snapshotExecutable(path string) (executableSnapshot, error) {
	info, err := os.Stat(path)
	if err != nil {
		return executableSnapshot{}, err
	}
	identity, err := domain.FileIdentity(info)
	if err != nil {
		return executableSnapshot{}, err
	}
	return executableSnapshot{identity: identity, modTime: info.ModTime(), size: info.Size()}, nil
}

// watchExecutable records the baseline the periodic check compares against.
// A baseline that cannot be taken disables the watch: os.Executable returns the
// pathname resolved at startup and keeps returning it after the file behind it
// is replaced or removed, so a missing stat result is not evidence that the
// binary is unchanged and must not be treated as such.
func (m *Manager) watchExecutable(executable string, executableErr error) {
	if executableErr != nil {
		m.log.Warn("daemon executable is unknown; automatic restart after a binary replacement is disabled", "error", executableErr)
		return
	}
	snapshot, err := snapshotExecutable(executable)
	if err != nil {
		m.log.Warn("daemon executable could not be identified; automatic restart after a binary replacement is disabled", "path", executable, "error", err)
		return
	}
	m.mu.Lock()
	m.executablePath = executable
	m.executableBaseline = snapshot
	m.executableWatch = true
	m.mu.Unlock()
}

// detectExecutableReplacement raises the pending flag once the binary behind
// the daemon's own pathname stops matching the startup baseline. The flag is
// never lowered again: the running process keeps executing the old inode no
// matter what happens to the pathname afterwards.
func (m *Manager) detectExecutableReplacement() {
	m.mu.RLock()
	watching, path, baseline, pending := m.executableWatch, m.executablePath, m.executableBaseline, m.restartPending
	m.mu.RUnlock()
	if !watching || pending {
		return
	}
	current, err := snapshotExecutable(path)
	if err != nil {
		// install(1) renames a temporary file over the pathname, which leaves a
		// window where it does not exist. Carry the check to the next cycle
		// instead of reporting a replacement that may not have happened.
		if !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("daemon executable could not be inspected", "path", path, "error", err)
		}
		return
	}
	if current.matches(baseline) {
		return
	}
	m.mu.Lock()
	m.restartPending = true
	m.mu.Unlock()
	m.log.Info("daemon executable was replaced; restart is pending", "path", path, "baseline_identity", baseline.identity, "current_identity", current.identity)
}

// RequestRestart records an operator's explicit restart request. It only raises
// the pending flag: the restart itself still goes through restartIfReplaced, so
// an operator who ran make install cannot cut short an in-flight RPC. That
// matters because a kickstart -k landing between BeginRPCRequest and
// CompleteRPCRequest leaves the reservation PENDING, and an idempotency key
// stuck that way answers IDEMPOTENCY_INDETERMINATE for the rest of its TTL —
// the session-scoped Release keys never succeed again for that session.
func (m *Manager) RequestRestart() map[string]any {
	m.mu.Lock()
	m.restartPending = true
	m.mu.Unlock()
	m.log.Info("daemon restart was requested; it will be issued once the daemon is idle")
	return map[string]any{"restart_pending": true}
}

// restartIfReplaced restarts the daemon once the pending replacement can be
// followed without dropping work. SIGTERM does not rescue an in-flight RPC (the
// connection is closed without a response) and Manager.Close blocks until every
// root reference is released, so passing this gate is the only protection the
// callers have; the client-side connect retry only covers the listen gap that
// remains after it. The gate therefore requires more than an in-flight count of
// zero: it also waits out restartQuietPeriod since the daemon went idle, so a
// restart cannot land between two RPCs of the same wx invocation.
func (m *Manager) restartIfReplaced() {
	m.mu.Lock()
	if !m.restartPending || m.restartRequested || !m.restartGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	path, baseline := m.executablePath, m.executableBaseline
	m.mu.Unlock()
	status, err := m.store.Status(m.ctx)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Warn("pending daemon restart could not read job state", "error", err)
		}
		return
	}
	if status.Jobs > 0 {
		return
	}
	if !m.underLaunchd() {
		m.warnRestartUnmanaged()
		return
	}
	m.mu.Lock()
	if m.restartRequested || !m.restartGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	// Claim the restart before issuing it. The claim is only permanent once
	// launchd accepted the request: cmd/wx's daemon serve stops honouring
	// SIGTERM after the first one (signal.NotifyContext keeps the registration
	// but the context is already cancelled), so repeating kickstart -k after a
	// successful one would only kill the replacement launchd just started.
	m.restartRequested = true
	m.mu.Unlock()
	m.log.Info("restarting daemon after executable replacement", "path", path, "baseline_identity", baseline.identity)
	// Issue the kickstart off maintainJobs. maintainJobs is tracked by m.wg and
	// Manager.Close waits on it before it releases the root handles, so running
	// launchctl inline would make the shutdown that this very command triggers
	// wait for the command that triggered it. The listener is already closed by
	// then, so every millisecond spent there widens the window where clients
	// cannot connect at all.
	go m.issueRestart(path)
}

// issueRestart runs the launchctl invocation that replaces this process. Its
// context is derived from context.Background rather than the manager's, because
// kickstart -k sends SIGTERM here and a manager context would cancel the
// command with its own shutdown, possibly before launchd had the request.
func (m *Manager) issueRestart(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), restartKickstartTimeout)
	defer cancel()
	err := m.kickstartService(ctx)
	if err == nil {
		return
	}
	// A kickstart that failed never delivered a SIGTERM, so the reasoning above
	// does not apply and this process is still running the replaced binary.
	// Release the claim so the next tick retries, but give up after a bounded
	// number of attempts rather than calling launchctl every 10 seconds for the
	// rest of the daemon's life.
	m.mu.Lock()
	m.restartAttempts++
	attempts := m.restartAttempts
	exhausted := attempts >= maxRestartAttempts
	m.restartRequested = exhausted
	m.mu.Unlock()
	if exhausted {
		m.log.Error("daemon restart after executable replacement failed and will not be retried; restart wx daemon manually", "path", path, "attempts", attempts, "error", err)
		return
	}
	m.log.Warn("daemon restart after executable replacement failed; retrying on the next check", "path", path, "attempts", attempts, "error", err)
}

// restartGateOpenLocked reports whether the daemon is idle enough to be
// replaced. A zero lastRequestEnd means no RPC has been served yet, so there is
// no operation in progress to cut short. m.mu must be held.
func (m *Manager) restartGateOpenLocked() bool {
	if m.inflightRequests > 0 {
		return false
	}
	return m.lastRequestEnd.IsZero() || time.Since(m.lastRequestEnd) >= restartQuietPeriod
}

func (m *Manager) kickstartService(ctx context.Context) error {
	m.mu.RLock()
	kickstart := m.kickstart
	m.mu.RUnlock()
	if kickstart == nil {
		kickstart = launchd.Kickstart
	}
	return kickstart(ctx)
}

// underLaunchd reports whether launchd owns this process. A manually started
// wx daemon serve must not kickstart itself: launchd would start a second
// daemon, that one would lose the socket lock and exit, and the manual process
// would survive still running the replaced binary.
func (m *Manager) underLaunchd() bool {
	m.mu.RLock()
	managed := m.launchdManaged
	m.mu.RUnlock()
	if managed == nil {
		managed = launchdManagedProcess
	}
	return managed()
}

func launchdManagedProcess() bool {
	return os.Getppid() == 1 && os.Getenv("XPC_SERVICE_NAME") == launchd.Label
}

func (m *Manager) warnRestartUnmanaged() {
	m.mu.Lock()
	warned := m.restartUnmanaged
	m.restartUnmanaged = true
	path := m.executablePath
	m.mu.Unlock()
	if warned {
		return
	}
	m.log.Warn("daemon executable was replaced but this daemon is not managed by launchd; restart it manually", "path", path)
}

// beginRequest and endRequest bracket every RPC the manager serves. The RPC
// server starts each connection in its own goroutine without any accounting of
// its own, so the restart gate counts handlers here instead. Both tolerate a
// nil manager because Handler dispatches parameter decoding before it reaches
// one, and the decoding contract is exercised without a manager at all.
func (m *Manager) beginRequest() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests++
	m.mu.Unlock()
}

func (m *Manager) endRequest() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests--
	// The quiet period is measured from the moment the daemon actually went
	// idle, so the stamp is taken only when the count reaches zero: a nested or
	// concurrent request that is still running has not ended anything.
	if m.inflightRequests == 0 {
		m.lastRequestEnd = time.Now()
	}
	m.mu.Unlock()
}
