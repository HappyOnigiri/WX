package archive

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// 条件付き除外は、workspace root の link rule が「どの path が link になり得るか」を示すだけの候補集合であり、
// 除外するかどうかは rule ではなく実体と archive が決める。
// rule を読み直して除外を決めると、貸出中に rule を変えただけで slot 内の実体が tar から落ちて作業が消える。
// snapshot 側は「slot 内で symlink だったか」、復元側は「archive にその path があるか」を見る。
// snapshot が除外しなかった候補は必ず tar に入るので、両者の判定は一致し、
// 「tar に入れない / prune で消さない / 復元で来たら異常」という 3 重契約が崩れない。
// commentlint:allow-long -- 条件付き除外の authority がなぜ rule でないかを、両側の判定が一致する理由まで含めて 1 箇所で説明する

// resolveBundleLinkExclusions は snapshot 側の条件付き除外を、bundle 内の実体から決める。
// symlink の候補だけを返し、実体（regular file・directory）の候補は除外せず tar へ入れる。
func resolveBundleLinkExclusions(bundle *os.Root, candidates []string) ([]string, error) {
	if bundle == nil {
		return nil, errors.New("workspace bundle root descriptor is nil")
	}
	resolved := make([]string, 0, len(candidates))
	for _, candidate := range normalizeExclusionCandidates(candidates) {
		// 祖先が既に除外（symlink）と確定した候補は、lstat が os.Root に弾かれるため判定せずに飛ばす。
		if workspacePathExcluded(candidate, resolved) {
			continue
		}
		info, err := bundle.Lstat(filepath.FromSlash(candidate))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved = append(resolved, candidate)
		}
	}
	return resolved, nil
}

// resolveArchiveLinkExclusions は復元側の条件付き除外を、archive が持つ path から決める。
// archive に entry が 1 件も無い候補だけを返す。snapshot 側で symlink として除外された path がこれに当たり、
// prune の対象から外すことで materialize 済みの link をそのまま残す。file は非圧縮 tar の先頭へ巻き戻して返す。
func resolveArchiveLinkExclusions(file *os.File, candidates []string) ([]string, error) {
	if file == nil {
		return nil, errors.New("workspace archive descriptor is nil")
	}
	normalized := normalizeExclusionCandidates(candidates)
	if len(normalized) == 0 {
		return nil, nil
	}
	present, err := archivePathsWithAncestors(file)
	if err != nil {
		return nil, err
	}
	resolved := make([]string, 0, len(normalized))
	for _, candidate := range normalized {
		if workspacePathExcluded(candidate, resolved) {
			continue
		}
		if !present[candidate] {
			resolved = append(resolved, candidate)
		}
	}
	return resolved, nil
}

// archivePathsWithAncestors は tar header だけを 1 周読み、entry の path とその祖先を集めて offset を先頭へ戻す。
// 祖先も入れるのは、候補 a/b に対し archive が a/b/c しか持たない場合も「存在する」と判定するためである。
func archivePathsWithAncestors(file *os.File) (map[string]bool, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		rel, relErr := archiveRelative(filepath.ToSlash(header.Name))
		if relErr != nil {
			continue
		}
		for current := rel; current != "."; current = path.Dir(current) {
			if present[current] {
				break
			}
			present[current] = true
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return present, nil
}

// normalizeExclusionCandidates は候補を正規化し、重複を除いて祖先が先に来る順へ並べる。
// 不正な path は静かに落とす。候補は利用者が link rule へ書いた値がそのまま来るため、
// 値が不正なだけで release を失敗させると、隔離を経て作業が失われる経路を作ってしまう。
// bundle 外を指す値は walk 対象でも archive の key でもないので、落としても除外の判定は変わらない。
// commentlint:allow-long -- 不正な候補を失敗ではなく黙って落とす理由と、それが安全である根拠を残す
func normalizeExclusionCandidates(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		rel, err := archiveRelative(filepath.ToSlash(value))
		if err != nil || seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
