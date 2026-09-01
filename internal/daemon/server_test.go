package daemon

import (
	"strings"
	"testing"
)

func TestDaemonRuntimeLockAllowsOnlyOneOwner(t *testing.T) {
	path := t.TempDir() + "/daemon.lock"
	first, err := acquireDaemonLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseDaemonLock(first)
	second, err := acquireDaemonLock(path)
	if err == nil {
		releaseDaemonLock(second)
		t.Fatal("second daemon acquired the runtime lock")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
}
