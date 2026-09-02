package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

func Serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := ensureWorktreeRoot(cfg.Storage.WorktreeRoot); err != nil {
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
	if err := os.MkdirAll(filepathDir(logPath), 0o700); err != nil {
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
		manager.git.SetDetailDir(filepathDir(logPath) + string(os.PathSeparator) + "details")
		manager.logLevel = &level
		defer manager.Close()
		rpcHandler = Handler{Manager: manager}
		durable = store
	}
	server := &rpc.Server{Socket: socket, Handler: rpcHandler, Durable: durable}
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
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
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

func filepathDir(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}
