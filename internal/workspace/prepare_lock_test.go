package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// waitForFile は barrier file の出現を待つ。待ちきれない場合はテストを失敗させる。
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("barrier %s did not appear within %s", path, timeout)
}

// TestPrepareCommandDoesNotBlockAnotherSlotOfTheSameRepository は、prepare command 実行中に
// 同じ repository の別 slot の準備が始められることを確認する。
// prepare 全体が common-directory lock を保持していると、この二つ目の準備は一つ目の完了まで進めない。
func TestPrepareCommandDoesNotBlockAnotherSlotOfTheSameRepository(t *testing.T) {
	_, repo, blocking, head, blockingTarget := prepareEdgesFixture(t)
	root := blocking.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	barriers := t.TempDir()
	started := filepath.Join(barriers, "started")
	proceed := filepath.Join(barriers, "proceed")
	cfg := blocking.Config
	script := "touch " + started + "; until [ -f " + proceed + " ]; do sleep 0.01; done"
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 60 * time.Second}}}}
	blocking.Config = cfg
	blocking.SlotLocks = &gitx.KeyedLocks{}

	free := *blocking
	free.SlotRelPath = filepath.Join(testWorkspaceID, "slot0002")
	free.SlotPath = filepath.Join(root, free.SlotRelPath)
	freeConfig := cfg
	freeConfig.Repositories = nil
	free.Config = freeConfig
	freeTarget := filepath.Join(free.SlotPath, testRepositoryID)

	blocked := make(chan error, 1)
	go func() {
		blocked <- blocking.Prepare(context.Background(), repo, blockingTarget, head, "slot")
	}()
	waitForFile(t, started, 60*time.Second)
	if err := free.Prepare(context.Background(), repo, freeTarget, head, "slot0002"); err != nil {
		t.Fatalf("prepare of an independent slot during a prepare command: %v", err)
	}
	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; err != nil {
		t.Fatalf("prepare with a blocking prepare command: %v", err)
	}
}

// TestPrepareExcludesConcurrentPreparationsOfTheSameSlot は、共通ロックを手放す区間でも
// 同じ slot の準備が重ならないことを確認する。
func TestPrepareExcludesConcurrentPreparationsOfTheSameSlot(t *testing.T) {
	_, repo, first, head, target := prepareEdgesFixture(t)
	root := first.Config.Storage.WorktreeRoot
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	barriers := t.TempDir()
	started := filepath.Join(barriers, "started")
	proceed := filepath.Join(barriers, "proceed")
	cfg := first.Config
	script := "touch " + started + "; until [ -f " + proceed + " ]; do sleep 0.01; done"
	cfg.Repositories = map[string]config.Repository{string(repo.MainPath): {Prepare: config.Prepare{Command: []string{"/bin/sh", "-c", script}, Timeout: config.Duration{Duration: 60 * time.Second}}}}
	first.Config = cfg
	first.SlotLocks = &gitx.KeyedLocks{}

	second := *first
	secondConfig := cfg
	secondConfig.Repositories = nil
	second.Config = secondConfig

	done := make(chan error, 1)
	go func() {
		done <- first.Prepare(context.Background(), repo, target, head, "slot")
	}()
	waitForFile(t, started, 60*time.Second)
	blockedCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := second.Prepare(blockedCtx, repo, target, head, "slot"); err == nil {
		t.Fatal("a concurrent preparation of the same slot proceeded")
	}
	if err := os.WriteFile(proceed, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("prepare with a blocking prepare command: %v", err)
	}
}
