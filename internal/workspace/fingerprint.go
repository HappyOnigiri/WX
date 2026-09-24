package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/HappyOnigiri/WorktreeX/internal/config"
	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
	"github.com/HappyOnigiri/WorktreeX/internal/domain"
	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

const (
	fingerprintSchemaVersion         = 10
	updateCompatibilitySchemaVersion = 4
)

// UpdateCompatibilityFingerprint は既存 worktree の差分更新では変更できない準備条件だけを hash 化する。
// OID、include/link の配置内容、readiness、reuse 方針は更新時に再計算できるため含めない。
// schema=3 は submodule 方針を含める。更新経路は submodule を実体化せず、false で作った standby を true 相当へ変換できない。
// schema=4 は source worktree の sparse checkout 方針を含める。更新経路は
// 既存 worktree の skip-worktree 範囲を安全に組み替えないため、パターン変更後に更新しない。
// commentlint:allow-long -- schema を上げた理由と、更新互換側にも要る条件を保守時に確認できるようにする
func UpdateCompatibilityFingerprint(generation int, repo discovery.Repository, c config.Config) (string, error) {
	return UpdateCompatibilityFingerprintWithGit(context.Background(), &gitx.Runner{}, generation, repo, c)
}

// UpdateCompatibilityFingerprintWithGit は Git の実効設定を使って更新互換 fingerprint を作る。
// daemon の Git runner を渡すことで、include と worktree-local config を Git 自身に解決させる。
func UpdateCompatibilityFingerprintWithGit(ctx context.Context, git *gitx.Runner, generation int, repo discovery.Repository, c config.Config) (string, error) {
	sparse, err := ReadSparseCheckout(ctx, git, repo)
	if err != nil {
		return "", err
	}
	return UpdateCompatibilityFingerprintWithSparse(generation, repo, c, sparse)
}

// UpdateCompatibilityFingerprintWithSparse は読み取り済みの sparse 設定で更新互換 fingerprint を作る。
func UpdateCompatibilityFingerprintWithSparse(generation int, repo discovery.Repository, c config.Config, sparse SparseCheckout) (string, error) {
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return "", err
	}
	submodules := c.RepositoryFor(workspaceRoot, repo.RelativePath, string(repo.MainPath)).Submodules
	if submodules == nil {
		value, _ := c.SubmodulesForWorkspace(workspaceRoot)
		submodules = &value
	}
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=%d\ngeneration=%d\ncopy_mode=%s\ncow_min_size_kib=%d\nsubmodules=%t\n",
		updateCompatibilitySchemaVersion, generation, c.CopyModeForWorkspaceRepository(workspaceRoot, repo.RelativePath, string(repo.MainPath)), c.COWMinSizeKiBForWorkspaceRepository(workspaceRoot, repo.RelativePath, string(repo.MainPath)), *submodules)
	if err := writePrepareFingerprint(h, repo, c); err != nil {
		return "", err
	}
	sparse.write(h)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fingerprint は prepared worktree を再利用可能にするすべての情報を hash 化する。slot 内の repository directory 名は意図的に含めない。
// slot が存在すれば slot_repositories.dir_name が権威となり、既存 slot は記録済みの名前を保つ。
// 新規 slot だけが変更後の storage.repo_dir_source や repositories.<path>.dir_name を使う。
// 名前を hash 化しても reuse check は保存済みの名前から再計算するため常に自身と一致し、挙動は変わらない。
// schema=6 はコピー方式を準備入力に含め、方式変更後に以前の READY slot を再利用しない。
// schema=7 は共有下限も含め、下限変更後の貸出で以前の下限で作った slot を再利用しない。
// 下限とコピー方式は repository ごとに解決した実効値を入れる。schema は上げない。
// 個別指定を足した repository は値そのものが変わって hash が変わり、他 repository の READY slot は生かしたままにできる。
// schema=8 は submodule 実体化の方針も含め、方針変更後に以前の READY slot を再利用しない。
// schema=9 は source worktree の sparse checkout 方針とパターンを含め、パターン変更後に
// 以前の READY slot が古い path set のまま貸し出されないようにする。
// schema=10 は .worktreelink の glob 展開を hash 対象に含める。展開前は pattern のまま source を検査していたため、
// glob のマッチ集合が増減しても値が動かなかった。
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
func Fingerprint(generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	return FingerprintWithGit(context.Background(), &gitx.Runner{}, generation, oid, repo, c)
}

