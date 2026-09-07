# 責務境界

依存境界の検査は[`.golangci.yml`](../.golangci.yml)のdepguardを参照する。
`internal/state`は`internal/discovery`のworkspace型を受け取るため、厳密な一方向の層構造ではない。

| 責務 | 実装の入口 |
| --- | --- |
| 引数解析・RPC・子プロセス起動と信号中継 | `cmd/wx`、`internal/cli` |
| hook実行・同期的な準備完了契約の判定 | `internal/agent`、`internal/hookconfig` |
| 会話選択 | `internal/sessions` |
| Unix socket通信・冪等キーによる重複抑止・要求契約型 | `internal/rpc` |
| 貸出・返却・永続ジョブ・周期処理 | `internal/daemon` |
| worktree準備・削除、snapshot・復元、branch解決 | `internal/workspace`、`internal/archive`、`internal/pool` |
| SQLite・所有権証明 | `internal/state`、`migrations` |
| workspace・リポジトリ解決 | `internal/discovery` |
| path・ID・descriptor、Git実行、fchdir束縛、設定、LaunchAgent | `internal/domain`、`internal/gitx`、`internal/fdexec`、`internal/config`、`internal/launchd` |

`ResolveAndLease`・`Resume`の要求は`internal/rpc/params.go`の共有structで表し、CLIの送信とdaemonのstrict decodeが同じ宣言を使う。
冪等キーはParamsのJSON文字列をそのまま比較するため、共有structはフィールドの宣言順（旧map実装の辞書順キー）とomitempty無しの出力を維持する。

会話選択は実行時にClaude・Codexの履歴からタイトル・会話ID・cwdを読む範囲とし、本文表示・全文検索・履歴DB・daemonでの履歴更新は持たない。
唯一の例外は`internal/sessions/metacache`が持つ再生成可能なメタデータキャッシュで、ユーザーのcache directory内のSQLiteにタイトル・会話ID・cwdと除外結果だけを保存する。
ディレクトリ列挙とファイル属性の確認は呼び出しごとに行い、属性が一致した未変更JSONLの再解析だけを省くため、権威はJSONLのままである。
破損・書き込み不能・schema差異では直接走査へ戻す。このキャッシュはdaemonのstate.dbと独立で、`migrations`にも`state.SchemaVersion`にも関わらない。
status/doctorの組み立ては`internal/daemon/status.go`に置き、診断用の独立パッケージは作らない。

`wx`バイナリはCLI・daemon・`internal/fdexec`のexecトランポリン（`__wx_exec_at_fd`）を兼ねる。
descriptor束縛でGitやエージェントを起動する経路は自分自身を再execする。

help本文・config schema・SQLite migration・LaunchAgent plistは手書きで維持し、shell completionは実装しない。
生成物検査の扱いは[`Makefile`](../Makefile)の`generated-check`を参照する。
