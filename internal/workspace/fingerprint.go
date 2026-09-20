package workspace

import (
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

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
)

const (
	fingerprintSchemaVersion         = 9
	updateCompatibilitySchemaVersion = 4
)

// UpdateCompatibilityFingerprint は既存 worktree の差分更新では変更できない準備条件だけを hash 化する。
// OID、include/link の配置内容、readiness、reuse 方針は更新時に再計算できるため含めない。
// schema=3 は submodule 方針を含める。更新経路は submodule を実体化せず、false で作った standby を true 相当へ変換できない。
// schema=4 は source worktree の sparse checkout 方針を含める。更新経路は
// 既存 worktree の skip-worktree 範囲を安全に組み替えないため、パターン変更後に更新しない。
// commentlint:allow-long -- schema を上げた理由と、更新互換側にも要る条件を保守時に確認できるようにする
func UpdateCompatibilityFingerprint(generation int, repo discovery.Repository, c config.Config) (string, error) {
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
	if err := writeSparseCheckoutFingerprint(h, repo); err != nil {
		return "", err
	}
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
// commentlint:allow-long -- 契約と安全条件を保持する説明のため
func Fingerprint(generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
	return fingerprintWithSchema(fingerprintSchemaVersion, generation, oid, repo, c)
}

func fingerprintWithSchema(schema, generation int, oid string, repo discovery.Repository, c config.Config) (string, error) {
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
	if err := validateRuleConflicts(nil, linkPatterns); err != nil {
		return "", err
	}
	linkSources, err := inspectLinkSources(sourceRoot, linkPatterns)
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
	// default include は Fingerprint が Git runner を持たないため、copyIncludesAt の tracked 検査なしで hash 化する。
	// tracked file も checkout に任せるため、main worktree の編集で再利用できた slot も cold start 時に再構築される。untracked file を除外すると古い local rule を持つ slot を渡してしまう。
	// default file がない場合の切り替えで materialized worktree は変わらないため、設定自体は意図的に hash 化しない。
	defaults, err := defaultIncludeCandidatesForRepository(repo, c, linkPatterns)
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
	if err := writeSparseCheckoutFingerprint(h, repo); err != nil {
		return "", err
	}
	if err := verifyPinnedRepositoryPath(sourceRoot, mainPath); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeSparseCheckoutFingerprint は source worktree の sparse 選択を fingerprint へ含める。
// sparse の実効値は Git の worktree-local config と worktree ごとの info/sparse-checkout にあり、
// OID や wx の設定だけでは path set の変更を検出できない。disabled のときは残った古い pattern を無視する。
func writeSparseCheckoutFingerprint(h hash.Hash, repo discovery.Repository) error {
	enabled, cone, patterns, err := sparseCheckoutSettings(repo)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(h, "sparse-checkout-enabled=%t\n", enabled)
	if !enabled {
		return nil
	}
	_, _ = fmt.Fprintf(h, "sparse-checkout-cone=%t\n", cone)
	_, _ = fmt.Fprintf(h, "sparse-checkout-patterns=%x\n", sha256.Sum256(patterns))
	return nil
}

// sparseCheckoutSettings は Git が source worktree に適用する sparse の設定と pattern を読む。
// Fingerprint は Git runner を持たないため、Git の local config のうち sparse に関係する値だけを読む。
// `.git` が無いテスト用・未初期化ディレクトリは非 sparse として扱い、既存の fingerprint 契約を保つ。
func sparseCheckoutSettings(repo discovery.Repository) (enabled, cone bool, patterns []byte, err error) {
	gitDir, found, err := worktreeGitDir(string(repo.MainPath))
	if err != nil {
		return false, false, nil, err
	}
	if !found {
		return false, false, nil, nil
	}
	commonDir := gitDir
	if data, readErr := os.ReadFile(filepath.Join(gitDir, "commondir")); readErr == nil {
		value := strings.TrimSpace(string(data))
		if value != "" {
			commonDir = value
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(gitDir, commonDir)
			}
			commonDir, err = filepath.Abs(filepath.Clean(commonDir))
			if err != nil {
				return false, false, nil, err
			}
		}
	}
	values, err := readGitConfigValues(filepath.Join(commonDir, "config"))
	if err != nil {
		return false, false, nil, err
	}
	// config.worktree は extensions.worktreeConfig が有効なときだけ Git が読む。
	worktreeConfig := gitBoolConfig(values["extensions.worktreeconfig"])
	if worktreeConfig {
		worktreeValues, readErr := readGitConfigValues(filepath.Join(gitDir, "config.worktree"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return false, false, nil, readErr
		}
		for key, value := range worktreeValues {
			values[key] = value
		}
	}
	enabled, err = parseGitBoolConfig(values["core.sparsecheckout"])
	if err != nil {
		return false, false, nil, err
	}
	if !enabled {
		return false, false, nil, nil
	}
	cone, err = parseGitBoolConfig(values["core.sparsecheckoutcone"])
	if err != nil {
		return false, false, nil, err
	}
	patternDir := commonDir
	if worktreeConfig {
		patternDir = gitDir
	}
	patterns, err = os.ReadFile(filepath.Join(patternDir, "info", "sparse-checkout"))
	if errors.Is(err, os.ErrNotExist) {
		patterns = nil
		err = nil
	}
	if err != nil {
		return false, false, nil, fmt.Errorf("read sparse checkout patterns: %w", err)
	}
	return enabled, cone, patterns, nil
}

// worktreeGitDir は worktree の `.git` directory/file から、その worktree 専用 Git directory を解決する。
func worktreeGitDir(worktree string) (string, bool, error) {
	gitPath := filepath.Join(worktree, ".git")
	info, err := os.Stat(gitPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		absolute, absErr := filepath.Abs(filepath.Clean(gitPath))
		return absolute, true, absErr
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(strings.ToLower(line), "gitdir:") {
		return "", false, fmt.Errorf("invalid .git file in %s", worktree)
	}
	value := strings.TrimSpace(line[len("gitdir:"):])
	if value == "" {
		return "", false, fmt.Errorf("empty gitdir in %s", worktree)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(worktree, value)
	}
	absolute, err := filepath.Abs(filepath.Clean(value))
	return absolute, true, err
}

// readGitConfigValues は sparse 判定に必要な範囲の Git config を読む小さな parser である。
// include は local config の sparse 値を上書きしないため展開せず、同じ key の後続値を採用する。
func readGitConfigValues(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	section := ""
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(strings.TrimSuffix(raw, "\r"))
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")))
			if index := strings.IndexByte(section, ' '); index >= 0 {
				section = section[:index]
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			key, value = line, "true"
		}
		if section == "" {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		values[section+"."+key] = value
	}
	return values, nil
}

func gitBoolConfig(value string) bool {
	parsed, err := parseGitBoolConfig(value)
	return err == nil && parsed
}

func parseGitBoolConfig(value string) (bool, error) {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(value), "\"'")) {
	case "true", "yes", "on", "1":
		return true, nil
	case "", "false", "no", "off", "0":
		return false, nil
	default:
		return false, fmt.Errorf("invalid Git boolean %q", value)
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
