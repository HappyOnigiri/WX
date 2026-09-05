package archive

import (
	"context"
	"fmt"
	"strings"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

// OrphanRef は削除候補の ref と、分類時に読み取った object ID。削除はこの OID を期待値として渡す。
type OrphanRef struct {
	Ref string `json:"ref"`
	OID string `json:"oid"`
}

// UnsafeOrphanRef は、消すと他のどの ref からも到達できなくなる object を持つ候補。
// UnreachableObjects はその object 数で、既定の削除が候補を残す理由になる。
type UnsafeOrphanRef struct {
	OrphanRef
	UnreachableObjects int `json:"unreachable_objects"`
}

// ClassifyOrphanRefs は候補 ref を、消しても到達可能性を失わないものと失うものへ分ける。
// 判定は候補を除く全 ref から到達できる object 集合との差で行い、実在しない候補は黙って落とす。
// commentlint:allow-long -- 判定の基準（reflog ではなく到達可能性）を呼び出し側が誤解しないため
func (m *Manager) ClassifyOrphanRefs(ctx context.Context, repo discovery.Repository, candidates []string) (safe []OrphanRef, unsafe []UnsafeOrphanRef, err error) {
	wanted := map[string]bool{}
	for _, ref := range candidates {
		wanted[ref] = true
	}
	listed, err := m.Git.Run(ctx, string(repo.MainPath), "for-each-ref", "--format=%(refname) %(objectname)")
	if err != nil {
		return nil, nil, err
	}
	var keepOIDs []string
	for _, line := range strings.Split(strings.TrimSpace(listed.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if wanted[fields[0]] {
			safe = append(safe, OrphanRef{Ref: fields[0], OID: fields[1]})
			continue
		}
		keepOIDs = append(keepOIDs, fields[1])
	}
	if len(safe) == 0 {
		return nil, nil, nil
	}
	keep, err := m.reachableObjects(ctx, repo, keepOIDs)
	if err != nil {
		return nil, nil, err
	}
	candidateOIDs := make([]string, 0, len(safe))
	for _, ref := range safe {
		candidateOIDs = append(candidateOIDs, ref.OID)
	}
	exclusive, err := m.unreachableCount(ctx, repo, candidateOIDs, keep)
	if err != nil {
		return nil, nil, err
	}
	if exclusive == 0 {
		return safe, nil, nil
	}
	// 全体では固有 object が残るので、責任のある ref を 1 本ずつ特定する。他の候補は互いに除外しない側（保守的）に倒す。
	all := safe
	safe = nil
	for _, ref := range all {
		count, countErr := m.unreachableCount(ctx, repo, []string{ref.OID}, keep)
		if countErr != nil {
			return nil, nil, countErr
		}
		if count == 0 {
			safe = append(safe, ref)
			continue
		}
		unsafe = append(unsafe, UnsafeOrphanRef{OrphanRef: ref, UnreachableObjects: count})
	}
	return safe, unsafe, nil
}

// reachableObjects は tips から到達できる object ID の集合を返す。
// `--not` は正の起点が tree のとき効かないため、除外は集合差で行う（起点の列挙もこのために明示的に渡す）。
func (m *Manager) reachableObjects(ctx context.Context, repo discovery.Repository, tips []string) (map[string]bool, error) {
	objects := map[string]bool{}
	if len(tips) == 0 {
		return objects, nil
	}
	result, err := m.Git.RunEnvInput(ctx, string(repo.MainPath), nil, []byte(strings.Join(tips, "\n")+"\n"), "rev-list", "--objects", "--no-object-names", "--stdin")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			objects[line] = true
		}
	}
	return objects, nil
}

func (m *Manager) unreachableCount(ctx context.Context, repo discovery.Repository, tips []string, keep map[string]bool) (int, error) {
	objects, err := m.reachableObjects(ctx, repo, tips)
	if err != nil {
		return 0, err
	}
	count := 0
	for object := range objects {
		if !keep[object] {
			count++
		}
	}
	return count, nil
}

// DeleteOrphanRefs は候補をまとめて削除する。期待 OID を添えるので、分類後に ref が動いていれば Git 側が batch ごと拒否する。
func (m *Manager) DeleteOrphanRefs(ctx context.Context, repo discovery.Repository, refs []OrphanRef) error {
	if len(refs) == 0 {
		return nil
	}
	var input strings.Builder
	for _, ref := range refs {
		if _, err := m.Git.Run(ctx, string(repo.MainPath), "check-ref-format", ref.Ref); err != nil {
			return fmt.Errorf("invalid recovery ref %q: %w", ref.Ref, err)
		}
		fmt.Fprintf(&input, "delete %s %s\n", ref.Ref, ref.OID)
	}
	return m.Git.WithCommonDirLock(string(repo.CommonDir), func() error {
		_, err := m.Git.RunEnvInput(ctx, string(repo.MainPath), nil, []byte(input.String()), "update-ref", "--stdin")
		return err
	})
}
