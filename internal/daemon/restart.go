package daemon

import (
	"context"
	"errors"
	"os"
	"syscall"
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

// lifecycleQuietPeriod is how long the daemon must have been idle before a
// pending restart or stop is issued. An in-flight count of zero is not by
// itself a safe moment: one wx invocation is a sequence of independent RPCs
// (Status, then ResolveAndLease, then RegisterAgentProcess, then Release at
// exit) with the daemon fully idle in between, so acting at the first zero
// lands squarely inside a single user-visible operation.
const lifecycleQuietPeriod = 5 * time.Second

// lifecycleReplyGrace keeps the gate shut for a moment after a lifecycle
// request left Handler.Handle. rpc.Server writes the response frame after the
// handler returns, so the in-flight bracket is already released while the reply
// is still unwritten, and RequestStop wakes the check that could open the gate
// from inside that same bracket. A signal delivered in that window would reach
// the caller as a dropped connection instead of as the accepted stop it is,
// which is the same hazard degradedStopDelay exists for.
const lifecycleReplyGrace = 100 * time.Millisecond

// maxLifecycleAttempts bounds how many times a failing kickstart or stop
// signal is retried before the daemon stops asking. A failure that survives
// this many attempts is not the transient kind a retry fixes.
const maxLifecycleAttempts = 3

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
// matter what happens to the pathname afterwards. A pending stop suppresses
// the detection entirely, because an operator who asked this daemon to exit
// must not have that request quietly turned into a restart.
func (m *Manager) detectExecutableReplacement() {
	m.mu.RLock()
	watching, path, baseline := m.executableWatch, m.executablePath, m.executableBaseline
	pending := m.restartPending || m.stopPending
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
	// The lock was not held across the stat, so a request may have raised an
	// intent meanwhile. Raising this one on top would put two intents on the
	// gate at once, and an operator's explicit stop would lose to a restart
	// nobody asked for.
	if m.restartPending || m.stopPending {
		m.mu.Unlock()
		return
	}
	m.restartPending = true
	m.mu.Unlock()
	m.log.Info("daemon executable was replaced; restart is pending", "path", path, "baseline_identity", baseline.identity, "current_identity", current.identity)
}

// RequestRestart records an operator's explicit restart request. It only raises
// the pending flag: the restart itself still goes through runPendingLifecycle,
// so an operator who ran make install cannot cut short an in-flight RPC. That
// matters because a kickstart -k landing between BeginRPCRequest and
// CompleteRPCRequest leaves the reservation PENDING, and an idempotency key
// stuck that way answers IDEMPOTENCY_INDETERMINATE for the rest of its TTL —
// the session-scoped Release keys never succeed again for that session.
//
// A restart request supersedes a pending stop and vice versa, so at most one
// intent is ever waiting on the gate and the daemon never has to rank them.
func (m *Manager) RequestRestart(ctx context.Context) map[string]any {
	m.mu.Lock()
	if m.lifecycleSignalDeliveredLocked() {
		stopping, restarting := m.stopPending, m.restartPending
		m.mu.Unlock()
		m.log.Warn("daemon restart was requested while another lifecycle action was already under way")
		return m.conflictReply(ctx, stopping, restarting)
	}
	already := m.restartPending
	m.restartPending = true
	m.stopPending = false
	m.resetLifecycleRetriesLocked()
	m.mu.Unlock()
	m.notifyLifecycleCheck()
	m.log.Info("daemon restart was requested; it will be issued once the daemon is idle")
	reply := m.lifecycleSnapshot(ctx)
	reply["restart_pending"] = true
	reply["already_pending"] = already
	return reply
}

// RequestStop records an operator's explicit stop request. It goes through the
// same idle gate as a restart, because SIGTERM does not rescue an in-flight
// RPC either: the RPC server closes the connection without a response, so the
// gate is the only thing standing between wx daemon stop and a half-finished
// reservation.
func (m *Manager) RequestStop(ctx context.Context) map[string]any {
	m.mu.Lock()
	if m.lifecycleSignalDeliveredLocked() {
		stopping, restarting := m.stopPending, m.restartPending
		m.mu.Unlock()
		m.log.Warn("daemon stop was requested while another lifecycle action was already under way")
		return m.conflictReply(ctx, stopping, restarting)
	}
	already := m.stopPending
	m.stopPending = true
	m.restartPending = false
	m.resetLifecycleRetriesLocked()
	m.mu.Unlock()
	m.notifyLifecycleCheck()
	m.log.Info("daemon stop was requested; it will be issued once the daemon is idle")
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = true
	reply["already_pending"] = already
	return reply
}

// conflictReply answers a lifecycle request that arrived after the other
// intent had already been handed to its signal. That signal cannot be called
// back, so the reply names the action actually under way rather than letting
// the caller believe its own request superseded it.
func (m *Manager) conflictReply(ctx context.Context, stopping, restarting bool) map[string]any {
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = stopping
	reply["restart_pending"] = restarting
	reply["conflict"] = true
	return reply
}

// RequestStart is what wx daemon start sends to a daemon that already answers
// the socket. Its only job is to call back a stop that has not been handed to
// its signal yet: a stop whose gate never opened stays pending after the
// caller gave up waiting, and without this the daemon would exit minutes after
// a later start reported "already running". A stop whose SIGTERM is already on
// its way cannot be called back, so the reply says the daemon is still
// stopping and leaves the caller to wait out the exit and ask launchd for a
// fresh daemon.
func (m *Manager) RequestStart(ctx context.Context) map[string]any {
	m.mu.Lock()
	cancelled := m.stopPending && !m.lifecycleSignalDeliveredLocked()
	if cancelled {
		m.stopPending = false
		m.resetLifecycleRetriesLocked()
	}
	stopping := m.stopPending
	m.mu.Unlock()
	if cancelled {
		m.log.Info("pending daemon stop was cancelled by a start request")
	}
	reply := m.lifecycleSnapshot(ctx)
	reply["stop_pending"] = stopping
	reply["stop_cancelled"] = cancelled
	return reply
}

// lifecycleSnapshot describes what the idle gate is still waiting for, so a
// synchronous wx daemon stop/restart that runs out of patience can say which
// condition held it up instead of only reporting a timeout. The caller cannot
// sample this itself while it waits: every Status call restamps
// lastRequestEnd, which would hold the quiet period open for as long as the
// polling lasted.
//
// inflight_requests counts only the requests the quiet period is there to
// protect. Handler.Handle brackets the lifecycle RPC being answered too, so
// reporting the raw counter would tell every operator that a request was in
// flight even on a completely idle daemon.
//
// launchd_managed answers the one question a caller cannot ask from outside:
// a restart is only ever issued under launchd, so a caller that waits for a
// replacement from a daemon started by hand would sit out the whole budget for
// something that is never coming.
func (m *Manager) lifecycleSnapshot(ctx context.Context) map[string]any {
	m.mu.RLock()
	inflight := m.inflightRequests - m.inflightLifecycle
	lastEnd := m.lastRequestEnd
	m.mu.RUnlock()
	remaining := time.Duration(0)
	if !lastEnd.IsZero() {
		if elapsed := time.Since(lastEnd); elapsed < lifecycleQuietPeriod {
			remaining = lifecycleQuietPeriod - elapsed
		}
	}
	jobs := 0
	if status, err := m.store.Status(ctx); err == nil {
		jobs = status.Jobs
	}
	return map[string]any{
		"pid":                       os.Getpid(),
		"launchd_managed":           m.underLaunchd(),
		"inflight_requests":         inflight,
		"queued_jobs":               jobs,
		"quiet_period_remaining_ms": remaining.Milliseconds(),
	}
}

// notifyLifecycleCheck wakes maintainJobs so a request does not have to wait
// out the 10s job tick before the gate is looked at for the first time.
func (m *Manager) notifyLifecycleCheck() {
	select {
	case m.lifecycleChecks <- struct{}{}:
	default:
	}
}

// lifecyclePending reports whether a stop or restart is still waiting on the
// gate. maintainJobs uses it to decide whether to keep re-checking on the
// short lifecycle interval instead of the 10s job tick.
func (m *Manager) lifecyclePending() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return (m.restartPending || m.stopPending) && !m.lifecycleClaimed
}

