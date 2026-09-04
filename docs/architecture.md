# アーキテクチャ

`AGENTS.md`の不変条件が、コードのどこでどう実現されているかを示す。

## 層とパッケージ

依存はおおむね上から下へ流れるが、厳密な一方向ではない（例えば`internal/state`は`internal/discovery`のworkspace型を受け取る）。
lintが機械的に守るのは`.golangci.yml`のdepguardが書いた`internal/domain`と`internal/state`の2つぶんだけで、それ以外の辺は規約でしか守られていない。

- **入口** — `cmd/wx`、`internal/cli`。
  引数解析、daemonへのRPC、エージェント子プロセスの起動と信号中継。
- **エージェント連携** — `internal/agent`、`internal/hookconfig`。
  hookの実行と、hook設定が同期的な準備完了契約を満たしているかの判定。
- **転送** — `internal/rpc`。
  Unix socket上の長さ前置きJSON、冪等性キーによる重複実行の抑止。
- **調整** — `internal/daemon`。
  ジョブのworker、貸出と返却、standby補充、reconcile、GC、status/doctor。
- **業務** — `internal/workspace`、`internal/archive`、`internal/pool`。
  worktreeの準備と削除、スナップショットと復元、branch指定の解決。
- **永続化** — `internal/state`、`migrations`。
  SQLiteの状態と所有権証明。
- **探索** — `internal/discovery`。
  workspaceとリポジトリの解決。
  `state`がこの型を受け取るので、層としては`state`より下に位置する。
- **基盤** — `internal/domain`、`internal/gitx`、`internal/fdexec`、`internal/config`、`internal/launchd`。
  path・ID・descriptorの原始的操作、Git実行、fchdir束縛、設定、LaunchAgent。

`internal/daemon/manager.go`はstatus/doctorの組み立ても持つ。
診断用の独立したパッケージは、分割の便益が間接参照の増加を上回らないので作っていない。

`wx`のバイナリは3つの役割を兼ねる。
CLI、`wx daemon`のサーバー本体、そして`internal/fdexec`のexecトランポリン（隠しコマンド`__wx_exec_at_fd`）である。
descriptor束縛でGitやエージェントを起動する経路は、必ず自分自身を再execする形になっている。

## 1セッションのライフサイクル

1. **貸出** — `wx claude`が`ensureDaemon`でdaemonの生存を確認し（応答が遅いだけの生きたdaemonをlaunchdで再起動しないよう、接続自体に失敗したときだけkickstartする）、`ResolveAndLease`を呼ぶ。
   daemonはcwdからworkspaceを解決し、要求したOIDと完全一致するREADY slotがあれば再利用、無ければPREPAREジョブを積んで準備中のpathを返す。
   一致しなければ再利用せずcold startになる。
2. **起動** — clientはleaseのpathをdescriptorとして開き、`internal/fdexec`経由でエージェントをそのdescriptorのディレクトリで起動する。
   子プロセスには`WX_SESSION_ID`・`WX_SESSION_TOKEN`・`WX_DAEMON_SOCKET`などが渡り、以降のhookはこれを持つ場合だけ動く。
3. **準備完了のゲート** — 準備が終わっていないworktreeでエージェントが動き出さない仕組みは2通りある。
   hookが入っていれば、`wx hook user-prompt-submit`と`wx hook pre-tool-use`が`WaitReady`をブロッキングで呼ぶので、準備とエージェント起動を重ねられる。
   hookが無ければ、client側が起動前に前面で`WaitReady`を待つ（`hookconfig.Available`で分岐）。
   `wx hook session-start`は`BindAgentSession`系でエージェント側のネイティブなセッションIDをwxのセッションへ結び付ける。
4. **返却** — `wx hook session-end`が`Release`を呼ぶ。
   ただしsession-end hookはエージェント本体より先に走ることがあるため、clientまたはエージェントのプロセスが生きている間は返却しない。
   前面clientの終了通知か、daemonのorphan reconcileが「もう書き手がいない」ことを確認してから先へ進む。
