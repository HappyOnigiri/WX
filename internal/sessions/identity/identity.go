// Package identity は会話をツールと native ID の組み合わせで識別する。
package identity

import (
	"crypto/sha256"
	"encoding/hex"
)

func ComputeSessionStableID(tool, nativeID string) string {
	if nativeID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("v1\nsession\n" + tool + "\n" + nativeID))
	return "s1-" + hex.EncodeToString(sum[:8])
}
