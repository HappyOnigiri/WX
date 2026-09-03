# AGENTS.md

`wx`は、Claude CodeとCodexをdaemon管理のdetached worktreeの中で起動する
Go製のCLI + daemonである。単一ユーザー・単一マシンでの利用を前提に規模を
決めており、機能の取捨も運用の複雑さもこの前提で判断する。

実行時の対象はmacOSだけで、状態やsocketのpathは`~/Library`配下に固定、
daemonの常駐はLaunchAgentが担う。ビルド対象がdarwinとlinuxに限られるのは
`internal/domain`の物理path操作がこの2つにしか実装を持たないためで、linuxは
退行を拾うための対象であって動作対象ではない。

## 破ってはならない不変条件

- **ソースリポジトリのHEAD・index・追跡ファイルをエージェントの作業対象に
  しない。** エージェントは常にwxが作ったworktreeの中で動く。
- **スナップショットしていない作業を破棄しない。** `internal/archive`の
  clean判定に付く`--untracked-files=all`・`--ignore-submodules=none`は、
  ユーザーのGit設定で隠れる内容をcleanと誤判定させないための保護であって、
  冗長な明示ではない。
- **Gitは必ず`internal/gitx`経由で起動する。** daemonが継承した
  `GIT_DIR`・`GIT_WORK_TREE`・`GIT_INDEX_FILE`などが子へ漏れると、無関係な
  リポジトリのindexに対して`add -A`や`write-tree`が成功したように振る舞い、
  実際には捕捉していないworktreeを削除するところまで進む。`gitx`はこれらを
  剥がしてから起動するので、`os/exec`でgitを直接叩かない。
- **破壊的なファイルシステム操作の前に所有権を証明する。**
  `state.OwnershipValidator`が`ErrOwnership`を返したらfail closedで、実体は
  削除せず`QUARANTINED`として残す。証明できないものを「たぶん自分のもの」と
  して扱う経路を作らない。
- **パス名ではなくdescriptorを信頼する。** rootのpin（`os.Root`・
  `domain.OpenOwnedRoot`）、全path成分のsymlink拒否
  （`domain.PhysicalPathInfo`）、子プロセスCWDのfchdir束縛
  （`internal/fdexec`）が揃って初めてTOCTOUが閉じる。「descriptorが無ければ
  パス名で代替する」フォールバックを足さない。
- **貸出中のslotのworktreeを書き換えない。** `--branch`指定やmain更新で
  OIDが一致しないときは、非同期にrefreshせずcold startで作り直す。貸出中に
  書き換える経路は、未スナップショット保護と所有権の境界を壊す。

## 状態の権威はSQLiteにある

`internal/state.Store`がslot・session・jobの状態の唯一の権威で、遷移はSQLの
`state IN (...)`と`RowsAffected()==1`によるcompare-and-swapで検証する。Go側に
状態のenum型や遷移ガードを作らない。二重に持つと、Store側にしかない状態
（`STALE`・`REMOVING`・`RETIRING`・`COLD`など）を表現できない検査経路が
できる。

隔離も同じくstateで表す。専用のディレクトリレイアウトは作らず、
`slots.state='QUARANTINED'`と`quarantined_artifacts`テーブルが実体である。

`migrations/*.sql`は名前順に並べた添字を版番号とし、`PRAGMA user_version`より
新しいものだけを適用する。適用済みDBには既存ファイルの編集が反映されないので、
スキーマ変更は必ず次の番号のファイルを追加する。`state.SchemaVersion`はその
本数と一致させる。この一致を検査するテストもCI checkも無いので、手で揃える。
`state.JSONSchemaVersion`は`--json`出力の形状に対する別の契約なので、
`SchemaVersion`とは独立に、出力形状が変わったときだけ上げる。

## 開発

- ローカルのゲートは`make ci`（初回のみ`make setup`）。`go test ./...`が
  通っただけで完了と判断しない。
- 参照のないコードを残せない。`make ci`の`deadcode`が`-test`付きで走るので、
  「将来使うから」とヘルパー・引数・exportを先回りで足すと落ちる。
- coreパッケージ（`config`・`state`・`pool`・`gitx`・`workspace`・
  `archive`・`rpc`）は関数単位でもカバレッジ0%を許さない。一方で、床を割った
  ときに設計と無関係な行を踏むだけのテストで数字を作らない。プロセスやOSの
  アダプタは`coverage-exclusions.txt`に理由付きで除外する。
- platform依存のコードを触ったら
  `CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`も手元で通す。
  PR CIでlinuxを踏むのは`static` jobのlintだけで、linuxのテストは週1の
  nightlyまで走らない。
- ファイルシステムを触るテストを持つパッケージには、`TMPDIR`を
  `filepath.EvalSymlinks`済みの物理パスへ差し替える`TestMain`を置く。macOSの
  `/var` → `/private/var`のようなsymlinkがあると、symlink成分を拒否する検査に
  テスト側のtemporary directoryが引っかかる。
- Git hookは書式・`go vet`・buildまでに絞ってある。全テストとlintはCIの仕事
  なので、hookへ足さない。

## 停止中の自動化

セキュリティ・SBOM関連の自動化はユーザーの明示判断で停止している。手動
opt-inのmake targetとしては残すが、**明示許可がない限り、CI・GitHub Actions
のトリガー・Git hookへ戻さない。**

mutation testingは使わない。閾値を割ったsurvivorのトリアージ費用が、この規模
での便益に見合わない。再導入するとしても、survivor起因の不具合が実際に出た
パッケージへ手動で限定する。

## 参照

- [docs/architecture.md](docs/architecture.md) — 層とパッケージの責務分担、
  セッションのライフサイクル、daemonのジョブ、所有権証明の構造、ディスク上の
  レイアウト。
