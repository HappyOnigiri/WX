# 責務境界

依存境界の検査は[`.golangci.yml`](../.golangci.yml)のdepguardを参照する。
`internal/state`は`internal/discovery`のworkspace型を受け取るため、厳密な一方向の層構造ではない。

| 責務 | 実装の入口 |
| --- | --- |
| 引数解析・RPC・子プロセス起動と信号中継 | `cmd/wx`、`internal/cli` |
| hook実行・同期的な準備完了契約の判定・agent hook設定のwxエントリの書き込み | `internal/agent`、`internal/hookconfig` |
| セットアップ項目の収集と適用 | `internal/setup` |
| 会話選択 | `internal/sessions` |
| Unix socket通信・冪等キーによる重複抑止・要求契約型 | `internal/rpc` |
| 貸出・返却・永続ジョブ・周期処理 | `internal/daemon` |
| worktree準備・削除、snapshot・復元、branch解決 | `internal/workspace`、`internal/archive`、`internal/pool` |
| SQLite・所有権証明 | `internal/state`、`migrations` |
| workspace・リポジトリ解決 | `internal/discovery` |
| path・ID・descriptor、Git実行、fchdir束縛、設定、LaunchAgent | `internal/domain`、`internal/gitx`、`internal/fdexec`、`internal/config`、`internal/launchd` |
| 複数の画面で規則を共有する表示整形（バイト数・ホーム短縮） | `internal/textfmt` |
| 対話的な選択UI | `internal/tui` |

`ResolveAndLease`・`Resume`の要求は`internal/rpc/params.go`の共有structで表し、片側だけの改名や型違いを防ぐためCLIの送信とdaemonのstrict decodeが同じ宣言を使う。
冪等キーがJSON文字列の一致で判定される都合上、この型のJSON出力形状には同ファイルのコメントが記す制約がある。

会話選択は実行時にClaude・Codexの履歴を読む範囲とし、本文表示・全文検索・履歴DB・daemonでの履歴更新は持たない。
唯一の例外は`internal/sessions/metacache`の再生成可能なキャッシュで、未変更JSONLの再解析を省くだけで権威はJSONLのまま、daemonのstate.dbと独立なので`migrations`にも`state.SchemaVersion`にも関わらない。

statusの組み立ては`internal/daemon/status.go`に置き、`internal/diag`はdaemon接続なしで成立する診断とfindingの表示・終了コードだけを持つ。
`diag.Check*`の名前空間は2パッケージで分担し、状態を読む検査はdaemon側に持つ。

`wx`バイナリはCLI・daemon・`internal/fdexec`のexecトランポリンを兼ね、descriptor束縛でGitやエージェントを起動する経路は自分自身を再execする。

help本文・config schema・SQLite migration・LaunchAgent plist・agent hook設定は手書きで維持し、shell completionは実装しない。
agent hook設定のうちwxが所有・書き換えるのはwxエントリだけで、他者のエントリはそのまま残す。
ただし準備完了契約の判定は同じファイルの他エントリに影響される。
