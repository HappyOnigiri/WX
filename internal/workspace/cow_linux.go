package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

func cowAvailable() bool { return false }

func cloneCOW(_, _ *os.File, _ string) error { return unix.ENOTSUP }

func swapCOW(_ *os.File, _, _ string) error { return unix.ENOTSUP }

func cowMetadata(_, _ *os.File, _ unix.Stat_t) (bool, error) { return false, unix.ENOTSUP }
