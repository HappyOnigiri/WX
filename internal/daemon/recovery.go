package daemon

import "strings"

// RecoveryUnavailableMarker は、当時のworktreeを復元できないことを示す機械可読なトークンである。
// RPCの失敗は文字列で往復するため、clientはこの印だけを見て「新しいworktreeで会話を再開してよいか」を確認する。
const RecoveryUnavailableMarker = "recovery=unavailable"

// restoreFailureCodes は復元経路でのみ付く slot の failure code である。
// prepare と共有する code（JOB_RETRY_EXHAUSTED、WORKTREE_OWNERSHIP_UNCERTAIN など）は、
// 新しい worktree でも同じ失敗を繰り返し得るため含めない。
var restoreFailureCodes = map[string]bool{
	"RESTORE_FAILED":       true,
	"RESTORE_AMBIGUOUS":    true,
	"SNAPSHOT_INCOMPLETE":  true,
	"SNAPSHOT_UNAVAILABLE": true,
}

// recoveryUnavailable は slot の failure code が復元不能を表すかを返す。
// RESTORE_FAILED には prepare command の failure ID が ":" で連結されることがある。
func recoveryUnavailable(failureCode string) bool {
	head, _, _ := strings.Cut(failureCode, ":")
	return restoreFailureCodes[head]
}

// IsRecoveryUnavailable は Resume・WaitReady の失敗が、当時のworktreeを復元できないことによるものかを返す。
func IsRecoveryUnavailable(err error) bool {
	return err != nil && strings.Contains(err.Error(), RecoveryUnavailableMarker)
}
