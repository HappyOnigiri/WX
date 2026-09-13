package archive

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// gitStateDirectories と gitStateFiles は、停止中 rebase の進行情報を持つ worktree 専用 gitdir 直下の path である。
// merge backend は rebase-merge/、apply backend は rebase-apply/ を使い、どちらも tree にも index にも現れない。
var (
	gitStateDirectories = []string{"rebase-apply", "rebase-merge"}
	gitStateFiles       = []string{"AUTO_MERGE", "ORIG_HEAD", "REBASE_HEAD"}
)

// gitStateCommitParents は、制御ファイルのうち復元後の HEAD から到達できない commit を指すものである。
// 保護 commit の親に並べることで、残りの todo・done が指す commit も orig-head 経由で到達可能になる。
var gitStateCommitParents = []string{
	"ORIG_HEAD",
	"REBASE_HEAD",
	"rebase-apply/onto",
	"rebase-apply/orig-head",
	"rebase-merge/onto",
	"rebase-merge/orig-head",
	"rebase-merge/stopped-sha",
}

// gitStateEntry は gitdir 相対の制御ファイル 1 件を表す。path は slash 区切りで、tree の path にそのまま使う。
type gitStateEntry struct {
	path    string
	content []byte
}

// gitRunner は worktree に束縛した Git 実行の最小契約で、snapshot と restore の双方が同じ手順を共有するために使う。
type gitRunner func(env []string, input []byte, args ...string) (gitx.Result, error)

// openGitStateRoot は worktree 専用 gitdir を pin して返す。
// gitdir は wx の worktree root ではなくソースリポジトリの common dir 配下にあるため、
// ここで開いた descriptor 自体を読み書きの境界として扱う。
func openGitStateRoot(value func(env []string, args ...string) (string, error)) (*os.Root, error) {
	gitDir, err := value(nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, fmt.Errorf("resolve worktree git directory: %w", err)
	}
	if gitDir == "" {
		return nil, errors.New("worktree git directory is empty")
	}
	root, _, err := domain.OpenOwnedRoot(gitDir, gitDir)
	if err != nil {
		return nil, fmt.Errorf("open worktree git directory: %w", err)
	}
	return root, nil
}

// collectGitState は pin した gitdir から制御ファイルを読み、path 順に並べて返す。
// 1 件も無ければ進行中 rebase が無いことを表す空 slice を返す。
// regular file 以外を見つけた場合は、内容を取りこぼしたまま成功にしないためにエラーにする。
func collectGitState(root *os.Root) ([]gitStateEntry, error) {
	var entries []gitStateEntry
	for _, directory := range gitStateDirectories {
		info, err := root.Lstat(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", directory, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is not a physical directory", directory)
		}
		collected, err := collectGitStateDirectory(root, directory)
		if err != nil {
			return nil, err
		}
		entries = append(entries, collected...)
	}
	for _, name := range gitStateFiles {
		entry, ok, err := readGitStateFile(root, name)
		if err != nil {
			return nil, err
		}
		if ok {
			entries = append(entries, entry)
		}
	}
	slices.SortFunc(entries, func(a, b gitStateEntry) int { return strings.Compare(a.path, b.path) })
	return entries, nil
}

func collectGitStateDirectory(root *os.Root, directory string) ([]gitStateEntry, error) {
	handle, err := root.Open(directory)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", directory, err)
	}
	names, err := handle.ReadDir(-1)
	closeErr := handle.Close()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", directory, err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var entries []gitStateEntry
	for _, name := range names {
		child := path.Join(directory, name.Name())
		if name.IsDir() {
			nested, err := collectGitStateDirectory(root, child)
			if err != nil {
				return nil, err
			}
			entries = append(entries, nested...)
			continue
		}
		entry, ok, err := readGitStateFile(root, child)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%s disappeared while reading rebase state", child)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// readGitStateFile は 1 件を読み、存在しなければ ok=false を返す。空ファイルにも意味があるため内容の長さは問わない。
func readGitStateFile(root *os.Root, name string) (gitStateEntry, bool, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return gitStateEntry{}, false, nil
	}
	if err != nil {
		return gitStateEntry{}, false, fmt.Errorf("inspect %s: %w", name, err)
	}
	if !info.Mode().IsRegular() {
		return gitStateEntry{}, false, fmt.Errorf("%s is not a regular file", name)
	}
	content, err := root.ReadFile(name)
	if err != nil {
		return gitStateEntry{}, false, fmt.Errorf("read %s: %w", name, err)
	}
	return gitStateEntry{path: name, content: content}, true, nil
}

