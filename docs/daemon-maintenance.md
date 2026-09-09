# daemonの補充・回収・再起動

ジョブ配送は`internal/daemon/jobs.go`、実行枠は`jobqueue.go`、周期処理は`maintenance.go`、実体照合は`reconcile.go`を参照する。
準備・復元の所有権失敗は終端させ、削除はDB登録済みの範囲を回収する。

## ジョブの分類と実行枠

実行枠は利用者向けと保守用に分かれる。
利用者向けの枠数は`pool.preparation_concurrency`（既定2）、保守用は常に1本で、保守へ利用者向けの枠を貸さない。
`preparation_concurrency: 1`でも保守と利用者処理はそれぞれ1本の枠を持ち、資源上限は既定で3本同時になる。
利用者向けが満杯のときの待ちと、物理ディスクの帯域競合は残る。

分類は`jobClassOf`が job rowの事実だけから決め、DBへ永続化しない。
session付きPREPARE・RESTORE・SNAPSHOTを利用者向けとし、SNAPSHOTは保存と将来のresumeの前提なので利用者が明示的に待っているかによらずこのクラスに置く。
待機用PREPARE・ENSURE_STANDBY・自動REMOVE系は保守用とし、実行中のclean runが完了を待つREMOVEだけを`advanceRemoving`が毎回の監視で利用者向けへ昇格させる。

`dispatchJobs`はクラス別の待ち行列から到着順に1件ずつ配り、枠を取ってから`ClaimJob`する。
このためキュー待ちのジョブはattemptもjob leaseも消費せず、同じジョブIDの二重登録も配送前に落とす。
待ち行列の上限を超えた分は`PENDING`のdurable jobとして残し、`maintainJobs`の10秒周期の回収が拾う。
実行枠は`jobExecutionSlot`としてジョブのgoroutineと分けてあり、実行中のコピーやprepare commandは優先度の変更でも枠の縮小でも中断しない。

## slot排他と共通ロック

同じslotへ書く準備・復元・保存・削除は、`roots.id`とroot相対pathをkeyにした`gitx.KeyedLocks`（daemonの`slotLocks`）で直列化する。
作成前後で変わるinodeはkeyに含めず、所有権検証の入力としてだけ使う。
slot lockは最上位のoperationで一度だけ取る。
通常の二段階準備は`prepareStagedSlot`が二巡全体のlockを保持する。
その他の入口は`Preparer.Prepare`・`archive.Restore`・`archive.SnapshotWithPersistence`・`archive.RemoveWorktree`で、内側の経路には取得済みのcontextを渡して取り直させない。

リポジトリ共有のGit管理情報は`common directory`をkeyにした同じ仕組みで排他する。
`prepare`はworktreeの作成とlock reasonの確立まで、そして`READY`へ移す最後の区間だけこのロックを保持し、その間のコピー・link・prepare commandは保持せずに行う。
このため同じリポジトリの別slotは、先行slotのコピーやprepare commandの完了を待たずに準備できる。
ロックを取り直す区間の入口では、DB状態・root/path/marker/inode・Git登録・OID・lock reasonを検証し直す。

取得順序はslot、common directory、実行枠に統一する。
どちらのロックも待機に入る前に`jobExecutionSlot`を返し、ロックを取得してから枠を取り直すので、Git管理操作を待つだけのジョブが無関係なリポジトリの枠を占有しない。
`preparation_concurrency: 1`でも、枠の取り直しがロック取得後であるため循環待ちにならない。
待機中もジョブの処理位置とlease更新は保たれ、ジョブ先頭からのretryにはならない。
contextが終わった要求はロックを取らず、callbackも実行しない。

## standby補充

`standby.go`は`hot`なworkspaceのREADY slotを待機枠数まで補充する。個数は
`workspaces.<root>.warm_count`、未指定なら`pool.warm_per_workspace`、さらに未指定なら既定値1の順で決まる。
workspace個別値は単一リポジトリではそのリポジトリのmain worktree、multi-repositoryではworkspace rootに適用する。
値0はそのworkspaceの補充を無効にするが、個数指定だけで`hot`へは変更しない。
設定ファイルでは次のように指定する。

```yaml
pool:
  warm_per_workspace: 1
workspaces:
  /path/to/repository:
    warm_count: 3
```