5. **アーカイブ** — SNAPSHOTジョブが2種類のスナップショットを取る。
   リポジトリごとのHEAD・index・worktreeは`refs/wx/recovery/<session>/<repo>/*`として**ソースリポジトリ側の**保護オブジェクトとrefになる。
   multi-repository workspaceではさらに、workspace root自体のtarをwxのworktree root配下（`recovery/workspace-snapshots/`）へ書き、`workspace_snapshots`行が指す。
   refの公開はDB行の永続化の後に行う。
   逆順だと、reconcileから見て正常なアーカイブが素性不明のrefに見える窓が開く。
6. **再開** — `wx resume`または各エージェントのネイティブなresumeが`Resume`/`AllocateResumeSlot`を通り、RESTOREジョブがスナップショットを新しいslotへ戻す。
   ネイティブresumeはhookが揃っていることを要求し、揃っていなければ前面で待つ`wx resume <wx-session-id>`へ倒す。
   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

## daemonの内部

- **ジョブ** — 永続ジョブは`jobs`テーブルにあり、種別は`PREPARE`、`ENSURE_STANDBY`、`SNAPSHOT`、`RESTORE`、`REMOVE`、`REMOVE_REPOSITORY`。
  workerがリースを取って実行する。
  失敗の包み方は`retryableJobError`と`dependencyPendingError`の2種である。
  **所有権失敗はどちらにも包まず終端させる**（証明できない実体に対する再試行は危険側に倒れる）。
- **standby補充** — 直近に使われた（hot）workspaceに対して、READYなslotを`pool.warm_per_workspace`まで先回りで用意する。
  `Store.HotRepositoryIDs`は`repositories.last_leased_at`でリポジトリ単位に絞る形をしている。
  その列を更新する側は5箇所すべてworkspace単位であるため、同じworkspaceのリポジトリを一律に同じ時刻へ揃える。
  そのため、貸出直後の`ENSURE_STANDBY`では全リポジトリがhotと判定され、このフィルタは実質効かない。
  粒度を上げるには、貸出時に実際に使ったリポジトリを`session_repositories`側へ記録し、`HotRepositoryIDs`とGCの`ColdRepositoryCandidates`の両方をそこから読ませる必要がある。
- **reconcile** — 定期的にDBと実体を突き合わせ、素性の分からないpath・refを隔離側へ倒す。
  clientとエージェントの両プロセスが死んでいるsessionはここで返却される。
- **degraded運用** — SQLiteが開けないときも`Status`・`Doctor`・`RequestStop`は`DegradedHandler`が答える。
  診断のためにdaemonを完全に沈黙させないためである。
  `RequestStop`だけは状態を変えるがゲートを通さない（状態を変えるRPCを一切受け付けない以上、守るべきin-flightの予約が無い）。
- **restart / stopのidleゲート** — `wx daemon restart`・`wx daemon stop`とバイナリ差し替えの自動検知は、いずれも`restartPending`・`stopPending`を立てるだけである。
  実行は`maintainJobs`から`runPendingLifecycle`が駆動する。
  ゲートは「in-flightのRPCが0」「最後のRPC終了から`lifecycleQuietPeriod`（5秒）経過」「`jobs`が0」を要求する。
  SIGTERMがin-flightのRPCを救わない（応答を書かずに接続が閉じる）以上、これが唯一の保護である。
  1回のwx起動が独立した複数のRPCの列である以上、in-flightが0になった最初の瞬間は安全な瞬間ではないので、quiet periodを別に要求する。
  ただしquiet periodを数える対象から`RequestStop`・`RequestRestart`・`RequestStart`自身は外す（`Handler.Handle`が`isLifecycleMethod`で振り分ける）。
  これらは守るべきユーザー操作ではなくゲートを待っている側であり、数えるとidleなdaemonでも自分が作ったquiet periodを5秒待つことになる。
  in-flightの数え上げからは外さない。
  応答フレームは`Handler.Handle`が戻った後に書かれるので、代わりに`lifecycleReplyGrace`（100ミリ秒）だけゲートを閉じたままにして、受理を伝える応答がそのsignalに切られないようにする。
  restartは`underLaunchd()`も要求する（手動起動のdaemonがkickstartすると二重起動になる）が、stopは要求しない。
  この判定は`launchd_managed`として応答にも載るので、CLIは来ない置換を待たずに断れる。
  要求は互いを打ち消し、同時に2つはpendingにならない。
  ただし打ち消せるのはsignalに渡される前までで、渡した後に届いた逆の要求は`conflict`として断る（配送済みのsignalは呼び戻せず、受理したように答えると呼び出し側が来ない状態を待ち続ける）。
  `wx daemon start`は起動済みのdaemonにも`RequestStart`を送る。
  CLIが待ちきれずに諦めたstopはpendingのまま残るので、signalに渡される前ならここで取り消す。
  渡された後は取り消せないため、CLIは終了を待ってからlaunchd経由で起動し直す。
  ゲートを通過したら、restartは`launchd.Kickstart`、stopは自プロセスへのSIGTERMを1度だけ発行する。
  同期待ちするCLI側は、stopとstartはsocketへのdial可否だけを見る（`Status`をpollingすると`lastRequestEnd`が更新され続けてゲートが永久に開かない）。
  restartだけは、listenerの消失を観測した後と、待ち切れた後の2箇所で`Status`のpidを読む。
  50msのプローブはkickstartが閉じて開き直す数ミリ秒を丸ごと取りこぼしうるので、置換の判定は観測した断ではなくpidの比較で行うためである。
  前者はkickstartが済んだ後なのでゲートに影響しないが、後者は諦める直前の1回で、restartがまだpendingならquiet periodを押し出す。
  待っている間、CLIは`stopping...`のような行を出す。
  ゲートが開くまでdaemonは何も言わないので、これが唯一の生存表示になる。
  ドットのアニメーションはstdoutが端末のときだけで、pipeやファイルへ出しているときは待機行を一切出さない（`interactiveOutput`）。

