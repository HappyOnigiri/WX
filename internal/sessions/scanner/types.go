package scanner

import "github.com/HappyOnigiri/WX/internal/sessions/identity"

// Session はセッション再開に必要なメタ情報だけを保持する。
type Session struct {
	Tool, SessionID, Title, CWD, StableID string
	Mtime                                 float64
	RawPath                               string
}

// StableID は native session ID から安定 ID を計算する。
func StableID(tool, sessionID string) string {
	return identity.ComputeSessionStableID(tool, sessionID)
}