// runPendingLifecycle carries out a pending restart or stop once it can be
// followed without dropping work. SIGTERM does not rescue an in-flight RPC (the
// connection is closed without a response) and Manager.Close blocks until every
// root reference is released, so passing this gate is the only protection the
// callers have; the client-side connect retry only covers the listen gap that
// remains after it. The gate therefore requires more than an in-flight count of
// zero: it also waits out lifecycleQuietPeriod since the daemon went idle, so
// neither action can land between two RPCs of the same wx invocation.
//
// A stop does not require launchd. wx daemon stop must work against a daemon
// started by hand with wx daemon start --foreground, and unlike a kickstart a
// SIGTERM to this very process cannot start a second daemon by mistake.
func (m *Manager) runPendingLifecycle() {
	m.mu.Lock()
	stop, restart := m.stopPending, m.restartPending
	if (!stop && !restart) || m.lifecycleClaimed || !m.lifecycleGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	path, baseline := m.executablePath, m.executableBaseline
	m.mu.Unlock()
	status, err := m.store.Status(m.ctx)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Warn("pending daemon lifecycle action could not read job state", "error", err)
		}
		return
	}
	if status.Jobs > 0 {
		return
	}
	if restart && !m.underLaunchd() {
		m.warnRestartUnmanaged()
		return
	}
	m.mu.Lock()
	// The lock was not held across the job query, so the intent read at the
	// top may have been superseded meanwhile. Issuing the stale one would send
	// the opposite signal to the request that was just answered, so the
	// evaluation is dropped and the new intent gets its own pass.
	if !m.lifecycleIntentUnchangedLocked(stop, restart) {
		m.mu.Unlock()
		m.notifyLifecycleCheck()
		return
	}
	if m.lifecycleClaimed || !m.lifecycleGateOpenLocked() {
		m.mu.Unlock()
		return
	}
	// Claim the action before issuing it. The claim is only permanent once the
	// signal was delivered: cmd/wx's daemon start --foreground stops honouring
	// SIGTERM after the first one (signal.NotifyContext keeps the registration
	// but the context is already cancelled), so repeating kickstart -k after a
	// successful one would only kill the replacement launchd just started.
	m.lifecycleClaimed = true
	m.mu.Unlock()
	// Issue the signal off maintainJobs. maintainJobs is tracked by m.wg and
	// Manager.Close waits on it before it releases the root handles, so running
	// launchctl inline would make the shutdown that this very command triggers
	// wait for the command that triggered it. The listener is already closed by
	// then, so every millisecond spent there widens the window where clients
	// cannot connect at all.
	if stop {
		m.log.Info("stopping daemon on request")
		go m.issueStop()
		return
	}
	m.log.Info("restarting daemon", "path", path, "baseline_identity", baseline.identity)
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
	attempts, exhausted := m.releaseLifecycleClaim()
	if exhausted {
		m.log.Error("daemon restart failed and will not be retried; restart wx daemon manually", "path", path, "attempts", attempts, "error", err)
		return
	}
	m.log.Warn("daemon restart failed; retrying on the next check", "path", path, "attempts", attempts, "error", err)
}

