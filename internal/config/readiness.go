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
	paths, err := validateEarlyPaths(r.EarlyPaths, "readiness.early_paths")
	if err != nil {
		return err
	}
	r.EarlyPaths = paths
	return nil
}

// validateEarlyPaths は early_paths の各要素が repository 相対の安全な path かを検査し、
// clean と重複除去を済ませた並びを返す。key はエラー文言に入れる設定キー路である。
// 戻り値は検証結果そのものなので、呼び出し側は設定へ書き戻して以後の実効値にする。
func validateEarlyPaths(values []string, key string) ([]string, error) {
	seen := map[string]bool{}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		clean := filepath.Clean(value)
		if value == "" || strings.ContainsRune(value, 0) || filepath.IsAbs(value) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("unsafe %s path %q", key, value)
		}
		for _, component := range strings.Split(clean, string(filepath.Separator)) {
			if strings.EqualFold(component, ".git") {
				return nil, fmt.Errorf("unsafe %s path %q", key, value)
			}
		}
		if !seen[clean] {
			paths = append(paths, clean)
			seen[clean] = true
		}
	}
	return paths, nil
}

// validatePrepareInputs は prepare.inputs の各要素を repository 相対の安全な
// path pattern として検査し、clean と重複除去を済ませた並びを返す。
func validatePrepareInputs(values []string, key string) ([]string, error) {
	paths, err := validateEarlyPaths(values, key)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		for _, segment := range strings.Split(path, string(filepath.Separator)) {
			if _, err := filepath.Match(segment, ""); err != nil {
				return nil, fmt.Errorf("invalid %s pattern %q: %w", key, path, err)
			}
		}
	}
	return paths, nil
}
