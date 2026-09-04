package workspace

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
)

// maxRepositoryDirNameLength bounds the directory name so a deep slot path
// stays comfortably inside the platform's per-component limit even for a
// repository whose remote name is pathological.
const maxRepositoryDirNameLength = 64

// reservedDirNamePrefix is the prefix wx keeps for its own top-level entries
// (_unbound, _recovery). Repository directories live one level deeper, but
// the same prefix is refused for them so the reservation reads as one rule
// and the orphan scan never has to special-case a repository name.
const reservedDirNamePrefix = "_"

// RepositoryDirName resolves the directory name a repository takes inside a
// slot. Resolution order is: the repository's pinned dir_name, the
// repository's dir_source, storage.repo_dir_source, and finally the main
// worktree's own directory name.
//
// Every step falls back rather than failing: the name only affects where wx
// puts a checkout, never whether it can prove ownership of one. The value
// actually used is recorded in slot_repositories.dir_name and is the
// authority from then on, so a later configuration or remote-URL change
// moves new slots without disturbing existing ones.
func RepositoryDirName(repo discovery.Repository, cfg config.Config) string {
	override := cfg.Repositories[string(repo.MainPath)]
	if name, ok := sanitizeDirName(override.DirName); ok {
		return name
	}
	source := override.DirSource
	if source == "" {
		source = cfg.Storage.RepoDirSource
	}
	if source != config.RepoDirSourceDirectory {
		if name, ok := sanitizeDirName(repo.RemoteName); ok {
			return name
		}
	}
	if name, ok := sanitizeDirName(filepath.Base(string(repo.MainPath))); ok {
		return name
	}
	// filepath.Base never returns an empty string and the repository ID is
	// hex, so this is only reached for a main path whose last component is
	// "/" or ".."; the repository ID is always a usable component.
	return string(repo.ID)
}

// sanitizeDirName rejects anything that cannot safely be one path component:
// an empty value, a path separator, a control character, "." or "..", the
// reserved "_" prefix, and the ownership marker prefix. Accepted names are
// truncated to maxRepositoryDirNameLength.
//
// The marker prefix has to be refused because the marker and the repository
// directory share one namespace: the marker is written into the slot
// directory, immediately beside the repository directories. A repository
// named ".wx-owner-<repository-id>" would want the same path as a marker, so
// preparation would fail outright, and in a bundle it could collide with
// another repository's marker instead of its own.
func sanitizeDirName(value string) (string, bool) {
	if value == "" || value == "." || value == ".." {
		return "", false
	}
	if strings.ContainsAny(value, `/\`) || strings.HasPrefix(value, reservedDirNamePrefix) {
		return "", false
	}
	if strings.HasPrefix(value, ownershipMarkerPrefix) {
		return "", false
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return "", false
		}
	}
	value = truncateDirName(value)
	if value != filepath.Clean(value) {
		return "", false
	}
	return value, true
}

// truncateDirName cuts value to the byte budget on a rune boundary. Cutting
// mid-rune would leave an invalid UTF-8 byte sequence in
// slot_repositories.dir_name and in the directory name on disk, which the
// checks after truncation do not inspect.
func truncateDirName(value string) string {
	if len(value) <= maxRepositoryDirNameLength {
		return value
	}
	cut := 0
	for index := range value {
		if index > maxRepositoryDirNameLength {
			break
		}
		cut = index
	}
	return value[:cut]
}

// UniqueDirName returns name, or name with a numeric suffix, so that no two
// repositories in one slot claim the same directory. Comparison is
// case-insensitive because APFS is case-insensitive by default: two names
// that differ only in case would be one directory on disk while remaining
// two distinct rows under UNIQUE(slot_id, dir_name).
func UniqueDirName(name string, taken map[string]bool) string {
	key := strings.ToLower(name)
	if !taken[key] {
		taken[key] = true
		return name
	}
	for suffix := 2; ; suffix++ {
		candidate := name + "-" + strconv.Itoa(suffix)
		key = strings.ToLower(candidate)
		if !taken[key] {
			taken[key] = true
			return candidate
		}
	}
}
