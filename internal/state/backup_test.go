package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/domain"
)

// backupBulkPages は online backup が複数 step に分かれるだけの page 数を稼ぐための blob 総量である。
// 既定 page_size は 4096 byte なので、4MiB は backupStepPages(256) を大きく超える。
const backupBulkBytes = 4 << 20

// growDatabase は step が1回で終わらない大きさまで database を膨らませる。
func growDatabase(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS backup_bulk(id INTEGER PRIMARY KEY, payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 64<<10)
	for i := 0; i < backupBulkBytes/len(payload); i++ {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO backup_bulk(payload) VALUES(?)`, payload); err != nil {
			t.Fatal(err)
		}
	}
}

// seedLeasableSlot は READY slot と貸出可能な session を用意し、貸出に使う session を返す。
func seedLeasableSlot(t *testing.T, store *Store, slotID string) Session {
	t.Helper()
	ctx := context.Background()
	worktree := filepath.Join(t.TempDir(), slotID, "repo")
	job, err := store.CreateStandby(ctx,
		Slot{ID: slotID, WorkspaceID: "workspace", Generation: 1, RootID: testRootID, RelPath: filepath.Join("workspace", slotID), State: "PREPARING"},
		[]SlotRepository{{RepositoryID: "repository", WorktreePath: worktree, State: "PREPARING", RequestedRef: "main", BaseOID: "abc", Fingerprint: "fp"}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJob(ctx, job.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlotRepositoryState(ctx, slotID, "repository", []string{"PREPARING"}, "READY"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkReady(ctx, slotID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishJob(ctx, claimed.ID, "owner", nil); err != nil {
		t.Fatal(err)
	}
	return Session{ID: slotID, WorkspaceID: "workspace", SlotID: slotID, State: "ACTIVE", AgentKind: "codex", ClientPID: os.Getpid(), TokenHash: HashToken("token")}
}

// openBackupRootForTest は Backup と同じ方法で backups directory を pin する。
func openBackupRootForTest(t *testing.T, dir string) (*os.Root, string, error) {
	t.Helper()
	root, relative, err := domain.OpenOwnedRoot(dir, dir)
	if err == nil {
		t.Cleanup(func() { _ = root.Close() })
	}
	return root, relative, err
}

func backupDirEntries(t *testing.T, store *Store) (databases, temporaries []string) {
	t.Helper()
	entries, err := os.ReadDir(store.path + ".backups")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch {
		case filepath.Ext(entry.Name()) == ".db":
			databases = append(databases, entry.Name())
		case strings.HasPrefix(entry.Name(), backupTempPrefix):
			temporaries = append(temporaries, entry.Name())
		}
	}
	return databases, temporaries
}

// TestOnlineBackupLetsLeaseAndHeartbeatProceedBetweenSteps は backup が Store.writer を占有しないことを検査する。
// 貸出 transaction と heartbeat 更新を別 goroutine で走らせ、backup 完了前に成功することを確認する。
func TestOnlineBackupLetsLeaseAndHeartbeatProceedBetweenSteps(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	session := seedLeasableSlot(t, store, "standby")
	growDatabase(t, store)
	ctx := context.Background()

	var steps atomic.Int64
	var once sync.Once
	writesDone := make(chan error, 1)
	store.backupStepBarrier = func() {
		steps.Add(1)
		once.Do(func() {
			go func() {
				if err := store.LeaseReady(ctx, "standby", session); err != nil {
					writesDone <- err
					return
				}
				writesDone <- store.Heartbeat(ctx, "standby", "token")
			}()
			select {
			case err := <-writesDone:
				if err != nil {
					t.Errorf("concurrent lease and heartbeat failed during backup: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("lease and heartbeat blocked behind the online backup writer")
			}
		})
	}
	path, err := store.Backup(ctx, 2, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if steps.Load() < 2 {
		t.Fatalf("backup finished in %d step(s); the finite-step loop was not exercised", steps.Load())
	}
	if databases, temporaries := backupDirEntries(t, store); len(databases) != 1 || len(temporaries) != 0 {
		t.Fatalf("backup directory databases=%v temporaries=%v after %s", databases, temporaries, path)
	}
}

// TestOnlineBackupWithConcurrentWritesStaysConsistent は並行書き込み下で完成した世代を別 Store で開き、
// integrity_check と foreign key 整合性、registry の commit 済み内容を確認する。
func TestOnlineBackupWithConcurrentWritesStaysConsistent(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	session := seedLeasableSlot(t, store, "standby")
	growDatabase(t, store)
	ctx := context.Background()
	if err := store.LeaseReady(ctx, "standby", session); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	writes := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				writes <- nil
				return
			default:
			}
			if err := store.Heartbeat(ctx, "standby", "token"); err != nil {
				writes <- err
				return
			}
		}
	}()
	path, backupErr := store.Backup(ctx, 2, time.Hour)
	close(stop)
	if err := <-writes; err != nil {
		t.Fatalf("concurrent heartbeat failed: %v", err)
	}
	if backupErr != nil {
		t.Fatal(backupErr)
	}

	copied, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = copied.Close() }()
	var integrity string
	if err := copied.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check=%q", integrity)
	}
	rows, err := copied.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign_key_check reported a violation in the completed backup")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	workspace, err := copied.Workspace(ctx, "workspace")
	if err != nil || len(workspace.Repositories) != 1 {
		t.Fatalf("backup workspace=%+v err=%v", workspace, err)
	}
	slots, err := copied.ListSlots(ctx, true)
	if err != nil || len(slots) != 1 || slots[0].SessionID != "standby" {
		t.Fatalf("backup slots=%+v err=%v", slots, err)
	}
}

// TestOnlineBackupDeadlineLeavesExistingGenerationsIntact は期限切れの backup が
// 不完全な DB を完成品として残さず、既存の成功世代と lastBackup 相当の成果を壊さないことを検査する。
func TestOnlineBackupDeadlineLeavesExistingGenerationsIntact(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	growDatabase(t, store)
	ctx := context.Background()
	first, err := store.Backup(ctx, 3, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	deadlined, cancel := context.WithCancel(ctx)
	defer cancel()
	store.backupStepBarrier = func() { cancel() }
	if _, err := store.Backup(deadlined, 3, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired backup err=%v want context.Canceled", err)
	}
	databases, temporaries := backupDirEntries(t, store)
	if len(temporaries) != 0 {
		t.Fatalf("incomplete backup temporaries survived: %v", temporaries)
	}
	if len(databases) != 1 || filepath.Join(store.path+".backups", databases[0]) != first {
		t.Fatalf("generations=%v want only %s", databases, first)
	}
}

// TestOnlineBackupsSerializeWithoutBlockingWriters は同時 backup が gate で直列化されることを検査する。
func TestOnlineBackupsSerializeWithoutBlockingWriters(t *testing.T) {
	store := openTestStore(t)
	seedWorkspace(t, store)
	growDatabase(t, store)
	ctx := context.Background()

	// gate が効いていれば step 中の goroutine は常に1つである。窓を開けて重なりを観測する。
	var stepping, peak atomic.Int64
	store.backupStepBarrier = func() {
		current := stepping.Add(1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		stepping.Add(-1)
	}
	paths := make(chan string, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			path, err := store.Backup(ctx, 5, time.Hour)
			if err != nil {
				failures <- err
				return
			}
			paths <- path
		}()
	}
	group.Wait()
	close(paths)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Fatalf("concurrent backups reused %s", path)
		}
		seen[path] = true
	}
	if len(seen) != 2 {
		t.Fatalf("completed backups=%v", seen)
	}
	if peak.Load() != 1 {
		t.Fatalf("observed %d concurrent stepping backup(s); want exactly 1", peak.Load())
	}
}

// TestCloseWaitsForInflightBackup は Close が実行中の backup を取り消し、source 接続の返却を待つことを検査する。
func TestCloseWaitsForInflightBackup(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	seedRoot(t, store, testRootID, testRootPath, "root-identity", true)
	seedWorkspace(t, store)
	growDatabase(t, store)

	started := make(chan struct{})
	var once sync.Once
	store.backupStepBarrier = func() { once.Do(func() { close(started) }) }
	backupDone := make(chan error, 1)
	go func() {
		_, err := store.Backup(context.Background(), 2, time.Hour)
		backupDone <- err
	}()
	<-started
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-backupDone:
		if err == nil {
			t.Fatal("backup completed after Close canceled it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close returned while the backup still held the source connection")
	}
	if _, err := store.Backup(context.Background(), 2, time.Hour); err == nil {
		t.Fatal("backup started on a closed store")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestBackupPublishFailureKeepsIncompleteCopyOutOfGenerations は rename の失敗が
// 完成前の成果物を `.db` の世代集合へ入れないことを検査する。
func TestBackupPublishFailureKeepsIncompleteCopyOutOfGenerations(t *testing.T) {
	dir := t.TempDir()
	root, _, err := openBackupRootForTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	temporary, identity, err := createBackupTemp(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := publishBackup(root, temporary); err == nil {
		t.Fatal("publish succeeded through a read-only backup directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	databases, err := filepath.Glob(filepath.Join(dir, "*.db"))
	if err != nil || len(databases) != 0 {
		t.Fatalf("generations=%v err=%v", databases, err)
	}
	removeOwnedBackupTemp(root, temporary, identity)
	if _, err := os.Lstat(filepath.Join(dir, temporary)); !os.IsNotExist(err) {
		t.Fatalf("owned temporary survived cleanup: %v", err)
	}
}

// TestRemoveOwnedBackupTempKeepsForeignEntries は名前が一致するだけの別実体を消さないことを検査する。
func TestRemoveOwnedBackupTempKeepsForeignEntries(t *testing.T) {
	dir := t.TempDir()
	root, _, err := openBackupRootForTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	temporary, identity, err := createBackupTemp(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, temporary), []byte("someone else"), 0o600); err != nil {
		t.Fatal(err)
	}
	removeOwnedBackupTemp(root, temporary, identity)
	content, err := os.ReadFile(filepath.Join(dir, temporary))
	if err != nil || string(content) != "someone else" {
		t.Fatalf("foreign temporary content=%q err=%v", content, err)
	}
}

// TestCopyIntoRejectsUnusableDestination は destination を開けない場合に driver の error を伝播することを検査する。
func TestCopyIntoRejectsUnusableDestination(t *testing.T) {
	store := openTestStore(t)
	destination := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.copyInto(context.Background(), destination); err == nil {
		t.Fatal("online backup opened a directory as its destination")
	}
}

// TestBackupBusyClassificationAndRetryWait は待って再試行する error の判定と待機の取消を検査する。
func TestBackupBusyClassificationAndRetryWait(t *testing.T) {
	if isBackupBusy(errors.New("not a sqlite error")) {
		t.Fatal("a non-SQLite error was classified as busy")
	}
	if isBackupBusy(nil) {
		t.Fatal("nil was classified as busy")
	}
	if err := waitBackupRetry(context.Background()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitBackupRetry(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("retry wait err=%v want context.Canceled", err)
	}
}

// TestPruneBackupsRejectsUnreadableDirectory は世代整理が directory を読めない場合に error を返すことを検査する。
func TestPruneBackupsRejectsUnreadableDirectory(t *testing.T) {
	dir := t.TempDir()
	root, _, err := openBackupRootForTest(t, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pruneBackups(root, 1, time.Hour); err == nil {
		t.Fatal("generation pruning succeeded on a closed root")
	}
	if err := syncBackupDirectory(root); err == nil {
		t.Fatal("directory sync succeeded on a closed root")
	}
	if err := syncBackupFile(root, "missing.tmp"); err == nil {
		t.Fatal("file sync succeeded on a closed root")
	}
	if _, _, err := createBackupTemp(root); err == nil {
		t.Fatal("temporary creation succeeded on a closed root")
	}
}