## 所有権の証明

破壊的操作の前に求める証明は、次の3つが同時に一致することである。

1. **DBの行** — `ValidateWorktreeOwnership`が`slots`・`slot_repositories`・`workspaces`・`repositories`・`workspace_repositories`の5表を結合して1行を読む。
   slotとリポジトリのstate、slot path、common dir、workspace内の相対pathが期待と一致することを確かめる。
   読み取り専用トランザクションが囲むのはSELECTだけで、path正規化と一致判定はcommit後に走る。
2. **ファイルシステム上のマーカー** — slotディレクトリ配下の`.wx-owner-<target由来の安定ID>`に、slot ID・対象worktree・common dirをJSONで書く。
   内容が一致しないマーカーは所有の否定として扱う。
3. **Gitのworktree lock** — wx自身が付けた`wx:<slot-id>:READY`・`PREPARING`・`RESTORING`のいずれかであること（`domain.ValidWxLockReason`）。
   認識できない理由でlockされたworktreeは、wxのものではない。

このうち2と3は、pinしたroot descriptor配下の相対pathに対して行う。
`domain.OpenOwnedRoot`がrootをinodeごとpinし、`domain.PhysicalPathInfo`が全成分のsymlinkを拒否するので、検査と実行の間にpathを差し替えられても、差し替え先へ操作が届かない。
1のDB側はpath名の正規化で比較しており、descriptorには載っていない。

## ディスク上のレイアウト

worktree root（`storage.worktree_root`）配下は次の形になる。

```text
<worktree_root>/workspaces/<workspace-id>/slots/<slot-id>/root/
<worktree_root>/unbound/<slot-id>/root/
<worktree_root>/recovery/workspace-snapshots/<安定ID>.tar
```

`root/`がエージェントへ貸し出す単位で、`unbound/`はworkspaceが確定していないslotが入る。
状態は`~/Library/Application Support/wx/state.db`が持ち、同じ場所の`state.db.backups/`にオンラインバックアップを世代保存する。

リポジトリ単位の復旧スナップショットだけは**wxの領域ではなくソースリポジトリのGitオブジェクトとref**として置かれるため、そのリポジトリを読めるプロセスからは中身が読める。

## 未実装のまま残しているもの

- `.worktreelink`に列挙したpathは、main worktree側の実体へ直接symlinkする（`createLinksAt`）。
  workspace内の相対位置を保って再構成する処理は持たない。
  必要になったら`~/.config/git/hooks/worktreelink-post-checkout`に実装済みのアルゴリズムを移植する。
- 生成器を持たない。
  help本文、config schema、SQLite migration、LaunchAgent plistはすべて手書きで維持し、shell completionは実装しない。
  `make`の`generated-check`が現状なにも検証していない理由と、それでも残している理由は`Makefile`の同targetのコメントにある。
