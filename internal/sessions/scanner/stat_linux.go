package scanner

import "syscall"

// changeTimeNanos は inode の変更時刻を nanosecond で返す。
// linux は退行検出用の target であり、Stat_t の field 名だけが darwin と異なる。
func changeTimeNanos(stat *syscall.Stat_t) int64 {
	return stat.Ctim.Sec*1e9 + stat.Ctim.Nsec
}
