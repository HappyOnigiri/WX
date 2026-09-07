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
キュー待ち時間と実行時間は`job attempt finished`のログで別々に記録する。

## slot排他と共通ロック

同じslotへ書く準備・復元・保存・削除は、`roots.id`とroot相対pathをkeyにした`gitx.KeyedLocks`（daemonの`slotLocks`）で直列化する。
作成前後で変わるinodeはkeyに含めず、所有権検証の入力としてだけ使う。
slot lockは最上位のoperationで一度だけ取る。
入口は`Preparer.Prepare`・`archive.Restore`・`archive.SnapshotWithPersistence`・`archive.RemoveWorktree`で、内側の経路には取得済みのcontextを渡して取り直させない。

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

`standby.go`は`hot`なworkspaceのREADY slotを`pool.warm_per_workspace`まで補充する。
`Store.HotRepositoryIDs`は`repositories.last_leased_at`で絞るが、貸出時の更新はworkspace単位なので、直後の補充では全リポジトリがhotになる。
リポジトリごとの利用に絞るなら、`session_repositories`へ実利用を記録し、`HotRepositoryIDs`とGCの`ColdRepositoryCandidates`をともに変更する必要がある。

リトライ中の`FAILED` slotは待機枠に数える。
通常sessionの準備成功または検証済みREADY slotの貸出時に、その成功に紐付く除外記録で補充数の計算から外す。
除外記録はslotの状態や実体を変更せず、同じ成功の再処理で後発の失敗slotまで除外しない。
復元成功や`SessionStart`による`ACTIVE`遷移だけでは除外記録を作らず、補充の契機にもならない。
`QUARANTINED`は待機枠に数えないが、待機用PREPAREの失敗後は補充を停止することでGCとの作成・削除ループを防ぐ。

補充停止は`replenish_suspensions`に永続化し、定期reconcileと補充ジョブの双方で参照する。
`reason`はclear由来の`CLEAN`または`STANDBY_PREPARE_FAILED`、`detail`はclean runまたは失敗jobのIDを持つ。
停止理由によらず、解除はそのworkspaceの手動起動（貸出・resume）の成功か`wx retry-standby`だけとし、既存sessionの返却では解除しない。

## clearとGC

`clean.go`は受付時点で全workspace・全root世代から対象を確定し、`clean_runs`・`clean_targets`へ対象と期限を永続化する。
`Manager.driveClean`はbackgroundで既存ジョブを監視し、workerを占有したまま別ジョブを待たない。
削除は通常の`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`へ載せる。

貸出前のREADY・補充中のPREPARINGは`--standby`と`--all`だけが対象に含め、隔離slotは全modeで`ScheduleQuarantinedRemoval`へ載せる。
`--discard`は保存を省略して削除を予約し、modeに永続化して再起動後も維持する。
登録外のpathは削除せず、登録済みslotのinode・marker・HEADの不一致は回収を妨げない。
実行中runへ合流できるのは対象範囲が同じmodeの再実行だけとする。
`--all`の終了要求は`session_termination_requests`へ期限付きで記録し、heartbeatとagent登録の応答でclientへ渡す。
signalを送るのはclientだけで、daemonは記録されたPIDへ触れない。
期限内に停止を確認できない対象は失敗として閉じ、遅れた終了は通常の返却へ戻す。

run実行中は`assertNoActiveClean`が貸出・復元・待機用作成の書き込みトランザクションを断り、対象が新しいsessionへ渡るのを防ぐ。
削除後に補充を停止するのは待機用slotを削除するmodeだけとする。
安全な処理境界の待機は`cleanBoundaryWait`で制限し、貸出を断ったまま無期限に待たない。
GC候補の選択と保持期限は`gc.go`を参照し、隔離slotも通常の`REMOVE`で登録範囲を回収する。

## reconcileと障害時の運用

DBと実体を照合し、素性の分からないpath・refは隔離する。
clientとagentの両プロセスが死んだsessionは返却する。
隔離slotを持つsessionは`DRAINING`へ進めず、`EXPIRED`で終端させslotのownerだけを外す。
slotは`QUARANTINED`のままworktree・snapshotを保持し、同じ返却の失敗が繰り返されるのを防ぐ。
この扱いは`Release`の全経路に適用する。
復旧snapshotを作らない返却は`Store.ReleaseWithOutcome`で区別してWarnへ記録する（clientはRelease応答を読まない）。

root世代登録が失敗するとallocationが`ErrOwnership`で落ち続けるため、周期処理はdescriptorを取り直して再登録を試みる。
これによりroot再作成やvolume再mountによる回復をdaemon再起動なしで拾い、同じ理由の連続失敗のログは1回に抑える。
使用量の測定契機とcacheは[使用量とCoWの観測](storage-usage.md)を参照する。
SQLiteを開けなくても`DegradedHandler`が`Status`・`Doctor`・`RequestStop`を受け付ける。
この場合は状態変更RPCの予約がないため、`RequestStop`はidleゲートを通さない。

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
restartはlistener消失後と待機期限後に`Status`のpidを読み、置換前後のpidで判定する。
短いlistener断はプローブが取りこぼすため、接続断の観測だけでは置換を判断できない。
待機表示は`interactiveOutput`でstdoutが端末のときだけ出す。

## 変更の入口と代表テスト

GCの候補選択と削除の入口は[`internal/daemon/gc.go`](../internal/daemon/gc.go)である。
代表テストは`internal/daemon`の[`TestGCRemovesRegisteredQuarantineWithoutCachedIdentity`](../internal/daemon/gc_integration_test.go)である。
restart/stopのidleゲートの入口は[`internal/daemon/restart.go`](../internal/daemon/restart.go)である。
代表テストは同パッケージの[`TestPendingRestartWaitsForJobsAndRequests`](../internal/daemon/restart_test.go)である。
どちらも`make test-focus PKG=./internal/daemon RUN=<テスト名>`で絞って動かせる。
この実行は[部分検証](worktree-copy.md#部分検証)であり、最終判定は`make ci`とする。