// writeGitStateTree は制御ファイルを blob 化して一時 index へ並べ、gitdir 相対の tree を書き出す。
// entries が空なら空文字を返し、新しい object を作らない。
func writeGitStateTree(run gitRunner, entries []gitStateEntry) (string, error) {
	if len(entries) == 0 {
		return "", nil
	}
	tmp, cleanup, err := temporaryIndex("rebase state", ".wx-gitstate-index-*")
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	// 作成直後の 0 byte file を git は「小さすぎる index」として拒むため、空 index として初期化してから積み上げる。
	if _, err := run(env, nil, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("initialize rebase state index: %w", err)
	}
	for _, entry := range entries {
		blob, err := run(env, entry.content, "hash-object", "-w", "--stdin")
		if err != nil {
			return "", fmt.Errorf("store %s: %w", entry.path, err)
		}
		oid := strings.TrimSpace(blob.Stdout)
		if _, err := run(env, nil, "update-index", "--add", "--cacheinfo", "100644,"+oid+","+entry.path); err != nil {
			return "", fmt.Errorf("stage %s: %w", entry.path, err)
		}
	}
	tree, err := run(env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write rebase state tree: %w", err)
	}
	return strings.TrimSpace(tree.Stdout), nil
}

// gitStateParents は保護 commit の親を返す。head に加えて制御ファイルが指す commit を並べ、
// SNAPSHOT job の再実行で同じ commit OID になるよう重複を除いて辞書順に固定する。
func gitStateParents(run gitRunner, entries []gitStateEntry, head string) []string {
	byPath := make(map[string]string, len(entries))
	for _, entry := range entries {
		byPath[entry.path] = strings.TrimSpace(string(entry.content))
	}
	parents := []string{head}
	for _, name := range gitStateCommitParents {
		candidate, ok := byPath[name]
		if !ok || candidate == "" || slices.Contains(parents, candidate) {
			continue
		}
		if _, err := run(nil, nil, "cat-file", "-e", candidate+"^{commit}"); err != nil {
			continue
		}
		parents = append(parents, candidate)
	}
	slices.Sort(parents)
	return slices.Compact(parents)
}

// captureGitState は停止中 rebase の制御ファイルを 1 本の commit にまとめ、その OID を返す。
// 進行中 rebase が無ければ空文字を返し、object も ref も作らない。
func captureGitState(value func(env []string, args ...string) (string, error), run gitRunner, head string) (string, error) {
	root, err := openGitStateRoot(value)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	entries, err := collectGitState(root)
	if err != nil {
		return "", err
	}
	tree, err := writeGitStateTree(run, entries)
	if err != nil || tree == "" {
		return "", err
	}
	args := []string{"commit-tree", tree}
	for _, parent := range gitStateParents(run, entries, head) {
		args = append(args, "-p", parent)
	}
	commit, err := run(recoveryCommitEnv(nil), []byte("wx rebase state snapshot\n"), args...)
	if err != nil {
		return "", fmt.Errorf("commit rebase state: %w", err)
	}
	return strings.TrimSpace(commit.Stdout), nil
}

// restoreGitState は復元先 gitdir の制御ファイルを snapshot の内容へ揃える。
// snapshot に進行中 rebase が無くても削除だけは行い、再利用された slot が古い進行情報を引き継がないようにする。
// 書き戻した内容から tree を再計算して照合し、一致しなければ復元を失敗させる。
func restoreGitState(value func(env []string, args ...string) (string, error), run gitRunner, wantTree string) error {
	root, err := openGitStateRoot(value)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := clearGitState(root); err != nil {
		return err
	}
	if wantTree == "" {
		return nil
	}
	if err := expandGitStateTree(root, run, wantTree); err != nil {
		return err
	}
	entries, err := collectGitState(root)
	if err != nil {
		return err
	}
	tree, err := writeGitStateTree(run, entries)
	if err != nil {
		return err
	}
	if tree != wantTree {
		return errors.New("restored rebase state does not match snapshot")
	}
	return nil
}

func clearGitState(root *os.Root) error {
	for _, directory := range gitStateDirectories {
		if err := root.RemoveAll(directory); err != nil {
			return fmt.Errorf("clear %s: %w", directory, err)
		}
	}
	for _, name := range gitStateFiles {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear %s: %w", name, err)
		}
	}
	return nil
}

func expandGitStateTree(root *os.Root, run gitRunner, tree string) error {
	listing, err := run(nil, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return fmt.Errorf("list rebase state tree: %w", err)
	}
	for _, record := range strings.Split(listing.Stdout, "\x00") {
		if record == "" {
			continue
		}
		meta, relative, found := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 {
			return fmt.Errorf("unexpected rebase state tree record %q", record)
		}
		if err := writeGitStateEntry(root, run, fields[2], relative); err != nil {
			return err
		}
	}
	return nil
}

func writeGitStateEntry(root *os.Root, run gitRunner, oid, relative string) error {
	if directory := path.Dir(relative); directory != "." {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", directory, err)
		}
	}
	blob, err := run(nil, nil, "cat-file", "blob", oid)
	if err != nil {
		return fmt.Errorf("read snapshot object for %s: %w", relative, err)
	}
	if err := root.WriteFile(relative, []byte(blob.Stdout), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", relative, err)
	}
	return nil
}