`wx config --workspace /path/to/repository`で実効値と継承元を確認し、
`wx config --workspace /path/to/repository warm_count 3`で変更する。
`warm_count 0`は補充を止め、`warm_count --reset`はグローバル値の継承へ戻す。
`worktree.reuse_standby`は既定でtrueであり、`workspaces.<root>.reuse_standby`が個別値を上書きする。
`wx config --workspace <path> reuse_standby false`はOID・fingerprint完全一致だけを貸す従来動作へ戻し、`--reset`はグローバル値の継承へ戻す。
`Store.HotRepositoryIDs`は`repositories.last_leased_at`で絞るが、貸出時の更新はworkspace単位なので、直後の補充では全リポジトリがhotになる。
リポジトリごとの利用に絞るなら、`session_repositories`へ実利用を記録し、`HotRepositoryIDs`とGCの`ColdRepositoryCandidates`をともに変更する必要がある。

リトライ中の`FAILED` slotは待機枠に数える。
通常sessionの準備成功または検証済みREADY slotの貸出時に、その成功に紐付く除外記録で補充数の計算から外す。
除外記録はslotの状態や実体を変更せず、同じ成功の再処理で後発の失敗slotまで除外しない。
復元成功や`SessionStart`による`ACTIVE`遷移だけでは除外記録を作らず、補充の契機にもならない。
`QUARANTINED`は待機枠に数えないが、待機用PREPAREの失敗後は補充を停止することでGCとの作成・削除ループを防ぐ。

個数を増やした設定の反映は次の保守一巡で不足分を補充する。減らした場合は準備中の処理を中断せず、完了後に余剰のREADY slotを既存GCが回収する。
貸出中slotは回収せず、保持期間によるCOLD化もworkspaceごとの実効値が正のときだけ行う。

再利用が有効な定期reconcileはREADY slotを保存済みOIDと更新互換fingerprintで検証し、現在のmainとの差だけではSTALEにしない。
配置履歴を持たないREADY slotは更新に使えないため、この検証の対象から外し、現在のmainと完全一致でなければSTALEにする。
貸出時に更新不適格と判定した候補もSTALEにして回収・補充へ回す。残しても毎回Cold Startになる一方で待機枠を占有し続けるためである。
`--branch`指定の貸出では回収しない。main向けのstandbyをbranch要求のために捨てないためである。
OIDと配置の更新は貸出要求時だけ行い、要求時点のOID・配置計画・copy modeをDBへ固定する。
UPDATEは利用者向け実行枠を使い、slot・STARTING session・jobの予約を同じtransactionで確定する。

補充停止は`replenish_suspensions`に永続化し、定期reconcileと補充ジョブの双方で参照する。
停止理由によらず、解除はそのworkspaceの手動起動（貸出・resume）の成功か`wx retry-standby`だけとし、既存sessionの返却では解除しない。

## clearとGC

`clean.go`は受付時点で全workspace・全root世代から対象を確定し、`clean_runs`・`clean_targets`へ対象と期限を永続化する。
`Manager.driveClean`はbackgroundで既存ジョブを監視し、workerを占有したまま別ジョブを待たない。
削除は通常の`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`へ載せる。

貸出前のREADY・補充中のPREPARINGは`--standby`と`--all`だけが対象に含め、隔離slotは全modeで`ScheduleQuarantinedRemoval`へ載せる。
`--discard`は保存を省略して削除を予約し、modeに永続化して再起動後も維持する。
実行中runへ合流できるのは対象範囲が同じmodeの再実行だけとする。
`--all`の終了要求は`session_termination_requests`へ期限付きで記録し、heartbeatとagent登録の応答でclientへ渡す。
signalを送るのはclientだけで、daemonは記録されたPIDへ触れない。
期限内に停止を確認できない対象は失敗として閉じ、遅れた終了は通常の返却へ戻す。

run実行中は`assertNoActiveClean`が貸出・復元・待機用作成の書き込みトランザクションを断り、対象が新しいsessionへ渡るのを防ぐ。
削除後に補充を停止するのは待機用slotを削除するmodeだけとする。
安全な処理境界の待機は`cleanBoundaryWait`で制限し、貸出を断ったまま無期限に待たない。
GC候補の選択と保持期限は`gc.go`を参照し、隔離slotも通常の`REMOVE`で登録範囲を回収する。

生きたclientもagentも持たない貸出（`wx new`）は終了要求の宛先がないため、`advancePending`は要求を積まずその場で返却して保存経路へ移す。
`--all`無しで残す場合のskip理由も、停止を待つ`--all`ではなく`wx release <id>`を案内する。
`wx shell` / `wx run`の強制停止は既存の`--all`経路で成立するので、`session_termination_requests`は貸出用に拡張しない。

## reconcileと障害時の運用

