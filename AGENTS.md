# AGENTS.md

`wx`は、Claude CodeとCodexをdaemon管理のdetached worktreeで起動するGo製CLI + daemonである。
機能と運用の複雑さは単一ユーザー・単一マシンを前提に判断する。
実行対象はmacOSのみ（状態・socketは`~/Library`配下、常駐はLaunchAgent）で、linuxは退行検出用のビルド・テスト対象とする。
CIのランナーは全てlinuxで、darwin専用実装は`make build-darwin`のクロスコンパイルでしか検査されない。
これらに触ったら手元の`make ci`で実行を確かめる。

## 不変条件

- エージェントはwxが作ったworktreeで作業し、ソースリポジトリのHEAD・index・追跡ファイルを変更しない。
- 正常に終了したslotについて、スナップショットしていない作業を自動で破棄しない（ユーザーが明示的に実行するコマンドでの削除経路は用意してよい）。
  異常終了・不整合な状態で終わったslot（`QUARANTINED`）はこの対象外とし、GCが`retention.quarantined`の経過後に削除する。
  `wx clear`はこの経過を待たずに削除し、`--discard`指定時は正常終了slotの未保存作業も破棄できる。
  `internal/archive`のclean判定では、Git設定で隠れる変更を見逃さないよう`--untracked-files=all`・`--ignore-submodules=none`を維持する。
- Gitは必ず`internal/gitx`経由で起動する。
  継承した`GIT_DIR`・`GIT_WORK_TREE`・`GIT_INDEX_FILE`などが漏れると、別リポジトリへの操作が成功し、未捕捉のworktreeを削除し得る。
- slot の削除権限は `slots` に登録された root と相対 path で決める。
  inode・marker・Git lock・HEAD・workspace 紐付けの不一致は削除を拒否する理由にしない。
  登録 path の実体が置き換わっていても回収する。
  登録外の実体は診断だけを行い、自動で slot として採用しない。
  削除は pin 済み root 内に閉じ、root から leaf の親までの symlink を辿らない。
  leaf symlink はリンク自体を削除する。
- 準備・復元ではrootのpin（`os.Root`・`domain.OpenOwnedRoot`）と配下のsymlink拒否（`domain.PhysicalPathInfo`）を使う。
  子プロセスのCWDは`internal/fdexec`でdescriptorへ束縛する。
  root自身より上の祖先成分は検査しない（`domain.ValidatePhysicalLeaf`はleafだけを見る）。
  単一ユーザー・単一マシンでは祖先を差し替える相手がおらず、symlink配下にworktree rootやソースリポジトリを置けるようにするためである。
  descriptorがない場合にパス名で代替しない。
- slotの位置は`roots.id` + root相対pathで表し、削除対象もその登録範囲に限る。
  `storage.worktree_root`変更後も既存slotは旧rootで寿命を全うする（移動・STALE化しない）。
- 貸出中のslotのworktreeを書き換えない。
  `--branch`指定やmain更新でOIDが一致しないときは、cold startで作り直す。

## 状態とスキーマ

slot・session・jobの状態は`internal/state.Store`を唯一の権威とし、遷移はSQLの`state IN (...)`と`RowsAffected()==1`によるcompare-and-swapで検証する。
Go側に状態のenum型や遷移ガードを作らない。
隔離は`slots.state='QUARANTINED'`と`quarantined_artifacts`テーブルで表し、専用ディレクトリを作らない。

スキーマ変更は`migrations/*.sql`に次の番号のファイルを追加する（既存ファイルの編集は適用済みDBに反映されない）。
`state.SchemaVersion`はファイル数に手動で揃える（テスト・CIによる一致検査はない）。
旧worktreeレイアウトのDBとの互換・移行コードは持たず、該当する`state.db`は作り直す。
`state.JSONSchemaVersion`はDB版と独立に、`--json`の出力形状が変わったときだけ上げる。

## 開発

- 変更後のゲートは`make ci`で通す。
  Git hookは書式・`go vet`・buildだけを見るため、lint・テスト・カバレッジは手元で走らせない限り検査されない。
  `core.hooksPath`のlocal設定はuserレベルのhook dispatcherを覆い隠すため設定しない。
- 機械的に判定できる規約は`tools/check*`の検査として実装し、`make ci`へ接続する。
  このAGENTS.mdやコメントでの指示は、静的に判定できない規約に限った最終手段とする。
- coreパッケージのカバレッジ基準は`tools/checkcoverage`を参照する。
  設計と無関係な行を踏むだけのテストで数字を作らず、プロセスやOSのアダプタは`coverage-exclusions.txt`に理由付きで除外する。
- platform依存のコードを触ったら`CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`も手元で通す。
- `internal/daemon`のトップレベルテストは、専用の一時ディレクトリ・DB・Managerだけを使うものに`t.Parallel()`を付ける。
  `t.Setenv`を自身かサブテストで呼ぶテスト、プロセス全体のgoroutine・fdを数えるテスト、短い待機に依存するテストは直列のまま残す。
- Markdown文書は1文1行とし、表示幅200桁を超える文は分割する（全角文字は2桁）。
- READMEは紹介・導入を中心としたコンパクトな記載に留め、機能追加では追記せず、不整合が起きたときだけ修正する。

## コメント

- 保守に必要な意図・制約・契約を書く。
  呼び出し側に必要な動作・戻り値・前提条件は説明し、処理や名前の自明な言い換えは省く。
  経緯やファイル間の関係は、現在の制約や誤解の防止に必要なものを残す。
- 手書きの説明コメントは日本語で書き、識別子・技術用語・参照URL・機械向け指示は原文を維持する。
- コメント群は原則3行以内、本文の表示幅は1行200桁以内とする。
  行数に合わせた詰め込みや不自然な分割を避け、必要な長文は同じコメント群の独立行に `commentlint:allow-long -- 理由` を付ける。
  Goでは `// commentlint:allow-long -- 理由` を基本的に末尾へ置き、先頭のdoc commentを維持する。
  マーカーは行数だけを免除し、理由を含む幅制限は維持する。
- Goコメントの形式は `make comments-check` で検査する。

## 停止中の自動化

セキュリティ・SBOM関連は手動opt-inのmake targetに留め、明示許可なしにCI・GitHub Actionsのトリガー・Git hookへ戻さない。
mutation testingは使わない。

## 作業別ドキュメント

変更・調査する観点に対応する文書だけを読む。

- パッケージの責務・依存境界を変えるとき: [責務境界](docs/architecture.md)
- 起動・hook・返却・snapshot・resumeを扱うとき: [セッションと復元](docs/session-lifecycle.md)
- CoW・include・linkの準備処理を扱うとき: [worktreeのコピーとリンク](docs/worktree-copy.md)
- slots/statusの容量・コピー方式の計測を扱うとき: [使用量とCoWの観測](docs/storage-usage.md)
- ジョブ・補充・clear・GC・障害回復・daemon再起動を扱うとき: [daemonの補充・回収・再起動](docs/daemon-maintenance.md)
- 削除・上書き・所有権検証を扱うとき: [所有権証明](docs/ownership.md)
- path・命名・root世代・workspaceのtarを扱うとき: [ディスク配置とroot世代](docs/storage-layout.md)
