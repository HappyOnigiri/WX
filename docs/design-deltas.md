# 設計差分

このリポジトリは `wx-daemon-worktree-manager` 設計書をもとに実装されている。
このドキュメントは、設計書が要求する項目のうち **実装しないと決めたもの** を
記録する。設計書自体はこのリポジトリの外にあり、行番号引用を通じてしか参照
していないため編集しない。ここでの行番号は執筆時点の参照であり、設計書側の
改版で前後する場合がある。

各項目は次の観点で書く。

- 設計書の要求: 何を求めていたか
- 実装の判断: 実際に何をした・しなかったか
- 理由: そう決めた根拠
- 将来必要になったときの入り口: 再検討する場合の起点

## 1. 状態機械の型（`SlotState` / `SessionState` / `SlotID` / `SessionID`）

- 設計書の要求: slotとsessionの状態機械を専用の型（`SlotState`、
  `SessionState`）と、遷移を検査する`SlotID`・`SessionID`型で表現する
  （設計書のstate machine節、577行目付近）。
- 実装の判断: 型と`CanTransitionSlot`のような遷移ガードは持たない。状態は
  `internal/state.Store`が扱うSQLiteの文字列列（`slots.state`、
  `sessions.state`など）で表現し、遷移はSQLiteのcompare-and-swap
  （`state IN (...)`と`RowsAffected()==1`の組）で検証する。この判断の経緯は
  `internal/domain/domain.go`のコメントに記録済みで、このファイルはそこへの
  索引を兼ねる。
- 理由: 過去に存在した`SlotState`/`SessionState`/`SlotID`/`SessionID`と
  `CanTransitionSlot`は本番コードのどこからも参照されない死んだ第二の検査
  経路で、`internal/state.Store`が既に持つ状態集合（`STALE`、`REMOVING`、
  `RETIRING`、`COLD`、`slot_repositories`の`PREPARE_RUNNING`/
  `RESTORE_RUNNING`など）を表現できていなかった。挙動を変えずに削除した。
- 将来必要になったときの入り口: 型による静的検査が要る規模になったら、
  `internal/state.Store`が実際に扱っている状態集合と遷移表を先に洗い出し、
  そこから型を起こす。

## 2. 生成物の再生成差分check（設計書のCI章、1137行目付近）

- 設計書の要求: help本文、config schema、SQLite migration、LaunchAgent
  plist、shell completionなど「生成元を持つartifact」の再生成差分がないこと
  をrequired checkにする。
- 実装の判断: 生成器を持たない方針とした。help本文、config schema、SQLite
  migration、LaunchAgent plistはいずれも手書きで維持し、shell completionは
  実装しない。`Makefile`の`generated-check`（`go generate ./...`の後に
  `git diff --exit-code`）は残すが、`//go:generate`ディレクティブがこの
  リポジトリに0件のため、現状は構造的に常に緑で何も検証していない。
- 理由: 検証していないものを検証しているかのように精緻化する（例えば
  temporary directoryへ展開して比較する）と、運用コストだけが増える。
  target自体を削除しない理由は、`generated-check`という名前がGitHubの
  branch protectionのrequired checkに登録されている可能性があり、その設定を
  403（権限不足）で確認できないため。削除できない理由であって、精緻化する
  理由ではない。
- 将来必要になったときの入り口: 上記artifactのいずれかに実際の
  `//go:generate`ディレクティブを追加すれば、`generated-check`は追加の配線
  なしにその場で効き始める。

## 3. `.worktreelink`内のworkspace相対symlink再構成（設計書578行目付近）

- 設計書の要求: `.worktreelink`に列挙したpathを、workspace内の相対位置を
  保ったままsymlinkとして再構成する。
- 実装の判断: 実装しない。`internal/workspace/prepare.go`の
  `createLinksAt`は、`.worktreelink`に列挙されたrelative pathをmain
  worktree側の実体へ直接symlinkするだけで、workspace側の相対位置を
  再現する処理は持たない。
- 理由: 個人利用の規模ではworkspace相対の再構成が必要になる場面が出ていない。
- 将来必要になったときの入り口: `~/.config/git/hooks/worktreelink-post-checkout`
  に実装済みのアルゴリズムを移植する。

## 4. `internal/diagnostics/`パッケージ（設計書946行目付近）

- 設計書の要求: `Status`/`Doctor`の実装を独立した`internal/diagnostics/`
  パッケージに置く。
- 実装の判断: 新設しない。`Status`/`Doctor`相当の組み立ては
  `internal/daemon`（`manager.go`）に置く。
