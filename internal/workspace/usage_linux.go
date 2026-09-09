package workspace

import (
	"os"

	"golang.org/x/sys/unix"
)

// physicalOffset は Linux では判定に使わない。cowAvailable が false のため呼出元はここへ到達しない。
func physicalOffset(_ *os.File, _ int64) (int64, error) { return 0, unix.ENOTSUP }

// usageDev は linux では既に uint64 の Dev をそのまま返す。
func usageDev(stat *unix.Stat_t) uint64 { return stat.Dev }
