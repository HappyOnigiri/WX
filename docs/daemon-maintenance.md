# daemonの補充・回収・再起動

ジョブ配送は`internal/daemon/jobs.go`、実行枠は`jobqueue.go`、周期処理は`maintenance.go`、実体照合は`reconcile.go`を参照する。
準備・復元の所有権失敗は終端させ、削除はDB登録済みの範囲を回収する。

## ジョブの分類と実行枠

実行枠は利用者向けと保守用に分かれる。
利用者向けの枠数は`pool.preparation_concurrency`（既定2）、保守用は常に2本で、保守へ利用者向けの枠を貸さない。
`preparation_concurrency: 1`でも保守と利用者処理はそれぞれ枠を持ち、資源上限は既定で4本同時になる。
保守用が1本だと、大きいrepositoryの補充・回収が数十秒単位で枠を占め、他workspaceの補充がその後ろで待ってREADYが枯れる。
2本にする根拠は、律速がディスク帯域ではなく枠数であること（同じrepositoryのPREPAREを並列に流すと壁時計が縮む）である。
利用者向けが満杯のときの待ちと、保守が並走する間に利用者向けの処理が遅くなる分は残る。

分類は`jobClassOf`が job rowの事実だけから決め、DBへ永続化しない。
session付きPREPARE・RESTORE・SNAPSHOTを利用者向けとし、SNAPSHOTは保存と将来のresumeの前提なので利用者が明示的に待っているかによらずこのクラスに置く。
待機用PREPARE・ENSURE_STANDBY・自動REMOVE系と、sessionを持たないUPDATE（待機中standbyのidle更新）は保守用とする。
実行中のclean runが完了を待つREMOVEだけを`advanceRemoving`が毎回の監視で利用者向けへ昇格させる。

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
削除中の`REMOVING`も`READY`へ戻らないため数えない。数えると返却直後の枠が削除の完了まで埋まり、その間に走った補充の確認が不足なしと判断して、次のreconcileまで待機枠が欠ける。
削除の完了時は`FinishRemoval`が補充の再確認を同じtransactionで予約する。`ENSURE_STANDBY`が既にPENDING・RUNNINGなら積み増さず、clean実行中は予約しない。
COLD化の`RETIRING`は完了後に`READY`へ戻るので枠に数える。

個数を増やした設定の反映は次の保守一巡で不足分を補充する。減らした場合は準備中の処理を中断せず、完了後に余剰のREADY slotを既存GCが回収する。
貸出中slotは回収せず、保持期間によるCOLD化もworkspaceごとの実効値が正のときだけ行う。

再利用が有効な定期reconcileはREADY slotを保存済みOIDと更新互換fingerprintで検証し、現在のmainとの差だけではSTALEにしない。
配置履歴を持たないREADY slotは更新に使えないため、この検証の対象から外し、現在のmainと完全一致でなければSTALEにする。
貸出時に更新不適格と判定した候補もSTALEにして回収・補充へ回す。残しても毎回Cold Startになる一方で待機枠を占有し続けるためである。
`--branch`指定の貸出では回収しない。main向けのstandbyをbranch要求のために捨てないためである。
OIDと配置の更新は貸出要求時と保守一巡のidle更新で行い、その時点のOID・配置計画・copy modeをDBへ固定する。
貸出要求のUPDATEは利用者向け実行枠を使い、slot・STARTING session・jobの予約を同じtransactionで確定する。

idle更新（`refreshIdleStandbys`）は、完全一致しないが更新適合なREADY standbyを貸出を待たずに現在の要求へ合わせる。
`.worktreeinclude`対象の書き換えのようにfingerprintだけがずれた待機枠を残すと、次の貸出がUPDATEの待ちを払い、`wx status`のREADYも実態とずれるためである。
予約（`ReserveIdleStandbyUpdate`）はsessionを作らず`owner_session_id`を空のままPREPARINGへ移すので、更新中のslotは貸出候補から外れ、併走する貸出予約とは`slots`のcompare-and-swapで排他になる。
jobはsessionを持たないため保守用の実行枠で走り、利用者向けの枠を奪わない。
歯止めは3つで、1巡につき1件だけ始める、待機枠が全てREADYに落ち着いたworkspaceだけを対象にする、workspaceごとに`idleStandbyRefreshCooldown`（1分）の間隔を空ける。
更新中はそのworkspaceのREADYが一時的に1本減るため、貸出が進行中のworkspaceでは始めない。
完了は`FinishIdleStandbyUpdate`がREADYへ戻し、書込み開始後の中断は貸出付きの更新と同じく隔離する（自動再実行はしない）。
更新に使えない候補はidle更新では回収せず、READYのまま残して貸出時の判断に委ねる。

補充停止は`replenish_suspensions`に永続化し、定期reconcileと補充ジョブの双方で参照する。
停止理由によらず、解除はそのworkspaceの手動起動（貸出・resume）の成功か`wx retry-standby`だけとし、既存sessionの返却では解除しない。

## clearとGC

`clean.go`は受付時点で全workspace・全root世代から対象を確定し、`clean_runs`・`clean_targets`へ対象と期限を永続化する。
`Manager.driveClean`はbackgroundで既存ジョブを監視し、workerを占有したまま別ジョブを待たない。
削除は通常の`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`へ載せる。

貸出前のREADY・補充中のPREPARINGは`--standby`と`--all`だけが対象に含め、隔離slotは全modeで`ScheduleQuarantinedRemoval`へ載せる。
`--discard`は保存を省略して削除を予約し、modeに永続化して再起動後も維持する。
使用中のdetached lease（`wx new`）も、返却と同じtransactionでSNAPSHOTを積まずREMOVEへ載せ、保存待ちを経ずに削除待ちへ進める。
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