- 理由: 個人利用の規模でパッケージを分ける便益が、分割による間接参照の
  増加を上回らない。
- 将来必要になったときの入り口: `internal/daemon/manager.go`の
  status/doctor関連関数が肥大化した時点で切り出す。

## 5. top-level `testdata/`（設計書951行目付近）

- 設計書の要求: 合成repositoryやhook payloadなどのテスト固有入力を
  top-levelの`testdata/`に集約する。
- 実装の判断: 作らない。テスト固有の入力は各パッケージ配下
  （例: `internal/agent/testdata`）に置く。
- 理由: 現状のテスト資産の量では、パッケージ配下に置く方がテストとの
  対応が読み取りやすく、集約の便益が薄い。
- 将来必要になったときの入り口: 複数パッケージで同じ合成repositoryや
  payloadを再利用する必要が出た時点で、top-levelへ寄せることを検討する。

## 6. `workspaces/<workspace-id>/quarantine/`のディレクトリレイアウト（設計書578行目付近）

- 設計書の要求: 所有権やGit状態が不明なslot・artifactを、
  workspaceごとの`quarantine/`ディレクトリへ物理的に隔離する。
- 実装の判断: 作らない。隔離はディレクトリレイアウトではなくDBのstateで
  表現する。`slots.state='QUARANTINED'`と、`quarantined_artifacts`
  テーブル（`migrations/005_rpc_idempotency.sql`）がその実体である。
- 理由: 設計書566行目付近が求める「自動破棄しない」という要求は、DB側の
  state表現でも満たしている。専用ディレクトリレイアウトを別途維持する
  追加コストに見合わない。
- 将来必要になったときの入り口: 隔離対象をファイルシステム上で目視・
  一括操作する必要が出た時点で、DBのQUARANTINED行から実体pathを列挙する
  補助コマンドを`wx doctor`側に足す方が、レイアウトを新設するより先に
  検討する価値がある。

## 7. 貸出手順 step 4の非同期refresh（設計書603行目・609行目付近）

- 設計書の要求: `--branch`指定やmain更新でREADY slotのOID・fingerprintが
  要求と一致しない場合、slotをLEASEDのまま非同期でrefreshし、
  準備中pathをclientへ即座に返す。設計書1246行目付近の受け入れ目標は、
  daemon起動済み・READY hot slot・main OID不変の通常ケースで、人間に
  知覚されるcheckout待ちを発生させないこと。
- 実装の判断: 実装しない。`--branch`指定時もREADY poolを確認しOIDが
  完全一致すれば再利用するところまでで止める。不一致時と、main更新直後は
  cold startになる。
- 理由: 貸出中slotのworktreeを書き換える経路を増やすと、未スナップショット
  状態の保護と所有権証明の境界（このリポジトリで縮小対象外とした不変条件）
  が複雑になる。個人利用の規模ではその複雑化が便益を上回る。上記の受け入れ
  目標は、この範囲では満たされない。
- 将来必要になったときの入り口: branch切り替えやmain更新の頻度が上がり
  cold startの待ちが体感上問題になった時点で、非同期refresh対象を
  LEASED状態のslotに限定した設計から着手する。

## 8. standbyのhot期間判定をrepository単位で行うこと（設計書588行目・604行目付近）

- 設計書の要求: standby slotのcheckout対象を、repositoryごとのhot期間
  （直近の貸出からの経過時間）で判定する。hot期間を外れたrepositoryは
  standbyのcheckout対象から外し、COLDとして退役させる。
- 実装の判断: workspace単位の判定までで止める。`ENSURE_STANDBY`は
  workspaceの最終貸出時刻でフィルタするので、貸出直後にstandbyを積む
  通常の流れではフィルタが実質的に効かず、hot期間を外れたrepositoryも
  checkout対象に残る。
- 理由: 判定に使う列（`last_leased_at`）をworkspace単位で更新しており、
  repository単位に分けるとGCのcold退役条件が同じ列を共有しているため、
  永続化層とGCの両方に変更が及ぶ。個人利用の規模では、hot期間を外れた
  repositoryをstandbyに含めても無駄なcheckoutが増えるだけで、データ安全性
  には影響しない。
- 将来必要になったときの入り口: workspaceあたりのrepository数が増えて
  standby準備のコストが問題になった時点で、`session_repositories`側に
  repository単位の最終貸出時刻を持たせ、`ENSURE_STANDBY`とGCの両方を
  そこから読む形にする。

## 9. CIゲートの構成とカバレッジの床（設計書1137行目付近のCI章）

