package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// handlerの期限上限へ加える余裕時間である。
// client側のdialとframing時間を吸収し、clientのbudget内の要求をserverだけで失効させない。
const readinessCeilingMargin = 5 * time.Minute

func Serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	_, root, err := ensureWorktreeRootDescriptor(cfg.Storage.WorktreeRoot)
	if err != nil {
		return fmt.Errorf("prepare worktree root: %w", err)
	}
	if err := root.Close(); err != nil {
		return fmt.Errorf("prepare worktree root: %w", err)
	}
	dbPath, err := config.StatePath()
	if err != nil {
		return err
	}
	logPath, err := config.LogPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var level slog.LevelVar
	level.Set(slogLevel(cfg.Logging.Level))
	logger := slog.New(slog.NewJSONHandler(file, &slog.HandlerOptions{Level: &level}))
	socket, err := config.SocketPath()
	if err != nil {
		return err
	}
	lock, err := acquireDaemonLock(socket + ".lock")
	if err != nil {
		return err
	}
	defer releaseDaemonLock(lock)
	store, openErr := state.Open(dbPath)
	var rpcHandler rpc.Handler
	var durable rpc.DurableIdempotency
	if openErr != nil {
		rpcHandler = DegradedHandler{DatabasePath: dbPath, OpenError: openErr}
		logger.Error("daemon entered read-only degraded mode", "database", dbPath, "error", openErr)
	} else {
		defer func() { _ = store.Close() }()
		manager := New(cfg, store, logger, true)
		manager.git.SetDetailDir(filepath.Dir(logPath) + string(os.PathSeparator) + "details")
		manager.logLevel = &level
		defer manager.Close()
		rpcHandler = Handler{Manager: manager}
		durable = store
	}
	server := &rpc.Server{Socket: socket, Handler: rpcHandler, Durable: durable, MaxHandlerTimeout: handlerCeiling(cfg.Readiness.Timeout.Duration)}
	logger.Info("daemon started", "socket", socket, "protocol_version", rpc.ProtocolVersion, "degraded", openErr != nil)
	if err := server.Serve(ctx); err != nil {
		return fmt.Errorf("serve daemon: %w", err)
	}
	return nil
}

func slogLevel(value string) slog.Level {
	switch value {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func acquireDaemonLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("wx daemon is already running")
		}
		return nil, fmt.Errorf("lock daemon runtime: %w", err)
	}
	return file, nil
}

func releaseDaemonLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
}

// RPC handler の期限上限を readiness budget 以上にする。
// readiness と無関係な handler もあるため、rpc の既定値を下回らない。
func handlerCeiling(readiness time.Duration) time.Duration {
	if ceiling := readiness + readinessCeilingMargin; ceiling > rpc.DefaultMaxHandlerTimeout {
		return ceiling
	}
	return rpc.DefaultMaxHandlerTimeout
}
