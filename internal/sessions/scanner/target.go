package scanner

// ResumeTarget はシェルを介さず再開先を受け渡すための会話情報である。
type ResumeTarget struct {
	Tool, Kind, SessionID, CWD, StableID, SourcePath string
	Mtime                                            float64
}

// Resumable は wx が構造化した再開起動へ渡せる対象かを返す。
func (t ResumeTarget) Resumable() bool {
	return t.Kind == "session" && (t.Tool == "claude" || t.Tool == "codex") && t.SessionID != ""
}

// Target はセッションの再開先表現を返す。
func (s Session) Target() ResumeTarget {
	return ResumeTarget{
		Tool:       s.Tool,
		Kind:       "session",
		SessionID:  s.SessionID,
		CWD:        s.CWD,
		StableID:   s.StableID,
		SourcePath: s.RawPath,
		Mtime:      s.Mtime,
	}
}
