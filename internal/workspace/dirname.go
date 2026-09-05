package workspace

import (
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
)

// maxRepositoryDirNameLength は directory 名を制限し、remote 名が異常に長い repository でも深い slot path を platform の component 長制限内に収める。
const maxRepositoryDirNameLength = 64

// reservedDirNamePrefix は wx が top-level entry（_recovery と旧リリースの _unbound）に予約する prefix である。
// repository directory は一階層下だが同じ prefix を拒否し、orphan scan が旧 _unbound を通常の workspace として扱わないようにする。
const reservedDirNamePrefix = "_"

// RepositoryDirName は slot 内の repository directory 名を、pin 済み dir_name、repository の dir_source、storage.repo_dir_source、main worktree 名の順で解決する。
// 名前は checkout の場所だけに影響し、所有権証明には影響しない。使用値は slot_repositories.dir_name に記録して以後の authority とし、設定/remote URL の変更は新規 slot だけに反映する。
// commentlint:allow-long -- directory 名の解決順序と既存 slot の不変条件を説明する
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
	// filepath.Base は空を返さず、repository ID は hex である。この経路は main path の末尾 component が `/` または `..` の場合だけで、repository ID は常に使える component である。
	return string(repo.ID)
}

// sanitizeDirName は空、separator、control character、`.`、`..`、予約 `_` prefix、ownership marker prefix を拒否し、一 component として安全な名前だけを受け入れる。
// 受理した名前は maxRepositoryDirNameLength に切り詰める。marker と repository directory は slot directory の同じ namespace を使うため、marker prefix は衝突を防ぐために拒否する。
// commentlint:allow-long -- marker と repository directory の namespace 衝突を防ぐため
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

// truncateDirName は rune boundary で byte budget まで value を切る。rune の途中で切ると slot_repositories.dir_name と disk 上の directory 名に無効な UTF-8 を残し、後続検査では検出できない。
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

// UniqueDirName は一 slot 内の repository が同じ directory を要求しないよう、必要なら数値 suffix 付きの name を返す。
// APFS は既定で case-insensitive のため、大文字小文字だけが違う二名は disk 上で一 directory になるが UNIQUE(slot_id, dir_name) では別 row になる。
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