DBと実体を照合し、素性の分からないpath・refは隔離する。
clientとagentの両プロセスが死んだsessionは返却する。
返却の実装は`Manager.releaseLeaseWithoutToken`に集約し、orphan回収・期限掃引・親連動・`wx release`が共有する。
`Manager.reconcileExpiredLeases`は`maintainJobs`の10秒tickerと`maintainLifecycle`の起動時一巡に繋ぎ、`lease.ttl`の到来と親sessionの終了の両方をここで拾う。
`Manager.Release`の成功後にも子貸出の返却を呼ぶが、これは待ち時間の最適化であり、正しさの根拠は周期処理側に置く。
隔離slotを持つsessionは`DRAINING`へ進めず、`EXPIRED`で終端させslotのownerだけを外す。
slotは`QUARANTINED`のままworktree・snapshotを保持し、同じ返却の失敗が繰り返されるのを防ぐ。
この扱いは`Release`の全経路に適用する。
復旧snapshotを作らない返却は`Store.ReleaseWithOutcome`で区別してWarnへ記録する（clientはRelease応答を読まない）。

root世代登録が失敗するとallocationが`ErrOwnership`で落ち続けるため、周期処理はdescriptorを取り直して再登録を試みる。
これによりroot再作成やvolume再mountによる回復をdaemon再起動なしで拾い、同じ理由の連続失敗のログは1回に抑える。
使用量の測定契機とcacheは[使用量とCoWの観測](storage-usage.md)を参照する。
SQLiteを開けなくても`DegradedHandler`が`Status`・`Doctor`・`RequestStop`を受け付ける。
この場合は状態変更RPCの予約がないため、`RequestStop`はidleゲートを通さない。

通常準備の開始は`slots.preparation_started_at`、全先行配置の完了は`slots.early_ready_at`へSQL CASで記録する。
Early Readyの間もslotはPREPARINGであり、hookが使うWaitReadyは成功しない。
WaitEarlyReadyは認証と終端状態を検査し、過去の完了時刻だけで失敗・隔離・終了済みのsessionを起動しない。
二段階準備がdaemon crashなどで中断した場合は、部分checkoutや外部hookの完了を推測せず隔離し、自動で先頭から再実行しない。
正常な実行中のlock待ちは同じ実行を継続し、全準備がREADYへ到達済みのslotとrestoreの回復処理はこの隔離条件に含めない。
COLD repositoryの再補充へ貸し出す際は古い先行完了・開始記録を消し、新しい二巡を始める。
UPDATEも書込み開始時刻を永続化し、開始後の中断は隔離する。
全更新と配置履歴の確定後にだけslotをLEASEDへ移し、DB確定後にjob完了だけが中断した場合は更新を再実行しない。

## doctorの診断

`wx doctor`は検査ごとに種別つきのfinding（`internal/diag`の`Finding`）を返し、表示側は文面から重大さを判定しない。
通常表示はproblem（利用者の対処が必要）と、原因を表示していないunchecked（前提の故障で実施できず）だけを「エラー内容・対象・原因・対処方法」の形で出す。
`unchecked`は`DependsOn`に原因の検査名を持ち、その検査のproblemを表示済みなら通常表示から省く。
終了コードはproblemまたはuncheckedがあれば1とし、実施できなかった検査を成功として扱わない。
`--json`は`-v`によらず全findingを返す（消費側の契約なので、絞り込みを足すなら`state.JSONSchemaVersion`を上げる）。
daemonへ接続できない場合とdegradedの場合は、store依存の検査（`diag.StoreDependentChecks`）をこの形で並べ、同じ故障を検査ごとに繰り返さない。
`findings`を返せない古いdaemonの応答は正常と読ませず、CLIが`wx daemon restart`を促すproblemを足す。

daemon接続なしで成立する検査は[`internal/diag`](../internal/diag/diag.go)に置く。
storeを要する検査は[`doctor.go`](../internal/daemon/doctor.go)と[`doctor_recovery.go`](../internal/daemon/doctor_recovery.go)に置く。
worktree rootのpath検査と登録検査は別のfindingとして両方保持し、登録状態でpath検査の結果を上書きしない。
準備・保存・復元の失敗は、上位の処理名で言い換えず`jobs.error_message`・`error_detail_path`から具体的な失敗理由と詳細ログの場所まで引き継ぐ。
原因が記録されていない場合は特定できていないことを明示し、推測を原因として表示しない。

## 準備時間の計測

`wx bench`は貸出からEARLY READY・FULL READYまでをclient側で測り、daemonが記録した区間内訳を添えて出す。
区間はPREPAREジョブの実行中に`workspace.PhaseTimings`が集計し、`internal/daemon/measurement.go`が直近`prepareMeasurementHistory`件だけをdaemonのメモリに持つ。
計測は診断であって状態ではないので、`state.Store`にもスキーマにも入れない。daemon再起動で消えるのは仕様である。

