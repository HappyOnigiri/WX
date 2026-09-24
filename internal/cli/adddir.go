package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/daemon"
)

// addDirArgs は dirs を --add-dir として agent 引数の前へ差し込む。
// 利用者が自分で --add-dir を渡していても抑制しない。指定は加算で、重ねても agent 側でエラーにならない。
func addDirArgs(dirs, args []string) []string {
	if len(dirs) == 0 {
		return args
	}
	// 容量は argv の契約に含まれず、dirs の件数と args の長さを予測するための
	// 計算だけをここへ持ち込むと、加算・乗算の変異を意味のある差分と誤認する。
	combined := make([]string, 0)
	for _, dir := range dirs {
		combined = append(combined, "--add-dir", dir)
	}
	return append(combined, args...)
}

// leaseAddDirs は worktree で起動する agent へ渡す repository directory を返す。
// 名前は daemon が記録済みの dir_name から返したもので、client 側では再計算しない。
// 絶対 path にするのは、codex の resume が --cd で CWD を移すため、相対名の解決先が起動時の CWD と変わり得るからである。
// commentlint:allow-long -- 絶対 path にする理由を残す
func leaseAddDirs(cfg config.Config, lease daemon.Lease) []string {
	// SourceWorkspace は daemon が返した canonical な workspace root で、設定キーと同じ表記になる。
	if mode, _ := cfg.AddDirForWorkspace(lease.SourceWorkspace); mode == config.AgentAddDirOff {
		return nil
	}
	dirs := make([]string, 0, len(lease.RepositoryDirs))
	for _, name := range lease.RepositoryDirs {
		dirs = append(dirs, filepath.Join(lease.Path, name))
	}
	return dirs
}

// directAddDirs は worktree を作らない起動で、CWD 直下の repository を絶対 path で返す。
// 直起動は daemon を通らず記録済みの名前がないため、実際にある directory だけを見る。
// root は設定を引くためだけの workspace root（解決できなければ空で global へ落ちる）で、走査する root は CWD のままにする。
func directAddDirs(cfg config.Config, root string) []string {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	return directAddDirsFrom(cfg, root, cwd)
}

// directAddDirsFrom は走査する directory を明示する。
// 会話の cwd で再開する直起動は、起動場所ではなくその cwd 直下の repository を渡す。
func directAddDirsFrom(cfg config.Config, root, cwd string) []string {
	if mode, _ := cfg.AddDirForWorkspace(root); mode != config.AgentAddDirAlways {
		return nil
	}
	return childRepositoryDirs(cwd)
}

// childRepositoryDirs は root 直下 1 階層の repository directory を返す。
// root 自体が repository のときは、単一 repository の workspace と同じく渡すものがない。
// 探索は 1 階層に閉じる。repository の中の repository は agent が CWD から辿れる。
func childRepositoryDirs(root string) []string {
	if isGitRepository(root) {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		// ReadDir は lstat 相当なので symlink は directory にならず、外の実体を指す入口をそのまま除く。
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(root, entry.Name())
		if isGitRepository(path) {
			dirs = append(dirs, path)
		}
	}
	return dirs
}

// isGitRepository は .git を持つ directory かを返す。linked worktree の .git は file なので種別は問わない。
func isGitRepository(path string) bool {
	_, err := os.Lstat(filepath.Join(path, ".git"))
	return err == nil
}
