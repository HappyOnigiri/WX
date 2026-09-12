package dashboard

// Action は TUI で確定した既存 CLI 操作である。
// WorkDir は起動・貸出・bench が対象にする明示的な作業元で、process の cwd は変更しない。
type Action struct {
	Args    []string
	WorkDir string
}

type menuItem struct {
	label       string
	description string
	impact      string
	command     string
	defaultArgs []string
	inputLabel  string
	inputNeeded bool
	workDir     bool
	destructive bool
}

var tabNames = []string{"ステータス", "起動・再開", "設定", "Doctor", "メンテナンス", "セットアップ・Daemon"}

var tabMenus = map[int][]menuItem{
	1: {
		{label: "Claude を起動", description: "選択した workspace で Claude を起動します。", impact: "worktree 方針と準備完了条件を既存 CLI と同じように適用します。", command: "claude", workDir: true},
		{label: "Codex を起動", description: "選択した workspace で Codex を起動します。", impact: "端末制御を Codex へ渡し、終了後に貸出を返却します。", command: "codex", workDir: true},
		{label: "会話を再開", description: "wx session ID を指定して保存済みの作業を復元します。", impact: "復元不能時も無断で fresh 起動へ切り替えません。", command: "resume", inputLabel: "wx session ID", inputNeeded: true},
		{label: "shell を開く", description: "選択した workspace の貸出 worktree で shell を開きます。", impact: "shell の終了後に未完了作業を保存して返却します。", command: "shell", workDir: true},
		{label: "command を実行", description: "貸出 worktree で1個の command と引数を実行します。", impact: "実行ファイルと引数は空白区切りで構造化して渡します。", command: "run", inputLabel: "command と引数", inputNeeded: true, workDir: true},
		{label: "新しい貸出を作る", description: "選択した workspace の path 貸出を作り、場所と session ID を表示します。", impact: "画面を閉じても自動返却せず、親 session・release・TTL のいずれかで返却します。", command: "new", workDir: true},
	},
	3: {
		{label: "通常診断", description: "設定、daemon、DB、slot の静的な診断結果を表示します。", impact: "daemon が不在でもローカルで確認できる事実を返します。", command: "doctor"},
		{label: "詳細診断", description: "正常項目と追加情報を含む詳細な診断結果を表示します。", impact: "状態は変更せず、通常診断より多くの情報を表示します。", command: "doctor", defaultArgs: []string{"--verbose"}},
		{label: "実地 probe", description: "登録 workspace ごとに worktree を準備して実地検査します。", impact: "standby の再作成を伴うため、実行を確定してから開始します。", command: "doctor", defaultArgs: []string{"--probe"}},
	},
	4: {
		{label: "GC", description: "保持期間を過ぎた管理対象を回収します。", impact: "最初に dry-run を選べます。実行時も daemon が対象を再判定します。", command: "gc", inputLabel: "追加引数（例: --dry-run）"},
		{label: "clear", description: "session と standby の削除を要求します。", impact: "未保存作業を破棄する --discard は明示した場合だけ使います。", command: "clear", inputLabel: "追加引数（推奨: --dry-run）", destructive: true},
		{label: "prune", description: "登録されている repository 情報を整理します。", impact: "対象は実行時に再判定されます。", command: "prune", inputLabel: "追加引数（例: --dry-run）"},
		{label: "standby を再試行", description: "停止した待機枠の補充を workspace 単位で再開します。", impact: "隔離済み slot は変更しません。", command: "retry-standby", inputLabel: "workspace path または --all", inputNeeded: true},
		{label: "貸出を返却", description: "wx new などで作った貸出を明示的に返却します。", impact: "通常は snapshot を作ってから返却します。", command: "release", inputLabel: "wx session ID", inputNeeded: true},
		{label: "復旧情報を破棄", description: "指定 workspace の復旧 snapshot を破棄します。", impact: "元の作業状態へ戻せなくなる破壊的操作です。", command: "discard-recovery", inputLabel: "workspace path", inputNeeded: true, destructive: true},
		{label: "workspace を忘れる", description: "登録済み workspace を管理対象から外します。", impact: "実体の削除条件は daemon の所有権規則を維持します。", command: "forget", inputLabel: "workspace path", inputNeeded: true, destructive: true},
		{label: "bench", description: "選択した workspace の準備時間と使用量を測定します。", impact: "既定では standby を退役させて cold start を測ります。", command: "bench", inputLabel: "追加引数（例: --runs 3 --reuse）", workDir: true},
	},
	5: {
		{label: "セットアップを確認", description: "hook と agent 設定の現在値、適用予定を確認します。", impact: "設定は変更しません。", command: "setup", defaultArgs: []string{"--check"}},
		{label: "セットアップを適用", description: "管理対象の hook と agent 設定を対話的に適用します。", impact: "wx が所有しない設定は維持します。", command: "setup"},
		{label: "セットアップを削除", description: "wx が管理する設定だけを削除します。", impact: "他者が管理する hook 設定は残します。", command: "setup", defaultArgs: []string{"--remove"}, destructive: true},
		{label: "daemon を起動", description: "LaunchAgent の daemon を起動して応答を待ちます。", impact: "要求受付と起動完了を区別します。", command: "daemon", defaultArgs: []string{"start"}},
		{label: "daemon を停止", description: "daemon に安全な停止を要求して完了を待ちます。", impact: "進行中の処理は既存の停止条件に従います。", command: "daemon", defaultArgs: []string{"stop"}},
		{label: "daemon を再起動", description: "daemon を停止してから新しい process の応答を待ちます。", impact: "古い binary が動いている場合の更新にも使えます。", command: "daemon", defaultArgs: []string{"restart"}},
		{label: "LaunchAgent を install", description: "現在の wx binary を使う LaunchAgent を配置します。", impact: "既存 plist が古い場合は内容を更新します。", command: "daemon", defaultArgs: []string{"install"}},
		{label: "LaunchAgent を uninstall", description: "wx の LaunchAgent 登録を削除します。", impact: "daemon は自動起動しなくなります。", command: "daemon", defaultArgs: []string{"uninstall"}, destructive: true},
	},
}