- 設計書の要求: `coverage gate`、変更行coverage、mutation gateを含む
  複数のcoverage関連gateと、integration shard分割、amd64/arm64両matrixでの
  concurrency testなど、幅広いrequired checkの構成。
- 実装の判断: PR CIは`static`/`coverage`/`build`/`required`の4 jobへ縮小
  した。`coverage`は全パッケージを`-race`付きで実行しcoverage gateも兼ねる。
  integration shard（state/git/daemon/launchd）は`coverage-check`が同じ
  範囲を包含するため削除した。amd64（Intel）matrixは全廃し、
  `build-darwin`のクロスビルドで退行を拾う。重い反復テスト
  （`-count=10`のrace、soak、crash、reproducible-build、portable-test）は
  週1のnightlyへ移した。カバレッジは overall 80% / core 85% を床とし、
  差分カバレッジgateは設けない。
- 理由: 実測（PR #1のActions実行、27 job・wall clock 41分20秒）で、
  `fmt-check`単体1分強のjob起動オーバーヘッドをmatrix job数分払っている点、
  integration shardとcoverage-checkが同じrace付き全スイートを2回実行して
  いる点、macOS runner（課金10倍）が全jobの半分を占める点が判明した。
  単一ユーザー・単一マシン・未公開・Draft PR段階という運用規模に対し、
  この重複と待ち時間は便益に見合わない。カバレッジの床は、閾値を割った
  ために設計と無関係な行を稼ぐ作業が発生していたため、テスト設計を
  駆動しない水準まで下げた。
- 将来必要になったときの入り口: 複数人での運用や公開に伴いPRの本数・
  同時実行数が増えたら、integration shard分割とamd64 matrixを先に復元する
  （`coverage-check`と重複させないよう、対象を明示的に分ける設計に戻す）。

## 10. git hookの検査範囲

- 設計書の要求: 明記なし（実装時の運用判断）。
- 実装の判断: pre-commitは書式（`gofumpt`/`gci`のdiff検査）のみ、pre-push
  は書式 + `go vet` + buildに絞った。全パッケージの`go test`と
  `golangci-lint run ./...`はhookから外し、CIの仕事にした。
- 理由: 全パッケージのtestは実測40〜48秒かかり、hookを30秒以内に収める
  目標と両立しない。lintとtestはPR CIの`static`/`coverage` jobが必ず
  実行するため、hookで重複させる必要がない。
- 将来必要になったときの入り口: 30秒の目安を明確に超える規模になったら、
  変更pathに応じた部分実行（変更packageだけの`go vet`など）を検討する。
  固定timeoutは導入しない方針を維持する。

## 11. mutation testing

- 設計書の要求: coverage gate・変更行coverageと並ぶrequired checkとして
  mutation gateを持つ（設計書1137行目付近）。
- 実装の判断: 実施しない。`gremlins`関連のtool、target
  （`mutation-check`/`mutation-full-check`/`mutation-run`）、
  `scripts/check-mutation-survivors.sh`を削除した。
- 理由: 閾値を割った生存ミュータントの個別トリアージにかかる継続コストが、
  単一ユーザー・単一マシンの運用規模での便益を上回ると判断した。
- 将来必要になったときの入り口: 中核package（`internal/config`、
  `internal/state`、`internal/pool`、`internal/gitx`、
  `internal/workspace`、`internal/archive`、`internal/rpc`）のテストで
  survivorが疑われる不具合が実際に発生した時点で、必要な範囲だけ手動で
  `gremlins`を導入し直す。

## 12. 静的セキュリティ解析の自動化

- 設計書の要求: `golangci-lint`の`gosec`有効化、`govulncheck`、依存関係
  scan、secret scan、SBOM生成、Dependabotなどをrequired checkおよび
  自動化として組み込む。
- 実装の判断: ユーザー指示により停止中。`security.yml`のトリガーは
  `workflow_dispatch`のみとし、`.github/dependabot.yml.disabled`として
  無効化している。`Makefile`の`security-local`/`govulncheck`/
  `dependency-check`/`gosec`/`license-check`/`secret-check`/`sbom`/
  `setup-security-tools`/`setup-sbom-tools`は手動opt-inのまま維持する。
- 理由: ユーザーの明示判断による一時停止であり、このドキュメントが対象と
  する縮小作業の範囲外。
- 将来必要になったときの入り口: **ユーザーの明示許可がない限り
  再有効化しない。** 許可が出た場合は、`security.yml`のトリガーを
  `pull_request`/`push`/`merge_group`へ戻し、`dependabot.yml.disabled`を
  `dependabot.yml`へリネームすることから始める。
