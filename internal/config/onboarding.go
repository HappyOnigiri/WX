package config

import (
	"fmt"
	"path/filepath"
)

// normalizeOnboardingRelative は repository onboarding 記録の workspace 相対 key を
// 検証する。workspace root の repository は "." で表す。
func normalizeOnboardingRelative(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repository relative path is required")
	}
	if clean := filepath.Clean(value); clean == "." {
		return clean, nil
	}
	return NormalizeRepositoryRelative(value)
}