記録したrecovery refがソースリポジトリに無いとき（リポジトリを消して同じpathに作り直した場合）は、`QuarantineMissingRecoveryRef`がsnapshot・session・slotを隔離する。
この隔離からの出口は`wx discard-recovery <workspace-path>`だけで、GCもreconcileも隔離したsnapshotを自動では捨てない。
`Manager.DiscardRecovery`が対象workspaceの`QUARANTINED`なsessionについてsnapshot行と`workspace_snapshots`行を消し、sessionを`EXPIRED`へ進め、そのsessionのslotを`ScheduleQuarantinedRemoval`で回収する。
refが無いsnapshotからは復元できないため失う復元手段は無いが、slotのworktreeにある未保存の作業は消えるので、`--dry-run`で対象とpathを出せるようにしている。
これを経ないと`wx forget`の前提（sessionは`EXPIRED`、snapshot行は無し、slotは`ARCHIVED`）を永久に満たせない。
隔離するとref照合の期待一覧（`sn.status='ARCHIVED'`だけを見る）から外れて他のfindingが消えるため、行き止まり自体は`Manager.quarantinedRecoveryFindings`がworkspace単位のproblemとして報告する。
復旧snapshotを作らない返却は`Store.ReleaseWithOutcome`で区別してWarnへ記録する（clientはRelease応答を読まない）。

root世代登録が失敗するとallocationが`ErrOwnership`で落ち続けるため、周期処理はdescriptorを取り直して再登録を試みる。
これによりroot再作成やvolume再mountによる回復をdaemon再起動なしで拾い、同じ理由の連続失敗のログは1回に抑える。
使用量の測定契機とcacheは[使用量とCoWの観測](storage-usage.md)を参照する。
SQLiteを開けなくても`DegradedHandler`が`Status`・`Doctor`・`RequestStop`を受け付ける。
この場合は状態変更RPCの予約がないため、`RequestStop`はidleゲートを通さない。

通常準備の開始は`slots.preparation_started_at`、全先行配置の完了は`slots.early_ready_at`へSQL CASで記録する。
Early Readyの間もslotはPREPARINGであり、hookが使うWaitReadyは成功しない。
WaitEarlyReadyは認証と終端状態を検査し、過去の完了時刻だけで失敗・隔離・終了済みのsessionを起動しない。
二段階準備がdaemon crashなどで中断した場合は、部分checkoutや外部hookの完了を推測せず、自動で先頭から再実行しない。
貸出先sessionを持たない待機枠は`STALE`にしてGCの回収と補充へ回し、隔離して残さない。待機枠には利用者の作業が無いためである。
貸出先sessionを持つslotは利用者が結果を待っているので、黙って作り直さず従来どおり隔離する。
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

`--probe`は静的検査の後に、登録済みworkspaceを1つずつ実際に貸し出して準備し、貸出中のworktreeを読み取りだけで検査してから保存せずに返す。
駆動はCLI（[`internal/cli/doctor_probe.go`](../internal/cli/doctor_probe.go)）が`wx bench`と同じRPC列で行い、daemonにprobe専用の処理を持たない。
検査結果は他と同じ`Finding`としてfindingsへ合流させ、所要時間とディスク使用量は失敗ではないので`Reply.Probes`へ分けて出す。
`--probe`は対象workspaceの待機standbyを`RetireStandby`でSTALEにするので、実行中と直後はそのworkspaceの起動が遅くなる。

daemon接続なしで成立する検査は[`internal/diag`](../internal/diag/diag.go)に置く。
storeを要する検査は[`doctor.go`](../internal/daemon/doctor.go)と[`doctor_recovery.go`](../internal/daemon/doctor_recovery.go)に置く。
worktree rootのpath検査と登録検査は別のfindingとして両方保持し、登録状態でpath検査の結果を上書きしない。
登録済みworkspaceに属さず照合すべきsnapshotも持たないrepository記録は、refsを読めなくてもproblemにせずinfoに留める。
`repositories`の行を消す経路が無いため、forget後に残った記録をerrorにするとdoctorが恒久的に失敗する。
登録済みworkspaceに属する repository の故障はこれまでどおりerrorとして報告する。
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

使用量は返却の直前に`wx slots`から引く。使用量の測定はslotが再び準備へ入っても即座には消えないため、貸出を要求した時刻より前の`measured_at`は前の準備の値として採らず、次の測定を待つ。

cold startを測るため、既定では対象workspaceの待機中READY slotを`RetireStandby`でSTALEにする。
実体は通常のGCが回収し、補充が作り直す。貸出中のslotには触れず、未登録のworkspaceは退役対象なしとして成功で返す。

失敗せずに出力だけを残した区間は`workspace.PrepareNotices`が集め、計測と同じ経路で`PrepareTimings`から引ける。
exit 0のpost-checkout hookが内部の失敗を飲み込んでも、出力を捨てるとwxからは正常と区別できないためである。
本文はdaemon logへwarnで出し、全文は失敗のstderrと同じ`<logdir>/details/<id>.log`へ書く。

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
待機中standbyのidle更新の入口は[`internal/daemon/standby_idle_update.go`](../internal/daemon/standby_idle_update.go)である。
代表テストは[`TestIdleStandbyRefreshUpdatesMismatchedReadyBeforeLease`](../internal/daemon/standby_idle_update_test.go)で、貸出前にREADYが現在のmainへ揃うことを通す。
準備時間の計測の入口はdaemon側が[`internal/daemon/measurement.go`](../internal/daemon/measurement.go)、client側が[`internal/cli/bench.go`](../internal/cli/bench.go)である。
代表テストは[`TestPrepareMeasurementRecordsPhasesOfARealPreparation`](../internal/daemon/measurement_test.go)で、実際の準備が区間内訳を残すことを通す。
