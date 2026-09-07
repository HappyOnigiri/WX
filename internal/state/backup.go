package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// backupStepPages は 1回の Backup.Step が複製する page 数である。
// writer を止めないため全 page を一度に押し切らず、step 間で context と lock 競合を確認できる粒度にする。
const backupStepPages = 256

// backupBusyWait は SQLITE_BUSY / SQLITE_LOCKED で進めなかった step を再試行するまでの待機である。
const backupBusyWait = 20 * time.Millisecond

// sqliteBusy と sqliteLocked は step が lock 競合で進めなかったことを表す primary result code である。
// backup は writer を止めないため、この二つだけは失敗にせず待って再試行する。
const (
	sqliteBusy   = 5
	sqliteLocked = 6
)

// backupTempPrefix は完成前の backup に付ける一時ファイルの接頭辞である。
// 拡張子が `.db` でないため世代集合には入らず、途中の成果物が完成品として公開されることはない。
const backupTempPrefix = "incomplete-"

// backupTempSuffix は一時ファイルの拡張子である。
const backupTempSuffix = ".tmp"

// Backup は state database の online backup を backups directory へ世代として書き出し、その path を返す。
// Store.writer を保持しないため、複製中も lease・heartbeat・release は通常どおり進む。
// ctx の期限内に複製が終わらなければ失敗として返し、既存の成功世代には触れない。
// commentlint:allow-long -- writer を止めない契約と期限切れ時の扱いを呼び出し側へ示すため
func (s *Store) Backup(ctx context.Context, generations int, retention time.Duration) (string, error) {
	ctx, cancel := s.backupContext(ctx)
	defer cancel()
	release, err := s.acquireBackupGate(ctx)
	if err != nil {
		return "", err
	}
	defer release()

	dir := s.path + ".backups"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	root, _, err := domain.OpenOwnedRoot(dir, dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()

	temporary, identity, err := createBackupTemp(root)
	if err != nil {
		return "", err
	}
	completed := false
	defer func() {
		if !completed {
			removeOwnedBackupTemp(root, temporary, identity)
		}
	}()

	if err := s.copyInto(ctx, filepath.Join(dir, temporary)); err != nil {
		return "", err
	}
	destination, err := publishBackup(root, temporary)
	if err != nil {
		return "", err
	}
	completed = true
	if err := pruneBackups(root, generations, retention); err != nil {
		return "", err
	}
	return filepath.Join(dir, destination), nil
}

// backupContext は Close の開始で取り消される context を返す。
// 呼び出し側の期限と Close のどちらでも step loop が抜け、source 接続が pool へ戻る。
func (s *Store) backupContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-stop:
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}

// acquireBackupGate は backup と世代整理を直列化する。通常の writer とは排他しない。
func (s *Store) acquireBackupGate(ctx context.Context) (func(), error) {
	select {
	case s.backupGate <- struct{}{}:
		return func() { <-s.backupGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// copyInto は一つの source 接続を確保し、NewBackup から Finish までを Raw callback 内で完結させる。
// driver handle を callback の外へ持ち出さず、この接続を途中で別の DB 操作へ再利用しない。
func (s *Store) copyInto(ctx context.Context, destination string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		backup, err := backuper.NewBackup(destination)
		if err != nil {
			return err
		}
		stepErr := s.stepBackup(ctx, backup)
		finishErr := backup.Finish()
		if stepErr != nil {
			return stepErr
		}
		return finishErr
	})
}

// stepBackup は有限 page ずつ複製し、各 step の前後で context を確認する。
// Step の false が SQLITE_DONE による完了であり、true は残 page があることを表す。
func (s *Store) stepBackup(ctx context.Context, backup *sqlite.Backup) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.backupStepBarrier != nil {
			s.backupStepBarrier()
		}
		more, err := backup.Step(backupStepPages)
		switch {
		case err == nil && !more:
			return nil
		case err == nil:
		case isBackupBusy(err):
			// 並行 writer と競合しただけなので、通常の writer を止めずに短く待って再試行する。
			if waitErr := waitBackupRetry(ctx); waitErr != nil {
				return waitErr
			}
		default:
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// isBackupBusy は step が lock 競合で進めなかったかを返す。extended result code の下位 8bit が primary code である。
func isBackupBusy(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	primary := sqliteErr.Code() & 0xff
	return primary == sqliteBusy || primary == sqliteLocked
}

// waitBackupRetry は再試行まで待ち、待機中に context が終われば その error を返す。
func waitBackupRetry(ctx context.Context) error {
	timer := time.NewTimer(backupBusyWait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// createBackupTemp は backups directory 内に 0600 の一時ファイルを排他生成し、その名前と作成時の identity を返す。
// identity は失敗時の後片付けで、自分が作った実体だけを消すための照合に使う。
func createBackupTemp(root *os.Root) (string, os.FileInfo, error) {
	for range idCollisionAttempts {
		suffix, err := domain.NewShortID()
		if err != nil {
			return "", nil, err
		}
		name := backupTempPrefix + suffix + backupTempSuffix
		file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil {
			_ = root.Remove(name)
			return "", nil, statErr
		}
		if closeErr != nil {
			_ = root.Remove(name)
			return "", nil, closeErr
		}
		return name, info, nil
	}
	return "", nil, fmt.Errorf("could not create an unused backup temporary file in %d attempts", idCollisionAttempts)
}

// removeOwnedBackupTemp は作成時に控えた identity と一致する一時ファイルだけを消す。
// 名前だけで一致する別の実体は残し、所有していない中間成果物を削除しない。
func removeOwnedBackupTemp(root *os.Root, name string, identity os.FileInfo) {
	current, err := domain.PhysicalPathInfo(root, name)
	if err != nil || !os.SameFile(identity, current) {
		return
	}
	_ = root.Remove(name)
}

// publishBackup は完成した一時ファイルを sync し、世代の timestamp 名へ rename して公開する。
// rename までは `.db` の世代集合に入らないため、途中で失敗しても不完全な DB は完成品として見えない。
func publishBackup(root *os.Root, temporary string) (string, error) {
	if err := syncBackupFile(root, temporary); err != nil {
		return "", err
	}
	destination := time.Now().UTC().Format("20060102T150405.000000000Z") + ".db"
	if err := root.Rename(temporary, destination); err != nil {
		return "", err
	}
	if err := syncBackupDirectory(root); err != nil {
		return "", err
	}
	return destination, nil
}

// syncBackupFile は完成前の backup を rename の前に永続化する。
func syncBackupFile(root *os.Root, name string) error {
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// syncBackupDirectory は rename による世代の公開を永続化する。
func syncBackupDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// pruneBackups は完成した `.db` の世代だけを新しい順に数え、世代数と保持期間を超えたものを削除する。
// 完成前の一時ファイルと backups directory 内の他の entry は数えず、削除もしない。
func pruneBackups(root *os.Root, generations int, retention time.Duration) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	closeErr := dir.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
	cutoff := time.Now().Add(-retention)
	backupIndex := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if backupIndex >= generations || (retention > 0 && info.ModTime().Before(cutoff)) {
			if err := root.Remove(entry.Name()); err != nil {
				return err
			}
		}
		backupIndex++
	}
	return nil
}
