package workspace

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/discovery"
	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/gitx"
)

// LFSPointer は Git LFS pointer に含まれる object と展開後の大きさである。
// pointer ではない blob は容量見積りでこの値を持たない。
type LFSPointer struct {
	OID  string
	Size int64
}

// LFSCacheState は common directory の cache object を、pointer の期待値と
// 比較した結果である。診断と準備が同じ分類を使えるよう内部値として保持する。
type LFSCacheState string

const (
	LFSCacheHealthy LFSCacheState = "healthy"
	LFSCacheMissing LFSCacheState = "missing"
	LFSCacheCorrupt LFSCacheState = "corrupt"
)

// LFSObjectInfo は準備時に参照する LFS object の診断情報である。
// Cached が false の object だけが、準備時に common directory 側へ新たに書かれる。
type LFSObjectInfo struct {
	OID       string   `json:"oid"`
	Size      int64    `json:"size"`
	Cached    bool     `json:"cached"`
	CacheSize int64    `json:"cache_size,omitempty"`
	CachePath string   `json:"cache_path,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	// CacheState は JSON 形状へは出さず、欠落と破損を修復側へ伝える。
	CacheState LFSCacheState `json:"-"`
}

// CapacityEstimate は repository 1 件を 1 slot へ準備する際の、書込み下限である。
// WorktreeBytes と LFSCacheBytes は異なる volume に載り得るので分けて返す。
type CapacityEstimate struct {
	// RepositoryID は daemon が同じ report の repository と LFS 内訳を対応付けるための内部値である。
	RepositoryID      string          `json:"-"`
	WorktreeBytes     int64           `json:"worktree_bytes"`
	LFSCacheBytes     int64           `json:"lfs_cache_bytes"`
	LFSExpandedBytes  int64           `json:"lfs_expanded_bytes"`
	BlobBytes         int64           `json:"blob_bytes"`
	TransformedBytes  int64           `json:"transformed_bytes"`
	COWAvoidableBytes int64           `json:"cow_avoidable_bytes"`
	LFSObjects        int             `json:"lfs_objects"`
	MissingLFSObjects int             `json:"missing_lfs_objects"`
	Sparse            bool            `json:"sparse"`
	LFS               []LFSObjectInfo `json:"lfs,omitempty"`
}

type capacityTreeEntry struct {
	Mode string
	OID  string
	Size int64
	Path string
}

// maxLFSPointerBytes は pointer として解析する blob の上限で、巨大な誤判定 blob
// を診断 daemon の一時メモリへ複製しないために置く。
const maxLFSPointerBytes = 1 << 20

// ParseLFSPointer は pointer の形式を検証し、展開後サイズと SHA-256 object を返す。
// Git LFS の仕様にない内容は pointer として扱わず、呼び出し側が blob サイズへ
// 安全に倒せるよう false を返す。
func ParseLFSPointer(data []byte) (LFSPointer, bool) {
	version, oidValue, sizeValue := "", "", ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		switch key {
		case "version":
			version = strings.TrimSpace(value)
		case "oid":
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "sha256:") {
				oidValue = strings.TrimSpace(strings.TrimPrefix(value, "sha256:"))
			}
		case "size":
			sizeValue = strings.TrimSpace(value)
		}
	}
	if version != "https://git-lfs.github.com/spec/v1" || len(oidValue) != 64 {
		return LFSPointer{}, false
	}
	if _, err := hex.DecodeString(oidValue); err != nil {
		return LFSPointer{}, false
	}
	size, err := strconv.ParseInt(sizeValue, 10, 64)
	if err != nil || size < 0 {
		return LFSPointer{}, false
	}
	return LFSPointer{OID: "sha256:" + strings.ToLower(oidValue), Size: size}, true
}

func parseLFSPointer(data []byte) (LFSPointer, bool) { return ParseLFSPointer(data) }

// EstimateCapacity は要求 OID を checkout するために確実に書かれる bytes の下限を
// Git の tree・属性・source index から組み立てる。検査に失敗した場合は daemon が
// 準備を拒否せず warn として扱えるよう error を返す。
func (p *Preparer) EstimateCapacity(ctx context.Context, repo discovery.Repository, oid string) (CapacityEstimate, error) {
	if p == nil || p.Git == nil {
		return CapacityEstimate{}, errors.New("capacity estimate requires a Git runner")
	}
	oid = strings.TrimSpace(oid)
	if oid == "" {
		return CapacityEstimate{}, errors.New("capacity estimate requires a requested object")
	}
	tree, err := p.capacityGit(ctx, repo, nil, "ls-tree", "-r", "-l", "-z", oid)
	if err != nil {
		return CapacityEstimate{}, fmt.Errorf("list requested tree for capacity estimate: %w", err)
	}
	entries, err := parseCapacityTree(tree.Stdout)
	if err != nil {
		return CapacityEstimate{}, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	convertible := map[string]bool{}
	lfsPaths := map[string]bool{}
	if len(paths) > 0 {
		attrs, attrErr := p.capacityAttributes(ctx, repo, oid, []byte(strings.Join(paths, "\x00")+"\x00"))
		if attrErr != nil {
			return CapacityEstimate{}, fmt.Errorf("read checkout attributes for capacity estimate: %w", attrErr)
		}
		convertible = parseCOWConvertiblePaths(attrs.Stdout)
		lfsPaths = parseLFSFilterPaths(attrs.Stdout)
	}
	sourceIndex, err := p.capacityGit(ctx, repo, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return CapacityEstimate{}, fmt.Errorf("read source index for capacity estimate: %w", err)
	}
	sourceOIDs := parseCOWSourceIndexOIDs(sourceIndex.Stdout)
	autocrlf, err := p.capacityGit(ctx, repo, nil, "config", "--default", "false", "--get", "core.autocrlf")
	if err != nil {
		return CapacityEstimate{}, fmt.Errorf("read core.autocrlf for capacity estimate: %w", err)
	}
	sparse, err := p.capacityGit(ctx, repo, nil, "config", "--default", "false", "--get", "core.sparseCheckout")
	if err != nil {
		return CapacityEstimate{}, fmt.Errorf("read sparse checkout setting for capacity estimate: %w", err)
	}
	sparseSkipped, err := p.capacitySparseSkippedPaths(ctx, repo, oid, gitBoolTrue(sparse.Stdout))
	if err != nil {
		return CapacityEstimate{}, err
	}
	targetTracked := make(map[string]bool, len(entries))
	for _, entry := range entries {
		targetTracked[entry.Path] = true
	}
	result := CapacityEstimate{Sparse: gitBoolTrue(sparse.Stdout)}
	if err := p.addRepositoryCopyBytes(repo, &result, targetTracked); err != nil {
		return CapacityEstimate{}, fmt.Errorf("inspect repository copy sources for capacity estimate: %w", err)
	}
	early := p.capacityEarlyPaths(repo, entries)

	// LFS pointer は blob ごとに一度だけ読み、同じ object を複数 path が参照しても
	// cache 側の書込みは二重に数えない。
	blobOIDs := make([]string, 0)
	seenBlob := map[string]bool{}
	for _, entry := range entries {
		if !lfsPaths[entry.Path] || seenBlob[entry.OID] {
			continue
		}
		seenBlob[entry.OID] = true
		blobOIDs = append(blobOIDs, entry.OID)
	}
	pointers, err := p.readCapacityBlobs(ctx, repo, blobOIDs)
	if err != nil {
		return CapacityEstimate{}, err
	}
	objects := map[string]*LFSObjectInfo{}
	for _, entry := range entries {
		result.BlobBytes = addBytes(result.BlobBytes, entry.Size)
		if convertible[entry.Path] {
			result.TransformedBytes = addBytes(result.TransformedBytes, entry.Size)
		}
		if lfsPaths[entry.Path] {
			pointer, pointerOK := pointers[entry.OID]
			written := entry.Size
			if pointerOK {
				written = pointer.Size
				result.LFSExpandedBytes = addBytes(result.LFSExpandedBytes, written)
				if !sparseSkipped[entry.Path] {
					object := objects[pointer.OID]
					if object == nil {
						object = &LFSObjectInfo{OID: pointer.OID, Size: pointer.Size, CachePath: lfsCachePath(repo, pointer.OID)}
						objects[pointer.OID] = object
					}
					object.Paths = append(object.Paths, entry.Path)
				}
			} else {
				// pointer として読めない blob は smudge 済み等の可能性がある。
				// 実際に tree にある blob サイズを下限に使い、cache の不足を推測しない。
				result.LFSExpandedBytes = addBytes(result.LFSExpandedBytes, written)
			}
			result.WorktreeBytes = addBytes(result.WorktreeBytes, written)
			continue
		}
		result.WorktreeBytes = addBytes(result.WorktreeBytes, entry.Size)
	}

	// 変換系属性がある回は配置方式で共有できない path が残り、後段の置換方式で
	// 変換後の bytes が必要になる可能性がある。配置済み path の重複は除外するが、
	// peak の下限を CoW で割り引く条件は変換なしの回に限り、楽観側へ見積もらない。
	canCOW := p.capacityCOWEnabled(repo, strings.TrimSpace(autocrlf.Stdout), convertible, lfsPaths)
	if canCOW {
		minSize := int64(p.Config.COWMinSizeKiBForWorkspaceRepository(p.workspaceRootForRepository(repo), repo.RelativePath, string(repo.MainPath))) * 1024
		for _, entry := range entries {
			if entry.Mode != "100644" && entry.Mode != "100755" || entry.Size < minSize || sourceOIDs[entry.Path] != entry.OID || early[entry.Path] {
				continue
			}
			result.COWAvoidableBytes = addBytes(result.COWAvoidableBytes, entry.Size)
			// 配置方式で避けられる bytes だけを worktree の必要量から引く。
			result.WorktreeBytes -= entry.Size
		}
	}
	for _, object := range objects {
		if err := refreshLFSObjectCacheState(object); err != nil {
			return CapacityEstimate{}, err
		}
		if !object.Cached {
			result.MissingLFSObjects++
			result.LFSCacheBytes = addBytes(result.LFSCacheBytes, object.Size)
		}
		result.LFSObjects++
		result.LFS = append(result.LFS, *object)
	}
	sort.Slice(result.LFS, func(i, j int) bool { return result.LFS[i].OID < result.LFS[j].OID })
	return result, nil
}

// RefreshLFSCacheState は見積り済み LFS object の filesystem 依存状態だけを
// 読み直す。Git tree や pointer の再解析は行わないため、doctor と準備が共有する
// 見積りを保ったまま外部の git lfs fetch 後にも最新の cache 状態を返せる。
func RefreshLFSCacheState(estimate *CapacityEstimate) error {
	if estimate == nil {
		return nil
	}
	refreshed := make([]LFSObjectInfo, len(estimate.LFS))
	copy(refreshed, estimate.LFS)
	missing := 0
	cacheBytes := int64(0)
	for index := range refreshed {
		object := &refreshed[index]
		if err := refreshLFSObjectCacheState(object); err != nil {
			return err
		}
		if !object.Cached {
			missing++
			cacheBytes = addBytes(cacheBytes, object.Size)
		}
	}
	estimate.LFS = refreshed
	estimate.LFSObjects = len(refreshed)
	estimate.MissingLFSObjects = missing
	estimate.LFSCacheBytes = cacheBytes
	return nil
}

func refreshLFSObjectCacheState(object *LFSObjectInfo) error {
	if object == nil {
		return nil
	}
	cached, size, err := capacityCacheState(object.CachePath)
	if err != nil {
		return fmt.Errorf("inspect LFS cache object %s: %w", object.OID, err)
	}
	object.CacheSize = size
	object.Cached = cached && size == object.Size
	switch {
	case !cached:
		object.CacheState = LFSCacheMissing
	case size != object.Size:
		object.CacheState = LFSCacheCorrupt
	default:
		object.CacheState = LFSCacheHealthy
	}
	return nil
}

func gitBoolTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	default:
		return false
	}
}

func (p *Preparer) capacitySparseSkippedPaths(ctx context.Context, repo discovery.Repository, oid string, sparse bool) (map[string]bool, error) {
	if !sparse {
		return nil, nil
	}
	index, err := os.CreateTemp("", ".wx-capacity-sparse-index-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary sparse capacity index: %w", err)
	}
	indexPath := index.Name()
	if err := index.Close(); err != nil {
		_ = os.Remove(indexPath)
		return nil, fmt.Errorf("close temporary sparse capacity index: %w", err)
	}
	defer func() { _ = os.Remove(indexPath) }()
	worktree, err := os.MkdirTemp("", ".wx-capacity-sparse-worktree-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary sparse capacity worktree: %w", err)
	}
	defer func() { _ = os.RemoveAll(worktree) }()
	env := []string{"GIT_INDEX_FILE=" + indexPath, "GIT_WORK_TREE=" + worktree}
	if _, err := p.capacityGitEnv(ctx, repo, env, nil, "read-tree", "--empty"); err != nil {
		return nil, fmt.Errorf("initialize temporary sparse capacity index: %w", err)
	}
	if _, err := p.capacityGitEnv(ctx, repo, env, nil, "read-tree", "--reset", "-i", oid); err != nil {
		return nil, fmt.Errorf("read requested tree into temporary sparse capacity index: %w", err)
	}
	if _, err := p.capacityGitEnv(ctx, repo, env, nil, "sparse-checkout", "reapply"); err != nil {
		return nil, fmt.Errorf("apply sparse checkout to requested tree for capacity estimate: %w", err)
	}
	listing, err := p.capacityGitEnv(ctx, repo, env, nil, "ls-files", "-v", "-z")
	if err != nil {
		return nil, fmt.Errorf("read sparse checkout paths for capacity estimate: %w", err)
	}
	paths := make(map[string]bool, len(ParseIndexFlags(listing.Stdout).SkipWorktree))
	for _, path := range ParseIndexFlags(listing.Stdout).SkipWorktree {
		paths[path] = true
	}
	return paths, nil
}

// SparseCheckoutEnabled は sparse 選択を再計算する必要があるか判定する。
// 選択内容は sparse-checkout file にあり、bool だけでは inside/outside の変更を区別できない。
// daemon は有効時の容量見積りを cache せず、要求 OID と現在の選択を毎回組み立てる。
func (p *Preparer) SparseCheckoutEnabled(ctx context.Context, repo discovery.Repository) (bool, error) {
	result, err := p.capacityGit(ctx, repo, nil, "config", "--default", "false", "--get", "core.sparseCheckout")
	if err != nil {
		return false, fmt.Errorf("read sparse checkout setting for capacity cache: %w", err)
	}
	return gitBoolTrue(result.Stdout), nil
}

// CapacityEstimate は呼び出し側が既存の名前で検索しやすいよう、短い別名も提供する。
func (p *Preparer) CapacityEstimate(ctx context.Context, repo discovery.Repository, oid string) (CapacityEstimate, error) {
	return p.EstimateCapacity(ctx, repo, oid)
}

func (p *Preparer) capacityGit(ctx context.Context, repo discovery.Repository, input []byte, args ...string) (gitx.Result, error) {
	return p.capacityGitEnv(ctx, repo, nil, input, args...)
}

func (p *Preparer) capacityGitEnv(ctx context.Context, repo discovery.Repository, env []string, input []byte, args ...string) (gitx.Result, error) {
	return p.Git.RunEnvInput(ctx, string(repo.MainPath), env, input, append([]string{"--no-optional-locks"}, args...)...)
}

// capacityAttributes は要求 OID の tree を一時 index へ読み、source index を
// 書き換えずに checkout 属性を読む。CoW 判定だけは呼び出し側が別途 source index
// から行うため、要求 tree と現在の作業状態の属性が混ざらない。
func (p *Preparer) capacityAttributes(ctx context.Context, repo discovery.Repository, oid string, input []byte) (gitx.Result, error) {
	file, err := os.CreateTemp("", ".wx-capacity-index-*")
	if err != nil {
		return gitx.Result{}, fmt.Errorf("create temporary capacity index: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return gitx.Result{}, fmt.Errorf("close temporary capacity index: %w", err)
	}
	defer func() { _ = os.Remove(path) }()
	env := []string{"GIT_INDEX_FILE=" + path}
	if _, err := p.capacityGitEnv(ctx, repo, env, nil, "read-tree", "--empty"); err != nil {
		return gitx.Result{}, fmt.Errorf("initialize temporary capacity index: %w", err)
	}
	if _, err := p.capacityGitEnv(ctx, repo, env, nil, "read-tree", "--reset", "-i", "--no-sparse-checkout", oid); err != nil {
		return gitx.Result{}, fmt.Errorf("read requested tree into temporary capacity index: %w", err)
	}
	return p.capacityGitEnv(ctx, repo, env, input, "check-attr", "--cached", "--all", "--stdin", "-z")
}

func parseCapacityTree(stdout string) ([]capacityTreeEntry, error) {
	entries := make([]capacityTreeEntry, 0)
	for _, record := range strings.Split(stdout, "\x00") {
		if record == "" {
			continue
		}
		header, path, ok := strings.Cut(record, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) < 4 {
			return nil, errors.New("invalid Git tree entry for capacity estimate")
		}
		if fields[1] != "blob" || fields[3] == "-" || fields[0] == "120000" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid Git tree blob size for %s", path)
		}
		if filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("unsafe Git tree path %q", path)
		}
		entries = append(entries, capacityTreeEntry{Mode: fields[0], OID: fields[2], Size: size, Path: path})
	}
	return entries, nil
}

func parseLFSFilterPaths(stdout string) map[string]bool {
	fields := strings.Split(stdout, "\x00")
	paths := map[string]bool{}
	for index := 0; index+2 < len(fields); index += 3 {
		if fields[index+1] == "filter" && fields[index+2] == "lfs" {
			paths[fields[index]] = true
		}
	}
	return paths
}

func (p *Preparer) readCapacityBlobs(ctx context.Context, repo discovery.Repository, oids []string) (map[string]LFSPointer, error) {
	pointers := map[string]LFSPointer{}
	if len(oids) == 0 {
		return pointers, nil
	}
	input := strings.Join(oids, "\n") + "\n"
	result, err := p.capacityGit(ctx, repo, []byte(input), "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read LFS pointer blobs: %w", err)
	}
	return parseLFSPointerBatch(result.Stdout, oids, false)
}

// parseLFSPointerBatch は `cat-file --batch` の blob を pointer として読む。
// missing を許す呼び出しでは、その object だけを pointer 無しとして返す。
func parseLFSPointerBatch(stdout string, oids []string, allowMissing bool) (map[string]LFSPointer, error) {
	pointers := map[string]LFSPointer{}
	reader := bufio.NewReader(strings.NewReader(stdout))
	for _, requested := range oids {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read LFS pointer header for %s: %w", requested, err)
		}
		fields := strings.Fields(strings.TrimSpace(header))
		if allowMissing && len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("invalid LFS pointer header for %s", requested)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid LFS pointer blob size for %s", requested)
		}
		if size > maxLFSPointerBytes {
			if _, err := io.CopyN(io.Discard, reader, size); err != nil {
				return nil, fmt.Errorf("skip oversized LFS pointer blob for %s: %w", requested, err)
			}
			if separator, err := reader.ReadByte(); err != nil || separator != '\n' {
				return nil, fmt.Errorf("invalid LFS pointer blob separator for %s", requested)
			}
			continue
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, fmt.Errorf("read LFS pointer blob for %s: %w", requested, err)
		}
		if separator, err := reader.ReadByte(); err != nil || separator != '\n' {
			return nil, fmt.Errorf("invalid LFS pointer blob separator for %s", requested)
		}
		if pointer, ok := ParseLFSPointer(data); ok {
			pointers[requested] = pointer
		}
	}
	return pointers, nil
}

func (p *Preparer) capacityCOWEnabled(repo discovery.Repository, autocrlf string, convertible, lfs map[string]bool) bool {
	mode := p.Config.CopyModeForWorkspaceRepository(p.workspaceRootForRepository(repo), repo.RelativePath, string(repo.MainPath))
	return capacityCOWEnabledFor(cowAvailable(), mode, autocrlf, convertible, lfs)
}

func capacityCOWEnabledFor(available bool, mode, autocrlf string, convertible, lfs map[string]bool) bool {
	if !available || mode == config.CopyModeCopy {
		return false
	}
	if strings.TrimSpace(autocrlf) != "false" || len(convertible) != 0 || len(lfs) != 0 {
		return false
	}
	return true
}

func (p *Preparer) capacityEarlyPaths(repo discovery.Repository, entries []capacityTreeEntry) map[string]bool {
	workspaceRoot := p.workspaceRootForRepository(repo)
	readiness := p.Config.ReadinessForWorkspaceRepository(workspaceRoot, repo.RelativePath, string(repo.MainPath))
	candidates := append(append([]string{}, defaultEarlyPaths...), readiness.EarlyPaths...)
	early := make(map[string]bool)
	for _, entry := range entries {
		base := filepath.Base(entry.Path)
		if earlyMatch(entry.Path, candidates) || base == ".gitignore" || base == ".gitattributes" {
			early[entry.Path] = true
		}
	}
	return early
}

func (p *Preparer) addRepositoryCopyBytes(repo discovery.Repository, estimate *CapacityEstimate, targetTracked map[string]bool) error {
	source, err := openPinnedRepositoryRoot(string(repo.MainPath))
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	plan := &earlyPlan{}
	if err := p.planIncludesAt(repo, plan, source); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, entry := range plan.copies {
		if entry.directory || seen[entry.path] || targetTracked[entry.path] {
			continue
		}
		seen[entry.path] = true
		info, err := domain.PhysicalPathInfo(source, entry.path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			estimate.WorktreeBytes = addBytes(estimate.WorktreeBytes, info.Size())
		}
	}
	return nil
}

func lfsCachePath(repo discovery.Repository, oid string) string {
	value := strings.TrimPrefix(strings.ToLower(oid), "sha256:")
	if len(value) != 64 {
		return filepath.Join(string(repo.CommonDir), "lfs", "objects")
	}
	// git-lfs は先頭 2 桁・次の 2 桁で directory を分け、leaf には OID 全体を使う。
	return filepath.Join(string(repo.CommonDir), "lfs", "objects", value[:2], value[2:4], value)
}

func capacityCacheState(path string) (bool, int64, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if !info.Mode().IsRegular() {
		return false, 0, nil
	}
	return true, info.Size(), nil
}

func addBytes(current, value int64) int64 {
	if value < 0 || current > int64(^uint64(0)>>1)-value {
		return int64(^uint64(0) >> 1)
	}
	return current + value
}

// EstimateRootCopyBytes は非 Git workspace root の copy rule が source から
// 持ち込む regular file の合計を返す。OptionalCopy の欠落と top-level symlink は
// materialize と同じく無視し、Copy の欠落や配下の symlink は診断不能として返す。
func EstimateRootCopyBytes(sourcePath string, rules RootRules) (int64, error) {
	source, err := OpenPhysicalRoot(sourcePath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = source.Close() }()
	total := int64(0)
	seenRules := map[string]bool{}
	seenDestinations := map[string]bool{}
	for _, item := range rules.Copy {
		clean, err := safeRelative(item)
		if err != nil {
			return 0, err
		}
		item = clean
		if seenRules[item] {
			continue
		}
		seenRules[item] = true
		if skip, err := skipRootCopySymlink(source, item, false); err != nil {
			return 0, err
		} else if skip {
			continue
		}
		var itemErr error
		total, itemErr = addRootCopyPath(source, item, total, seenDestinations)
		if itemErr != nil {
			return 0, itemErr
		}
	}
	for _, item := range rules.OptionalCopy {
		clean, err := safeRelative(item)
		if err != nil {
			return 0, err
		}
		item = clean
		if seenRules[item] {
			continue
		}
		seenRules[item] = true
		if skip, err := skipRootCopySymlink(source, item, true); err != nil {
			return 0, err
		} else if skip {
			continue
		}
		var itemErr error
		total, itemErr = addRootCopyPath(source, item, total, seenDestinations)
		if itemErr != nil {
			return 0, itemErr
		}
	}
	return total, nil
}

func skipRootCopySymlink(source *os.Root, relative string, optional bool) (bool, error) {
	info, err := source.Lstat(relative)
	if errors.Is(err, os.ErrNotExist) && optional {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink != 0, nil
}

func addRootCopyPath(source *os.Root, relative string, total int64, seen map[string]bool) (int64, error) {
	relative = filepath.Clean(relative)
	if relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return 0, fmt.Errorf("unsafe workspace root copy path %q", relative)
	}
	info, err := domain.PhysicalPathInfo(source, relative)
	if err != nil {
		return 0, err
	}
	if info.Mode().IsRegular() {
		if seen[relative] {
			return total, nil
		}
		seen[relative] = true
		return addBytes(total, info.Size()), nil
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("workspace root copy source %s is not a regular file", relative)
	}
	directory, _, err := domain.OpenDirectoryAt(source, relative)
	if err != nil {
		return 0, err
	}
	defer func() { _ = directory.Close() }()
	names, err := directory.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	sort.Strings(names)
	for _, name := range names {
		total, err = addRootCopyPath(source, filepath.Join(relative, name), total, seen)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}