// FingerprintWithGit は Git の実効設定を使って prepared worktree の fingerprint を作る。
// daemon は自身の Runner を渡し、Git の環境変数を浄化する既存の実行契約を維持する。
func FingerprintWithGit(ctx context.Context, git *gitx.Runner, generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	return fingerprintWithSchemaAndGit(ctx, git, fingerprintSchemaVersion, generation, oid, repo, c)
}

// FingerprintWithSparse は読み取り済みの sparse 設定で prepared worktree の fingerprint を作る。
func FingerprintWithSparse(generation int, oid string, repo discovery.Repository, c config.Config, sparse SparseCheckout) (string, error) {
	return fingerprintWithSchemaAndSparse(fingerprintSchemaVersion, generation, oid, repo, c, sparse)
}

func fingerprintWithSchema(schema, generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	return fingerprintWithSchemaAndGit(context.Background(), &gitx.Runner{}, schema, generation, oid, repo, c)
}

func fingerprintWithSchemaAndGit(ctx context.Context, git *gitx.Runner, schema, generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	sparse, err := ReadSparseCheckout(ctx, git, repo)
	if err != nil {
		return "", err
	}
	return fingerprintWithSchemaAndSparse(schema, generation, oid, repo, c, sparse)
}

func fingerprintWithSchemaAndSparse(schema, generation int, oid string, repo discovery.Repository, c config.Config, sparse SparseCheckout) (string, error) {
	mainPath := string(repo.MainPath)
	workspaceRoot, rootErr := repositoryWorkspaceRoot(repo)
	if rootErr != nil {
		return "", rootErr
	}
	sourceRoot, err := openPinnedRepositoryRoot(mainPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = sourceRoot.Close() }()
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "schema=%d\ngeneration=%d\noid=%s\ncopy_mode=%s\ncow_min_size_kib=%d\n",
		schema, generation, oid, c.CopyModeForWorkspaceRepository(workspaceRoot, repo.RelativePath, mainPath), c.COWMinSizeKiBForWorkspaceRepository(workspaceRoot, repo.RelativePath, mainPath))
	var linkPatterns []string
	for _, name := range []string{".worktreeinclude", ".worktreelink"} {
		data, err := readPhysicalManifestAt(sourceRoot, name)
		if err != nil {
			return "", err
		}
		_, _ = fmt.Fprintf(h, "manifest=%s:%x\n", name, sha256.Sum256(data))
		if name == ".worktreelink" && data != nil {
			linkPatterns, err = parsePhysicalPatterns(data)
			if err != nil {
				return "", err
			}
		}
	}
	links, err := expandLinkPatternsAt(sourceRoot, linkPatterns)
	if err != nil {
		return "", err
	}
	if err := validateRuleConflicts(nil, links); err != nil {
		return "", err
	}
	linkSources, err := inspectLinkSources(sourceRoot, links)
	if err != nil {
		return "", err
	}
	sort.SliceStable(linkSources, func(i, j int) bool {
		return linkSources[i].relative < linkSources[j].relative
	})
	for _, source := range linkSources {
		_, _ = fmt.Fprintf(h, "worktreelink-source=%s:present=%t\n", source.relative, source.present)
	}
	patterns, err := readPhysicalPatternsAt(sourceRoot, ".worktreeinclude")
	if err != nil {
		return "", err
	}
	seenIncludes := map[string]bool{}
	// default include は copyIncludesAt の tracked 検査なしで hash 化する。
	// tracked file も checkout に任せるため、main worktree の編集で再利用できた slot も cold start 時に再構築される。untracked file を除外すると古い local rule を持つ slot を渡してしまう。
	// default file がない場合の切り替えで materialized worktree は変わらないため、設定自体は意図的に hash 化しない。
	defaults, err := defaultIncludeCandidatesForRepository(repo, c, links)
	if err != nil {
		return "", err
	}
	for _, rel := range defaults {
		seenIncludes[rel] = true
		if err := fingerprintPath(h, mainPath, filepath.Join(mainPath, rel)); err != nil {
			return "", err
		}
	}
	for _, pattern := range patterns {
		clean := filepath.Clean(pattern)
		if filepath.IsAbs(pattern) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("unsafe .worktreeinclude pattern %q", pattern)
		}
		matches, err := safeGlob(mainPath, pattern)
		if err != nil {
			return "", err
		}
		for _, match := range matches {
			rel, err := filepath.Rel(mainPath, match)
			if err != nil {
				return "", err
			}
			rel, err = safeRelative(rel)
			if err != nil {
				return "", err
			}
			if seenIncludes[rel] {
				continue
			}
			seenIncludes[rel] = true
			if err := fingerprintPath(h, string(repo.MainPath), match); err != nil {
				return "", err
			}
		}
	}
	if err := fingerprintWorkspaceRoot(h, repo, c); err != nil {
		return "", err
	}
	if err := writePrepareFingerprint(h, repo, c); err != nil {
		return "", err
	}
	sparse.write(h)
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SparseCheckout は source worktree の sparse 選択の読み取り結果で、fingerprint へ含める。
// 実効値は Git の worktree-local config と info/sparse-checkout にあり、OID や wx の設定だけでは path set の変更を検出できない。
// 更新互換 fingerprint と fingerprint を続けて作る呼び出し側は、1回読んだ値を両方へ渡す。
type SparseCheckout struct {
	enabled  bool
	cone     bool
	patterns []byte
}