// issueStop asks this process to shut down the way launchd's own SIGTERM
// would, so the exit runs through cmd/wx's signal.NotifyContext and
// Manager.Close rather than dropping the socket where it stands. The exit code
// is 0, which is what keeps the LaunchAgent's KeepAlive{SuccessfulExit:false}
// from immediately starting a replacement.
func (m *Manager) issueStop() {
	err := m.terminateSelf()
	if err == nil {
		return
	}
	attempts, exhausted := m.releaseLifecycleClaim()
	if exhausted {
		m.log.Error("daemon stop failed and will not be retried; stop wx daemon manually", "attempts", attempts, "error", err)
		return
	}
	m.log.Warn("daemon stop failed; retrying on the next check", "attempts", attempts, "error", err)
}

// releaseLifecycleClaim hands the claim back so the next check retries an
// action whose signal was never delivered, but gives up after a bounded number
// of attempts rather than calling launchctl every second for the rest of the
// daemon's life. An exhausted claim parks the pending intent until an operator
// asks again: resetLifecycleRetriesLocked lifts it for the next explicit
// request, so a failing launchctl cannot leave the daemon unable to stop.
// resetLifecycleRetriesLocked gives an explicitly re-requested intent a fresh
// attempt budget. The exhausted latch is shared by both intents, so without
// this a restart whose launchctl call failed maxLifecycleAttempts times would
// park every later stop too — and a stop is a SIGTERM to this very process,
// about which the failure of an external launchctl says nothing. A claim that
// is not exhausted belongs to a signal that was already delivered and is left
// alone: not repeating it is the whole point of holding it. m.mu must be held.
func (m *Manager) resetLifecycleRetriesLocked() {
	if m.lifecycleSignalDeliveredLocked() {
		return
	}
	m.lifecycleAttempts = 0
	m.lifecycleClaimed = false
}

