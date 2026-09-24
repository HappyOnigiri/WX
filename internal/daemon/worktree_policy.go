package daemon

import (
	"context"
	"path/filepath"

	"github.com/HappyOnigiri/WorktreeX/internal/discovery"
)

// WorktreePolicyReply は cwd に適用される worktree の方針である。
// Resolved が false のときは workspace を決められなかったことを表し、Root と Mode は空になる。
type WorktreePolicyReply struct {
	Root     string `json:"root"`
	Mode     string `json:"mode"`
	Resolved bool   `json:"resolved"`
}

// WorktreePolicy は cwd が属する workspace root と、そこに適用される worktree policy を返す。
// 会話に記録された cwd で worktree の可否を決める resume 経路のためにあり、
// 畳まれた slot の path は登録済みの workspace root へ読み替える。client 側では DB を引けず、この読み替えができない。
// 解決できない cwd は失敗にせず Resolved=false で返す。呼び出し側は worktree 無しで会話を再開する。
// commentlint:allow-long -- client 側に置けない理由と、失敗を返さない理由を残す
func (m *Manager) WorktreePolicy(ctx context.Context, cwd string) WorktreePolicyReply {
	discoverer := discovery.Discoverer{Git: m.git, Config: m.Config()}
	root, err := discoverer.PolicyRoot(ctx, cwd)
	if err != nil {
		retired, lookupErr := m.store.WorkspaceRootForSlotPath(ctx, filepath.Clean(cwd))
		if lookupErr != nil || retired == "" {
			return WorktreePolicyReply{}
		}
		if root, err = discoverer.PolicyRoot(ctx, retired); err != nil {
			return WorktreePolicyReply{}
		}
	}
	return WorktreePolicyReply{Root: root, Mode: m.Config().WorktreeMode(root), Resolved: true}
}