// ReadSparseCheckout は Git が source worktree に適用する sparse の設定と pattern を読む。
func ReadSparseCheckout(ctx context.Context, git *gitx.Runner, repo discovery.Repository) (SparseCheckout, error) {
	enabled, cone, patterns, err := sparseCheckoutSettings(ctx, git, repo)
	if err != nil {
		return SparseCheckout{}, err
	}
	return SparseCheckout{enabled: enabled, cone: cone, patterns: patterns}, nil
}

// write は disabled のとき、残った古い pattern を無視する。
func (s SparseCheckout) write(h hash.Hash) {
	_, _ = fmt.Fprintf(h, "sparse-checkout-enabled=%t\n", s.enabled)
	if !s.enabled {
		return
	}
	_, _ = fmt.Fprintf(h, "sparse-checkout-cone=%t\n", s.cone)
	_, _ = fmt.Fprintf(h, "sparse-checkout-patterns=%x\n", sha256.Sum256(s.patterns))
}

// sparseCheckoutSettings は Git が source worktree に適用する sparse の設定と pattern を読む。
// 設定の include・コメント・worktree-local config の解釈を Git に任せ、wx 独自 parser と実効値がずれないようにする。
// `.git` が無いテスト用・未初期化ディレクトリは非 sparse として扱い、既存の fingerprint 契約を保つ。
func sparseCheckoutSettings(ctx context.Context, git *gitx.Runner, repo discovery.Repository) (enabled, cone bool, patterns []byte, err error) {
	if git == nil {
		git = &gitx.Runner{}
	}
	mainPath := string(repo.MainPath)
	if _, err := os.Lstat(filepath.Join(mainPath, ".git")); errors.Is(err, os.ErrNotExist) {
		// Git が親ディレクトリの設定を探索する前に、worktree として管理されていない
		// テスト用・未初期化ディレクトリを既存契約どおり非 sparse として扱う。
		return false, false, nil, nil
	} else if err != nil {
		return false, false, nil, fmt.Errorf("inspect Git metadata: %w", err)
	}
	if _, err := git.Run(ctx, mainPath, "rev-parse", "--git-dir"); err != nil {
		if gitx.IsNotRepository(err) {
			return false, false, nil, nil
		}
		return false, false, nil, fmt.Errorf("resolve Git directory: %w", err)
	}
	enabled, err = gitConfigBool(ctx, git, mainPath, "core.sparseCheckout")
	if err != nil || !enabled {
		return enabled, false, nil, err
	}
	cone, err = gitConfigBool(ctx, git, mainPath, "core.sparseCheckoutCone")
	if err != nil {
		return false, false, nil, err
	}
	result, err := git.Run(ctx, mainPath, "rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout")
	if err != nil {
		return false, false, nil, fmt.Errorf("resolve sparse checkout patterns: %w", err)
	}
	patternPath := strings.TrimSpace(result.Stdout)
	if patternPath == "" {
		return false, false, nil, errors.New("Git returned an empty sparse checkout pattern path")
	}
	patterns, err = os.ReadFile(patternPath)
	if errors.Is(err, os.ErrNotExist) {
		return enabled, cone, nil, nil
	}
	if err != nil {
		return false, false, nil, fmt.Errorf("read sparse checkout patterns: %w", err)
	}
	return enabled, cone, patterns, nil
}

