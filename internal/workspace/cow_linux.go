package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

func cowAvailable() bool { return false }

func cloneCOW(_, _ *os.File, _ string) error { return unix.ENOTSUP }

func swapCOW(_ *os.File, _, _ string) error { return unix.ENOTSUP }

// cowACLBufferSize は darwin 実装と同じ定数を共有コードへ見せるためだけに置く。
const cowACLBufferSize = 64 << 10

func cowMetadata(_, _ *os.File, _ unix.Stat_t, _ *cowScratch) (bool, error) {
	return false, unix.ENOTSUP
}
