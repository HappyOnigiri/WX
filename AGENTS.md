# AGENTS.md

`wx`は、Claude CodeとCodexをdaemon管理のdetached worktreeで起動するGo製CLI + daemonである。
機能と運用の複雑さは単一ユーザー・単一マシンを前提に判断する。
実行対象はmacOSのみ（状態・socketは`~/Library`配下、常駐はLaunchAgent）で、linuxは退行検出用のビルド・テスト対象とする。
CIのランナーは全てlinuxなので、platform依存の実装を触ったら[部分検証](docs/worktree-copy.md#部分検証)の手順を手元で行う。

## 不変条件

- エージェントはwxが作ったworktreeで作業し、ソースリポジトリのHEAD・index・追跡ファイルを変更しない。
- 正常に終了したslotについて、スナップショットしていない作業を自動で破棄しない。
  ユーザーが明示的に実行するコマンドでの削除経路は用意してよい。
- Gitは必ず`internal/gitx`経由で起動する。
  継承した`GIT_DIR`・`GIT_WORK_TREE`・`GIT_INDEX_FILE`などが漏れると、別リポジトリへの操作が成功し、未捕捉のworktreeを削除し得る。
- slotの削除権限は`slots`に登録されたroot世代とroot相対pathだけで決め、inode・marker・Git lock・HEAD・workspace紐付けの不一致を拒否の理由にしない。
  登録外の実体は診断だけを行い、自動でslotとして採用しない。
- 破壊的操作はpinしたroot descriptorの配下に閉じ、descriptorが無い場合にパス名で代替しない。
  root自身より上の祖先成分は検査しない。
  単一ユーザー・単一マシンでは祖先を差し替える相手がおらず、symlink配下にworktree rootやソースリポジトリを置けるようにするためである。
- `storage.worktree_root`を変更しても、既存slotは登録済みのroot世代で寿命を全うする（移動・STALE化しない）。
- 貸出中のslotのworktreeを書き換えない。
  READYのHot Standbyは`worktree.reuse_standby`が有効で更新適合条件を満たす場合に限り、貸出予約後・起動前に要求OIDへ更新してよい。
  更新の書込み開始後に失敗・中断したslotは隔離し、自動再実行や別pathへの切替を行わない。
  更新適合条件を満たさないREADY standbyはSTALEとして回収し、補充で作り直す（`--branch`指定の貸出では回収しない）。

## 状態とスキーマ

slot・session・jobの状態は`internal/state.Store`を唯一の権威とし、遷移はSQLの`state IN (...)`と`RowsAffected()==1`によるcompare-and-swapで検証する。
Go側に状態のenum型や遷移ガードを作らない。
隔離は`slots.state='QUARANTINED'`と`quarantined_artifacts`テーブルで表し、専用ディレクトリを作らない。

スキーマ変更は`migrations/*.sql`の追加と`state.SchemaVersion`の更新で行う。
既存ファイルの編集は適用済みDBに反映されない。
旧worktreeレイアウトのDBとの互換・移行コードは持たず、該当する`state.db`は作り直す。
`state.JSONSchemaVersion`はDB版と独立に、`--json`の出力形状が変わったときだけ上げる。

## 開発

- 編集途中の短い確認には、Go編集時の構造検査を集めた`make check-fast`と、対象を絞る`make test-focus PKG=<パッケージ> RUN=<正規表現>`を使う。
- `core.hooksPath`のlocal設定はuserレベルのhook dispatcherを覆い隠すため設定しない。
- 機械的に判定できる規約は`tools/check*`の検査として実装し、`make ci`へ接続する。
  このAGENTS.mdやコメントでの指示は、静的に判定できない規約に限った最終手段とする。
- 設計と無関係な行を踏むだけのテストでカバレッジの数字を作らず、プロセスやOSのアダプタは`coverage-exclusions.txt`で除外する。
- `internal/daemon`のトップレベルテストは、専用の一時ディレクトリ・DB・Managerだけを使うものに`t.Parallel()`を付ける。
  `t.Setenv`を自身かサブテストで呼ぶテスト、プロセス全体のgoroutine・fdを数えるテスト、短い待機に依存するテストは直列のまま残す。
- READMEは紹介・導入を中心としたコンパクトな記載に留め、機能追加では追記せず、不整合が起きたときだけ修正する。

## コメント

- 保守に必要な意図・制約・契約を書く。
  呼び出し側に必要な動作・戻り値・前提条件は説明し、処理や名前の自明な言い換えは省く。
  経緯やファイル間の関係は、現在の制約や誤解の防止に必要なものを残す。
- 手書きの説明コメントは日本語で書き、識別子・技術用語・参照URL・機械向け指示は原文を維持する。
- 長さは`make comments-check`が検査する。
  上限に合わせた詰め込みや不自然な分割はせず、必要な長文は同じコメント群の独立行に `commentlint:allow-long -- 理由` を置く（Goでは末尾へ置き、先頭のdoc commentを維持する）。

## 作業別ドキュメント

`docs/`はコードを読む前に方針・経緯・設計を掴むための文書で、短時間で読み切れる分量を保つ。
書くのは、設計全体に関わる判断、コードから読めない意図や理由、複数ファイルを横断しないと分からない関係だけとする。
該当する1ファイルを読めば分かること、`--help`やコマンド出力で読めること、周期や既定値などの具体値（実装との不整合の原因になる）は書かない。

変更・調査する観点に対応する文書だけを読む。

- パッケージの責務・依存境界を変えるとき: [責務境界](docs/architecture.md)
- 起動・hook・返却・snapshot・resumeを扱うとき: [セッションと復元](docs/session-lifecycle.md)
- CoW・include・linkの準備処理を扱うとき: [worktreeのコピーとリンク](docs/worktree-copy.md)
- slots/statusの容量・コピー方式の計測を扱うとき: [使用量とCoWの観測](docs/storage-usage.md)
- ジョブ・補充・clear・GC・障害回復を扱うとき: [daemonの補充と回収](docs/daemon-maintenance.md)
- `wx doctor`の診断・`wx bench`の計測・daemonのrestart/stopを扱うとき: [daemonの診断と再起動](docs/daemon-diagnostics.md)
- 削除・上書き・所有権検証を扱うとき: [所有権証明](docs/ownership.md)
- path・命名・root世代・workspaceのtarを扱うとき: [ディスク配置とroot世代](docs/storage-layout.md)
- バージョンの埋め込み・リリースのworkflowを扱うとき: [バージョンとリリース](docs/release.md)