func gitConfigBool(ctx context.Context, git *gitx.Runner, directory, key string) (bool, error) {
	result, err := git.Run(ctx, directory, "config", "--includes", "--type=bool", "--default", "false", "--get", key)
	if err != nil {
		return false, fmt.Errorf("read Git config %s: %w", key, err)
	}
	switch strings.ToLower(strings.TrimSpace(result.Stdout)) {
	case "true":
		return true, nil
	case "false", "":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected Git boolean config %s=%q", key, strings.TrimSpace(result.Stdout))
	}
}

// fingerprintWorkspaceRoot は workspace root の rule と、その rule が配置する実体を hash 化する。
// rule の解決は消費側と同じ関数に任せ、配置しない実体を混ぜない。
func fingerprintWorkspaceRoot(h hash.Hash, repo discovery.Repository, c config.Config) error {
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return err
	}
	rules, err := rootRulesForRepository(repo, workspaceRoot, c)
	if err != nil {
		return err
	}
	resolvedSubmodules := c.RepositoryFor(workspaceRoot, repo.RelativePath, string(repo.MainPath)).Submodules
	var submodules bool
	if resolvedSubmodules != nil {
		submodules = *resolvedSubmodules
	} else {
		submodules, _ = c.SubmodulesForWorkspace(workspaceRoot)
	}
	_, _ = fmt.Fprintf(h, "workspace-root=%s\ncopy-rules=%q\nlink-rules=%q\nsubmodules=%t\n", workspaceRoot, rules.Copy, rules.Link, submodules)
	copyNames, explicitCopies, err := workspaceRootCopyPlan(rules)
	if err != nil {
		return err
	}
	workspaceRootHandle, err := OpenPhysicalRoot(workspaceRoot)
	if err != nil {
		return err
	}
	defer func() { _ = workspaceRootHandle.Close() }()
	presentCopies, err := validateWorkspaceRootCopySources(nil, workspaceRootHandle, workspaceRoot, copyNames, explicitCopies)
	if err != nil {
		return err
	}
	seenCopies := map[string]bool{}
	for _, name := range copyNames {
		clean, err := safeRelative(name)
		if err != nil {
			return err
		}
		if seenCopies[clean] {
			continue
		}
		seenCopies[clean] = true
		if !presentCopies[clean] {
			continue
		}
		if err := fingerprintRootPath(h, workspaceRootHandle, clean, clean); err != nil {
			return err
		}
	}
	for _, name := range rules.Link {
		clean, err := safeRelative(name)
		if err != nil {
			return err
		}
		path := filepath.Join(workspaceRoot, clean)
		// MaterializeRootAt が skip する source なので、fingerprint も内容ではなく skip した事実だけを混ぜる。
		if err := domain.ValidatePhysicalLeaf(path); err != nil {
			if errors.Is(err, domain.ErrSymlinkPath) {
				_, _ = fmt.Fprintf(h, "workspace-link=%s:skipped-symlink\n", clean)
				continue
			}
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(h, "workspace-link=%s:%s\n", clean, info.Mode())
	}
	return nil
}