// lifecycleSignalDeliveredLocked distinguishes the two states a raised claim
// can be in: a signal that was actually delivered and is carrying the intent
// out, or the latch releaseLifecycleClaim leaves once the attempt budget runs
// out. Only the first is irreversible. m.mu must be held.
func (m *Manager) lifecycleSignalDeliveredLocked() bool {
	return m.lifecycleClaimed && m.lifecycleAttempts < maxLifecycleAttempts
}

func (m *Manager) releaseLifecycleClaim() (attempts int, exhausted bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleAttempts++
	exhausted = m.lifecycleAttempts >= maxLifecycleAttempts
	m.lifecycleClaimed = exhausted
	return m.lifecycleAttempts, exhausted
}

// lifecycleIntentUnchangedLocked reports whether the pending intent is still
// the one an evaluation started with. m.mu must be held.
func (m *Manager) lifecycleIntentUnchangedLocked(stop, restart bool) bool {
	return m.stopPending == stop && m.restartPending == restart
}

// lifecycleGateOpenLocked reports whether the daemon is idle enough to be
// replaced or stopped. A zero lastRequestEnd means no request that the quiet
// period protects has been served yet, so there is no operation in progress to
// cut short. m.mu must be held.
func (m *Manager) lifecycleGateOpenLocked() bool {
	if m.inflightRequests > 0 {
		return false
	}
	if !m.lastLifecycleEnd.IsZero() && time.Since(m.lastLifecycleEnd) < lifecycleReplyGrace {
		return false
	}
	return m.lastRequestEnd.IsZero() || time.Since(m.lastRequestEnd) >= lifecycleQuietPeriod
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

func (m *Manager) terminateSelf() error {
	m.mu.RLock()
	terminate := m.terminate
	m.mu.RUnlock()
	if terminate == nil {
		terminate = signalSelfTerminate
	}
	return terminate()
}

func signalSelfTerminate() error {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM)
}

// underLaunchd reports whether launchd owns this process. A manually started
// wx daemon start --foreground must not kickstart itself: launchd would start a
// second daemon, that one would lose the socket lock and exit, and the manual
// process would survive still running the replaced binary.
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
//
// lifecycle marks the requests that exist only to change the daemon's own run
// state. They are counted like any other request, because a signal delivered
// while one is still dispatching would close its connection without a reply,
// but they are deliberately kept out of the quiet-period stamp: the quiet
// period protects a user-visible operation from being cut in half, and wx
// daemon stop is not such an operation. Stamping it would make the gate wait
// five seconds for the request that opened it.
func (m *Manager) beginRequest(lifecycle bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests++
	if lifecycle {
		m.inflightLifecycle++
	}
	m.mu.Unlock()
}

func (m *Manager) endRequest(lifecycle bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.inflightRequests--
	if lifecycle {
		m.inflightLifecycle--
		// The reply has not been written yet, so record the moment the gate
		// has to stay shut until.
		m.lastLifecycleEnd = time.Now()
		m.mu.Unlock()
		return
	}
	// The quiet period is measured from the moment the daemon actually went
	// idle of the work it does for users, so the stamp is taken only when that
	// count reaches zero: a nested or concurrent request that is still running
	// has not ended anything. A lifecycle request that is still in flight does
	// not hold the stamp back, because it is not the kind of work the quiet
	// period exists to protect.
	if m.inflightRequests-m.inflightLifecycle == 0 {
		m.lastRequestEnd = time.Now()
	}
	m.mu.Unlock()
}
