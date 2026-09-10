package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

func cowAvailable() bool { return false }

func cloneCOW(_, _ *os.File, _ string) error { return unix.ENOTSUP }

// cowSourceFlags は clone を持たない platform では常に 0 を返す。
func cowSourceFlags(_ *unix.Stat_t) uint32 { return 0 }

func swapCOW(_ *os.File, _, _ string) error { return unix.ENOTSUP }

// cowACLBufferSize は darwin 実装と同じ定数を共有コードへ見せるためだけに置く。
const cowACLBufferSize = 64 << 10

func cowMetadata(_, _ *os.File, _ unix.Stat_t, _ *cowScratch) (bool, error) {
	return false, unix.ENOTSUP
}

func cowXattrs(_ *os.File) (map[string][]byte, error) { return nil, nil }
