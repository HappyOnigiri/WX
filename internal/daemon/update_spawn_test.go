package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSpawnDetachedLeavesTheParentSession は自動適用の子が親と別 session で動くことを固定する。
// 同じ session に残ると、install.sh が `wx daemon restart` で親 daemon を止めた瞬間に子も落ちる。
// 出力が log へ向くこと、終了した子が zombie で残らないことも併せて見る。
// 子 process と reaper goroutine は process 全体へ影響するため、並行実行はしない。
// commentlint:allow-long -- session 分離の理由と、同じテストが併せて見る範囲を続けて示す
func TestSpawnDetachedLeavesTheParentSession(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, updateApplyLogName)
	// 子は自分の PID と process group ID だけを書いて終わる。ps の書式は先頭の空白を含むため、読む側で削る。
	script := `printf '%s %s\n' "$$" "$(ps -o pgid= -p $$)"`
	if err := spawnDetached([]string{"/bin/sh", "-c", script}, os.Environ(), logPath); err != nil {
		t.Fatal(err)
	}
	pid, pgid := waitForSpawnedIDs(t, logPath)
	if own, err := syscall.Getpgid(os.Getpid()); err != nil {
		t.Fatal(err)
	} else if pgid == own {
		t.Fatalf("the child stayed in process group %d with its parent", pgid)
	}
	waitForReaped(t, pid)
}

// waitForSpawnedIDs は子が書いた PID と process group ID を読み取る。
func waitForSpawnedIDs(t *testing.T, logPath string) (pid, pgid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fields := strings.Fields(readFileOrEmpty(t, logPath))
		if len(fields) == 2 {
			first, firstErr := strconv.Atoi(fields[0])
			second, secondErr := strconv.Atoi(fields[1])
			if firstErr == nil && secondErr == nil {
				return first, second
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child wrote %q to %s", readFileOrEmpty(t, logPath), logPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readFileOrEmpty(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // path は test が作った一時ディレクトリの下にある
	if err != nil {
		return ""
	}
	return string(data)
}

// waitForReaped は子の process table 上の entry が消えるまで待つ。
// Wait を呼ばずに手放すと zombie として残り続け、この待機が期限切れになる。
func waitForReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(out))
		if state == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still listed as %q", pid, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