// writePrepareFingerprint は準備コマンドと明示した version だけを fingerprint へ含める。
// prepare.inputs は更新時に現在の Git 差分と配置差分を判定するための値で、設定を追加しただけで
// 既存の READY slot を一斉に無効化しないよう fingerprint へは含めない。
func writePrepareFingerprint(h hash.Hash, repo discovery.Repository, c config.Config) error {
	workspaceRoot, err := repositoryWorkspaceRoot(repo)
	if err != nil {
		return err
	}
	o := c.RepositoryFor(workspaceRoot, repo.RelativePath, string(repo.MainPath))
	if len(o.Prepare.Command) == 0 && o.Prepare.Version == "" {
		return nil
	}
	prepareInput, err := json.Marshal(struct {
		Command []string `json:"command"`
		Version string   `json:"version"`
	}{Command: o.Prepare.Command, Version: o.Prepare.Version})
	if err != nil {
		return fmt.Errorf("marshal prepare fingerprint input: %w", err)
	}
	_, _ = fmt.Fprintf(h, "prepare=%s\n", prepareInput)
	return nil
}

// rootRulesForRepository は workspace root の rule を workspace の種別に合わせて解決する。
// repository workspace では root が repository の main worktree そのもので、root 直下は Git が checkout する。
// manifest も agent 資産の既定もここで効かせると、配置しない実体を fingerprint に混ぜて READY slot を無効化してしまう。
func rootRulesForRepository(repo discovery.Repository, workspaceRoot string, c config.Config) (RootRules, error) {
	if isRepositoryWorkspaceLayout(repo) {
		return RootRulesFromConfig(c.WorkspaceFor(workspaceRoot)), nil
	}
	return ResolveRootRules(workspaceRoot, c.WorkspaceFor(workspaceRoot))
}

// isRepositoryWorkspaceLayout は repository が workspace root そのものかを返す。偽なら root は Git 管理外の multi-repository root である。
func isRepositoryWorkspaceLayout(repo discovery.Repository) bool {
	return repo.RelativePath == "" || filepath.Clean(repo.RelativePath) == "."
}

func repositoryWorkspaceRoot(repo discovery.Repository) (string, error) {
	if isRepositoryWorkspaceLayout(repo) {
		return string(repo.MainPath), nil
	}
	rel, err := safeRelative(repo.RelativePath)
	if err != nil {
		return "", err
	}
	root := string(repo.MainPath)
	for range strings.Split(rel, string(filepath.Separator)) {
		root = filepath.Dir(root)
	}
	return root, nil
}

func fingerprintPath(h hash.Hash, root, path string) error {
	absoluteRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	absolutePath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absoluteRoot, absolutePath)
	if err != nil {
		return err
	}
	rel, err = safeRelative(rel)
	if err != nil {
		return err
	}
	rootHandle, err := OpenPhysicalRoot(absoluteRoot)
	if err != nil {
		return err
	}
	defer func() { _ = rootHandle.Close() }()
	return fingerprintRootPath(h, rootHandle, rel, rel)
}

func fingerprintRootPath(h hash.Hash, root *os.Root, relative, display string) error {
	info, err := domain.PhysicalPathInfo(root, relative)
	if err != nil {
		// symlink は materializer が辿らず skip するため、内容ではなく skip した事実だけを混ぜて再利用判定を一致させる。
		if errors.Is(err, domain.ErrSymlinkPath) {
			_, _ = fmt.Fprintf(h, "path=%s skipped-symlink\n", display)
			return nil
		}
		return err
	}
	_, _ = fmt.Fprintf(h, "path=%s mode=%s size=%d\n", display, info.Mode(), info.Size())
	if info.IsDir() {
		directory, err := root.OpenFile(relative, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		openedInfo, statErr := directory.Stat()
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = directory.Close()
			return fmt.Errorf("fingerprint directory %s changed while opening", display)
		}
		entries, readErr := directory.Readdirnames(-1)
		closeErr := directory.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		sort.Strings(entries)
		for _, name := range entries {
			child := filepath.Join(relative, name)
			childDisplay := filepath.Join(display, name)
			if err := fingerprintRootPath(h, root, child, childDisplay); err != nil {
				return err
			}
		}
		return nil
	}
	file, err := root.OpenFile(relative, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		_ = file.Close()
		return fmt.Errorf("fingerprint file %s changed while opening", display)
	}
	_, copyErr := io.Copy(h, file)
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
