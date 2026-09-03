package state

import (
	"path/filepath"
	"sync"
	"testing"
)

// TestLoadOrCreateRPCKeyConvergesUnderConcurrentCreation exercises the
// publish-time race in loadOrCreateRPCKey: many goroutines race to create the
// same idempotency key file. Before the fix, the winner became visible via
// O_CREATE|O_EXCL at the final path before its Write/Sync/Close completed, so
// a loser's retry could Lstat-succeed and read a torn (short or empty) file
// and fail with "invalid length". The fix publishes through a private
// temporary file and only links it into place after the content is fully
// synced, so every caller must observe either no file or a complete one. The
// outcome (a single 32-byte key shared by every caller, no error) is
// deterministic regardless of goroutine scheduling, so this is not a flaky
// test — run with -race and a high goroutine count to make the original
// window easy to hit.
func TestLoadOrCreateRPCKeyConvergesUnderConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc-key")
	const attempts = 64
	keys := make([][]byte, attempts)
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	wg.Add(attempts)
	for i := range attempts {
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = loadOrCreateRPCKey(path)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if len(keys[i]) != 32 {
			t.Fatalf("attempt %d: key length=%d", i, len(keys[i]))
		}
		if string(keys[i]) != string(keys[0]) {
			t.Fatal("concurrent key creation converged on different keys")
		}
	}
}
