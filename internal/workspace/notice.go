package workspace

import "sync"

// PrepareNotice は準備が成功したまま出力だけを残した区間の記録である。
// exit 0 の hook が内部の失敗を飲み込んでも wx から見えなくならないよう、出力の本文をそのまま運ぶ。
type PrepareNotice struct {
	// Target は出力を出した worktree の path である。
	Target string
	// Phase は timePhase と同じ区間名で、どの処理の出力かを表す。
	Phase string
	// Stdout と Stderr は捨てずに運ぶ生の出力である。改行や整形は行わない。
	Stdout, Stderr string
}

// PrepareNotices は準備 1 回分の notice を、記録された順で集める器である。
// 記録は準備結果を変えないため、nil のままでも同じ worktree ができ、記録だけが落ちる。
// 準備は repository ごとに並列化され得るので、追加と読み出しは mutex の下でだけ行う。
type PrepareNotices struct {
	mu    sync.Mutex
	items []PrepareNotice
}

// Add は出力を 1 件加える。stdout と stderr が両方空の呼び出しは記録しない。
func (n *PrepareNotices) Add(notice PrepareNotice) {
	if n == nil || notice.Stdout == "" && notice.Stderr == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.items = append(n.items, notice)
}

// Notices は記録された順で notice を返す。
func (n *PrepareNotices) Notices() []PrepareNotice {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]PrepareNotice, len(n.items))
	copy(out, n.items)
	return out
}
