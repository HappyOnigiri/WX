//go:build darwin

package scanner

import (
	"syscall"
	"testing"
)

func TestChangeTimeNanosPreservesSecondsAndNanoseconds(t *testing.T) {
	stat := &syscall.Stat_t{Ctimespec: syscall.Timespec{Sec: 12, Nsec: 345}}
	if got, want := changeTimeNanos(stat), int64(12_000_000_345); got != want {
		t.Fatalf("changeTimeNanos = %d, want %d", got, want)
	}
}
