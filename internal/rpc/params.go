package rpc

// 貸出と復元の要求型。CLI の送信と daemon の decode で同じ宣言を使い、片側だけの改名や型違いを防ぐ。
// 冪等キーと再送判定は Params の JSON 文字列をそのまま比較するため、フィールド宣言順は旧 map[string]any の marshal 結果（辞書順キー）に揃える。
// 同じ理由で omitempty を付けず、null slice・空文字・false・0 も従来どおり出力する。

// ResolveAndLeaseParams は Method "ResolveAndLease" の要求。
type ResolveAndLeaseParams struct {
	Agent         string   `json:"agent"`
	Branches      []string `json:"branches"`
	ClientPID     int      `json:"client_pid"`
	CWD           string   `json:"cwd"`
	ForceWorktree bool     `json:"force_worktree"`
}

// ResumeParams は Method "Resume" の要求。
type ResumeParams struct {
	Agent          string   `json:"agent"`
	AgentSessionID string   `json:"agent_session_id"`
	Branches       []string `json:"branches"`
	ClientPID      int      `json:"client_pid"`
	Fresh          bool     `json:"fresh"`
	WXSessionID    string   `json:"wx_session_id"`
}
