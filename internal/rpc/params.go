package rpc

// 貸出と復元の要求型。CLI と daemon で共有し、片側だけの改名や型違いを防ぐ。
// Params の JSON 文字列が冪等キーなので、宣言順と既存のゼロ値は従来の payload を保つ。
// 新しい任意フィールドの ForceCold と Language だけは、旧クライアントと同じになるゼロ値を省略する。

// LeaseKind 以下の3フィールドは agent 起動以外への貸出（wx shell / wx run / wx new）を表す。
// LeaseKind が空なら従来の agent 起動である。LeaseOwner* は wx new を呼んだ親 session の
// WX_SESSION_ID / WX_SESSION_TOKEN で、daemon は既存の session token 検証を通してから記録する。

// PrepareCopyMode と PrepareCOWMinSizeKiB は、この貸出で準備する slot にだけ適用する設定の上書きである。
// 空文字と null は上書きなしを表し、daemon の実効設定と設定ファイルはどちらも変更しない。
// cow_min_size_kib は 0 が下限なしを意味するため、未指定と区別できるよう pointer で送る。

// ResolveAndLeaseParams は Method "ResolveAndLease" の要求。
type ResolveAndLeaseParams struct {
	Agent                string   `json:"agent"`
	Branches             []string `json:"branches"`
	ClientPID            int      `json:"client_pid"`
	CWD                  string   `json:"cwd"`
	ForceCold            bool     `json:"force_cold,omitempty"`
	ForceWorktree        bool     `json:"force_worktree"`
	LeaseKind            string   `json:"lease_kind"`
	LeaseOwnerSessionID  string   `json:"lease_owner_session_id"`
	LeaseOwnerToken      string   `json:"lease_owner_token"`
	PrepareCopyMode      string   `json:"prepare_copy_mode"`
	PrepareCOWMinSizeKiB *int     `json:"prepare_cow_min_size_kib"`
	Language             string   `json:"language,omitempty"`
}

// ResumeParams は Method "Resume" の要求。
type ResumeParams struct {
	Agent               string   `json:"agent"`
	AgentSessionID      string   `json:"agent_session_id"`
	Branches            []string `json:"branches"`
	ClientPID           int      `json:"client_pid"`
	Fresh               bool     `json:"fresh"`
	LeaseKind           string   `json:"lease_kind"`
	LeaseOwnerSessionID string   `json:"lease_owner_session_id"`
	LeaseOwnerToken     string   `json:"lease_owner_token"`
	WXSessionID         string   `json:"wx_session_id"`
	Language            string   `json:"language,omitempty"`
}

// UpdateStatusParams は Method "UpdateStatus" の要求。読み取りだけの method なので冪等キーには使わず、
// フィールド宣言順の縛りも効かない。表示文は CLI と TUI が組み立てるため language は持たない。
// DisallowUnknownFields の decode を旧 daemon でも壊さないよう、後からの field 追加は行わない。
type UpdateStatusParams struct {
	// ClaimAnnouncement は、この呼び出しがその版の案内権を要求するかどうかである。
	// 対話起動の CLI だけが true で呼び、TUI と wx update は常時表示・明示実行なので false で呼ぶ。
	ClaimAnnouncement bool `json:"claim_announcement"`
}
