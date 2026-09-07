package scanner

import "syscall"

// changeTimeNanos は inode の変更時刻を nanosecond で返す。
// macOS では mtime を復元した同一 inode への上書きを検出するため、cache の検証値に含める。
func changeTimeNanos(stat *syscall.Stat_t) int64 {
	return stat.Ctimespec.Sec*1e9 + stat.Ctimespec.Nsec
}
