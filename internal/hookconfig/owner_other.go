//go:build !darwin && !linux

package hookconfig

import "os"

// ownedByCurrentUser は uid を読めない platform では判定を諦め、他の fail closed 条件に委ねる。
func ownedByCurrentUser(os.FileInfo) bool { return true }
