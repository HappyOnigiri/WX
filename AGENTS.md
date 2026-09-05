# AGENTS.md

`wx`は、Claude CodeとCodexをdaemon管理のdetached worktreeで起動するGo製CLI + daemonである。
機能と運用の複雑さは単一ユーザー・単一マシンを前提に判断する。
実行対象はmacOSのみ（状態・socketは`~/Library`配下、常駐はLaunchAgent）で、linuxは退行検出用のビルド対象とする。

## 不変条件

- エージェントはwxが作ったworktreeで作業し、ソースリポジトリのHEAD・index・追跡ファイルを変更しない。
- スナップショットしていない作業を破棄しない。
  `internal/archive`のclean判定では、Git設定で隠れる変更を見逃さないよう`--untracked-files=all`・`--ignore-submodules=none`を維持する。
- Gitは必ず`internal/gitx`経由で起動する。
  継承した`GIT_DIR`・`GIT_WORK_TREE`・`GIT_INDEX_FILE`などが漏れると、別リポジトリへの操作が成功し、未捕捉のworktreeを削除し得る。
- 破壊的なファイルシステム操作の前に所有権を証明する。
  `state.OwnershipValidator`が`ErrOwnership`を返したら、実体を削除せず`QUARANTINED`として残す。
- TOCTOU対策はrootのpin（`os.Root`・`domain.OpenOwnedRoot`）、全path成分のsymlink拒否（`domain.PhysicalPathInfo`）、子プロセスCWDのfchdir束縛（`internal/fdexec`）を揃える。
  descriptorがない場合にパス名で代替しない。
- slotの位置は`roots.id` + root相対pathで表し、所有権はそれとinode identityで証明する。
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

- ローカルのゲートは`make ci`（初回のみ`make setup`）。
- coreパッケージのカバレッジ基準は`tools/checkcoverage`を参照する。
  設計と無関係な行を踏むだけのテストで数字を作らず、プロセスやOSのアダプタは`coverage-exclusions.txt`に理由付きで除外する。
- platform依存のコードを触ったら`CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`も手元で通す。
- ファイルシステムを触るテストを持つパッケージには、`TMPDIR`を`filepath.EvalSymlinks`済みの物理パスへ差し替える`TestMain`を置く。
  macOSの`/var` → `/private/var`などがsymlink拒否の検査に引っかかるためである。
- `internal/daemon`のトップレベルテストは、専用の一時ディレクトリ・DB・Managerだけを使うものに`t.Parallel()`を付ける。
  `t.Setenv`を自身かサブテストで呼ぶテスト、プロセス全体のgoroutine・fdを数えるテスト、短い待機に依存するテストは直列のまま残す。
- CIはrace検査とcoverage計測（`make coverage-check`）を別ジョブで並列に実行し、`coverage`ジョブが両方の結果を集約する。
  race検査は3コアランナーでCPU律速になるため、`make test-race-daemon`と`make test-race-rest`の2ランナーへ分ける。
  手元の`make ci`と`make test-race`は全パッケージをまとめて検査し、`make test-race-coverage`は両者を同時に走らせた場合の比較・診断用に残す。
- Git hookは書式・`go vet`・buildに絞る。
  本体は`$(git rev-parse --git-common-dir)`の`hooks/`直下に置き、`make hook-pre-commit`・`make hook-pre-push`を呼ぶ。
  `core.hooksPath`のlocal設定はuserレベルのhook dispatcherを覆い隠し、`post-checkout`なども止めるため設定しない。
- Markdown文書は1文1行とし、表示幅200桁を超える文は分割する（全角文字は2桁）。
- READMEは紹介・導入を中心としたコンパクトな記載に留める。
- 機能追加のたびにREADMEへ追記しない。
- READMEとの不整合が起きたときだけ修正する。

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
  日本語の使用と説明の有用性はレビューで確認する。

## 停止中の自動化

セキュリティ・SBOM関連は手動opt-inのmake targetに留め、明示許可なしにCI・GitHub Actionsのトリガー・Git hookへ戻さない。
mutation testingは使わない。
再導入はsurvivor起因の不具合が実際に出たパッケージへの手動実行に限定する。

## 参照

- [docs/architecture.md](docs/architecture.md) — 責務分担、ライフサイクル、所有権証明、ディスク配置の実装。
