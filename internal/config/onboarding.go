package config

import (
	"fmt"
	"path/filepath"
)

func cloneRepositoryOnboarding(in map[string]RepositoryOnboarding) map[string]RepositoryOnboarding {
	if in == nil {
		return nil
	}
	out := make(map[string]RepositoryOnboarding, len(in))
	for relativePath, record := range in {
		out[relativePath] = record
	}
	return out
}

func normalizeOnboardingRelative(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("repository relative path is required")
	}
	if clean := filepath.Clean(value); clean == "." {
		return clean, nil
	}
	return NormalizeRepositoryRelative(value)
}

func normalizeRepositoryOnboarding(root string, records map[string]RepositoryOnboarding) (map[string]RepositoryOnboarding, error) {
	normalized := make(map[string]RepositoryOnboarding, len(records))
	for relativePath, record := range records {
		clean, err := normalizeOnboardingRelative(relativePath)
		if err != nil {
			return nil, fmt.Errorf("workspaces.%s.onboarding.%s must be a workspace-relative path", root, relativePath)
		}
		if _, exists := normalized[clean]; exists {
			return nil, fmt.Errorf("workspaces.%s.onboarding paths collide at %s", root, clean)
		}
		normalized[clean] = record
	}
	return normalized, nil
}
