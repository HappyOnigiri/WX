package rpc

// 貸出と復元の要求型。CLI の送信と daemon の decode で同じ宣言を使い、片側だけの改名や型違いを防ぐ。
// 冪等キーと再送判定は Params の JSON 文字列をそのまま比較するため、フィールド宣言順は旧 map[string]any の marshal 結果（辞書順キー）に揃える。
// 同じ理由で omitempty を付けず、null slice・空文字・false・0 も従来どおり出力する。

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
	ForceWorktree        bool     `json:"force_worktree"`
	LeaseKind            string   `json:"lease_kind"`
	LeaseOwnerSessionID  string   `json:"lease_owner_session_id"`
	LeaseOwnerToken      string   `json:"lease_owner_token"`
	PrepareCopyMode      string   `json:"prepare_copy_mode"`
	PrepareCOWMinSizeKiB *int     `json:"prepare_cow_min_size_kib"`
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
}
