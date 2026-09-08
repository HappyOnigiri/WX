package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

func validateReadiness(r *Readiness) error {
	if r.Mode != "early" && r.Mode != "full" {
		return fmt.Errorf("readiness.mode must be early or full")
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(r.EarlyPaths))
	for _, value := range r.EarlyPaths {
		clean := filepath.Clean(value)
		if value == "" || strings.ContainsRune(value, 0) || filepath.IsAbs(value) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe readiness.early_paths path %q", value)
		}
		for _, component := range strings.Split(clean, string(filepath.Separator)) {
			if strings.EqualFold(component, ".git") {
				return fmt.Errorf("unsafe readiness.early_paths path %q", value)
			}
		}
		if !seen[clean] {
			paths = append(paths, clean)
			seen[clean] = true
		}
	}
	r.EarlyPaths = paths
	return nil
}