区間名は準備の節目に対応する。
先行配置までが`git-register`・`early-index`・`early-checkout`・`early-place`・`early-root`・`early-ready`である。
以降は`checkout`・`submodule`・`post-checkout`・`place`・`link`・`prepare-command`・`tracked-status`・`cow`・`tracked-status-refresh`・`ready-lock`・`root`と続く。
`cow.compare`のようにドットを含む区間はCoW共有の並列worker間の合計で、`cow`区間の実時間を超えることがある。
`cow.entries`・`cow.candidates`・`cow.shared`・`cow.skipped_size`は時間ではなく件数として同じ表に載る。
区間の合計はEARLY/FULL READYと一致しない。所有権証明・キュー待ち・貸出解決のように計測していない時間が残るためである。

cold startを測るため、既定では対象workspaceの待機中READY slotを`RetireStandby`でSTALEにする。
実体は通常のGCが回収し、補充が作り直す。貸出中のslotには触れず、未登録のworkspaceは退役対象なしとして成功で返す。

## restart / stopのidleゲート

明示的なrestart/stopとバイナリ差し替えの自動検知はpendingを立て、同じidleゲートへ合流する。
`restartPending`・`stopPending`は`maintainJobs`から`runPendingLifecycle`が駆動し、in-flight RPCとjobsがともに0になってから実行する。
SIGTERMは応答前のRPC接続も閉じるため、このゲートで保護する。
複数sessionのheartbeatが位相をずらして続くと待機が解けなくなるため、RPC間のquiet periodは要求しない。
接続の隙間の置換は`rpc.Client.ConnectRetry`、状態変更RPCの再送は`CallWithKey`の冪等キーで扱う。

lifecycle要求もin-flightとして数えるが、応答の`inflight_requests`からは除き、待機要求自身を待機理由として表示しない。
応答フレームは`Handler.Handle`の後に書かれるため、`lifecycleReplyGrace`の間ゲートを閉じ、受理応答がsignalで切れるのを防ぐ。
restartは二重起動を避けるため`underLaunchd()`を要求し、stopは要求しない。

要求は互いを打ち消して同時にpendingにしないが、signal配送後の逆要求は`conflict`として断る。
`RequestStart`は起動済みdaemonにも送り、CLIの待機終了後に残ったstopを配送前なら取り消す。
配送後ならCLIは終了を待ってlaunchd経由で起動し直す。
ゲート通過後はrestartの`launchd.Kickstart`またはstopの自プロセスへのSIGTERMを1度だけ発行する。

CLIのstop/start待ちはsocketへのdialだけを使い、RPCでゲートを塞がない。
restartは要求応答のPIDを基準に、250ms間隔の`Ping`で応答元PIDが変わるまで待つ。
`Ping`にPIDがない旧daemonや`Ping`が未対応のdaemonでは、同じ確認予算内で`Status`へフォールバックする。
短いlistener断の観測は成功条件にせず、確認ごとのRPC予算は待機期限の残り時間で制限する。
LaunchAgentには`ThrottleInterval=1`を設定し、連続再起動時のlaunchdの待機を短くする。

## テストの並行化

`internal/daemon`のトップレベルテストは、専用の一時ディレクトリ・DB・Managerだけを使うものに`t.Parallel()`を付ける。
`t.Setenv`を自身かサブテストで呼ぶテスト、プロセス全体のgoroutine・fdを数えるテスト、短い待機に依存するテストは直列のまま残す。

## 変更の入口と代表テスト

GCの候補選択と削除の入口は[`internal/daemon/gc.go`](../internal/daemon/gc.go)、代表テストは[`TestGCRemovesRegisteredQuarantineWithoutCachedIdentity`](../internal/daemon/gc_integration_test.go)である。
restart/stopのidleゲートの入口は[`internal/daemon/restart.go`](../internal/daemon/restart.go)、代表テストは[`TestPendingRestartWaitsForJobsAndRequests`](../internal/daemon/restart_test.go)である。
準備時間の計測の入口はdaemon側が[`internal/daemon/measurement.go`](../internal/daemon/measurement.go)、client側が[`internal/cli/bench.go`](../internal/cli/bench.go)である。
代表テストは[`TestPrepareMeasurementRecordsPhasesOfARealPreparation`](../internal/daemon/measurement_test.go)で、実際の準備が区間内訳を残すことを通す。
