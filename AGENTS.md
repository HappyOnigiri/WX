# AGENTS.md

`wx`は、Claude CodeとCodexをdaemon管理のdetached worktreeの中で起動するGo製のCLI + daemonである。
単一ユーザー・単一マシンでの利用を前提に規模を決めており、機能の取捨も運用の複雑さもこの前提で判断する。

実行時の対象はmacOSだけで、状態やsocketのpathは`~/Library`配下に固定、daemonの常駐はLaunchAgentが担う。
ビルド対象がdarwinとlinuxに限られるのは`internal/domain`の物理path操作がこの2つにしか実装を持たないためで、linuxは退行を拾うための対象であって動作対象ではない。

## 破ってはならない不変条件

- **ソースリポジトリのHEAD・index・追跡ファイルをエージェントの作業対象にしない。**
  エージェントは常にwxが作ったworktreeの中で動く。
- **スナップショットしていない作業を破棄しない。**
  `internal/archive`のclean判定に付く`--untracked-files=all`・`--ignore-submodules=none`は、ユーザーのGit設定で隠れる内容をcleanと誤判定させないための保護であって、冗長な明示ではない。
- **Gitは必ず`internal/gitx`経由で起動する。**
  daemonが継承した`GIT_DIR`・`GIT_WORK_TREE`・`GIT_INDEX_FILE`などが子へ漏れると、無関係なリポジトリのindexに対して`add -A`や`write-tree`が成功したように振る舞う。
  その結果、実際には捕捉していないworktreeを削除するところまで進む。
  `gitx`はこれらを剥がしてから起動するので、`os/exec`でgitを直接叩かない。
- **破壊的なファイルシステム操作の前に所有権を証明する。**
  `state.OwnershipValidator`が`ErrOwnership`を返したらfail closedで、実体は削除せず`QUARANTINED`として残す。
  証明できないものを「たぶん自分のもの」として扱う経路を作らない。
- **パス名ではなくdescriptorを信頼する。**
  rootのpin（`os.Root`・`domain.OpenOwnedRoot`）、全path成分のsymlink拒否（`domain.PhysicalPathInfo`）、子プロセスCWDのfchdir束縛（`internal/fdexec`）が揃って初めてTOCTOUが閉じる。
  「descriptorが無ければパス名で代替する」フォールバックを足さない。
- **worktree rootの世代をpathで表さない。**
  slotの位置は`roots.id` + root相対pathで、所有権証明はそれとinode identityで行う。
  path文字列の一致比較を戻さない。
  `storage.worktree_root`を変えても既存slotは旧root配下で寿命を全うする（実体を移動しない・STALEにしない）。
- **貸出中のslotのworktreeを書き換えない。**
  `--branch`指定やmain更新でOIDが一致しないときは、非同期にrefreshせずcold startで作り直す。
  貸出中に書き換える経路は、未スナップショット保護と所有権の境界を壊す。

## 状態の権威はSQLiteにある

`internal/state.Store`がslot・session・jobの状態の唯一の権威で、遷移はSQLの`state IN (...)`と`RowsAffected()==1`によるcompare-and-swapで検証する。
Go側に状態のenum型や遷移ガードを作らない。
二重に持つと、Store側にしかない状態（`STALE`・`REMOVING`・`RETIRING`・`COLD`など）を表現できない検査経路ができる。

隔離も同じくstateで表す。
専用のディレクトリレイアウトは作らず、`slots.state='QUARANTINED'`と`quarantined_artifacts`テーブルが実体である。

`migrations/*.sql`は名前順に並べた添字を版番号とし、`PRAGMA user_version`より新しいものだけを適用する。
適用済みDBには既存ファイルの編集が反映されないので、スキーマ変更は必ず次の番号のファイルを追加する。
配布前の一度きりの例外として、worktreeレイアウト刷新（`roots`テーブルの導入、`slots.path`と`slot_repositories.worktree_path`の削除）では`001_initial.sql`を書き換えた。
旧レイアウトとの互換を持たない前提で、002を足すと旧レイアウト由来の列を引きずるだけになるためである。
既存の`state.db`は作り直す必要があり、移行コードは持たない。
この例外を再利用しないこと。以後のスキーマ変更は次の番号のファイルを追加する。
`state.SchemaVersion`はその本数と一致させる。
この一致を検査するテストもCI checkも無いので、手で揃える。
`state.JSONSchemaVersion`は`--json`出力の形状に対する別の契約である。
`SchemaVersion`とは独立に、出力形状が変わったときだけ上げる。

## 開発

- ローカルのゲートは`make ci`（初回のみ`make setup`）。
  `go test ./...`が通っただけで完了と判断しない。
- 参照のないコードを残せない。
  `make ci`の`deadcode`が`-test`付きで走るので、「将来使うから」とヘルパー・引数・exportを先回りで足すと落ちる。
- coreパッケージ（`config`・`state`・`pool`・`gitx`・`workspace`・`archive`・`rpc`）は関数単位でもカバレッジ0%を許さない。
  床を割ったときに設計と無関係な行を踏むだけのテストで数字を作らない。
  プロセスやOSのアダプタは`coverage-exclusions.txt`に理由付きで除外する。
- platform依存のコードを触ったら`CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`も手元で通す。
  PR CIでlinuxを踏むのは`static` jobのlintだけで、linuxのテストは週1のnightlyまで走らない。
- ファイルシステムを触るテストを持つパッケージには、`TMPDIR`を`filepath.EvalSymlinks`済みの物理パスへ差し替える`TestMain`を置く。
  macOSの`/var` → `/private/var`のようなsymlinkがあると、symlink成分を拒否する検査にテスト側のtemporary directoryが引っかかる。
- Git hookは書式・`go vet`・buildまでに絞ってある。
  全テストとlintはCIの仕事なので、hookへ足さない。
- Git hookの本体はリポジトリで管理しない。
  `$(git rev-parse --git-common-dir)`の`hooks/`直下に置き、そこから`make hook-pre-commit`・`make hook-pre-push`を呼ぶ。
  `core.hooksPath`のlocal設定はglobal設定を丸ごと覆い隠すので、userレベルのhook dispatcherを使う環境ではこのリポジトリのhookだけでなく`post-checkout`など全てのhookが止まる。
  local設定を復活させない。
- Markdown文書は1文1行で書く。
  表示幅200桁を超える文は分割する。
  全角文字は2桁として数える。

## 停止中の自動化

セキュリティ・SBOM関連の自動化はユーザーの明示判断で停止している。
手動opt-inのmake targetとしては残すが、**明示許可がない限り、CI・GitHub Actionsのトリガー・Git hookへ戻さない。**

mutation testingは使わない。
閾値を割ったsurvivorのトリアージ費用が、この規模での便益に見合わない。
再導入するとしても、survivor起因の不具合が実際に出たパッケージへ手動で限定する。

## 参照

- [docs/architecture.md](docs/architecture.md) — 層とパッケージの責務分担、セッションのライフサイクル、daemonのジョブ、所有権証明の構造、ディスク上のレイアウト。
