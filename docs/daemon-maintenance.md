# daemonの補充・回収・再起動

準備・復元の所有権失敗は終端させ、削除はDB登録済みの範囲を回収する。

## ジョブの分類と実行枠

実行枠は利用者向け（`pool.preparation_concurrency`）と保守用（`maintenanceJobSlots`）に分かれ、保守へ利用者向けの枠を貸さない。
どちらも他方の待ちで飢えないことを優先し、利用者向けが満杯のときの待ちと、保守が並走する間に利用者向けの処理が遅くなる分は受け入れる。

分類は`jobClassOf`が job rowの事実だけから決め、DBへ永続化しない。
実行中のclean runが完了を待つREMOVEだけを`advanceRemoving`が毎回の監視で利用者向けへ昇格させる。

`dispatchJobs`はクラス別の待ち行列から到着順に配り、枠を取ってから`ClaimJob`する。
このためキュー待ちのジョブはattemptもjob leaseも消費せず、同じジョブIDの二重登録も配送前に落とす。
待ち行列の上限を超えた分は`PENDING`のdurable jobとして残し、周期的なジョブ回収が拾う。
実行枠はジョブのgoroutineと分けてあり、実行中のコピーやprepare commandは優先度の変更でも枠の縮小でも中断しない。

## slot排他と共通ロック

同じslotへ書く準備・復元・保存・削除は、`roots.id`とroot相対pathをkeyにしたkeyed lock（`gitx.KeyedLocks`、daemonの`slotLocks`）で直列化する。
作成前後で変わるinodeはkeyに含めず、所有権検証の入力としてだけ使う。
slot lockは最上位のoperationで一度だけ取り、内側の経路には取得済みのcontextを渡して取り直させない。
二段階準備では二巡全体で保持し続ける。

リポジトリ共有のGit管理情報は`common directory`をkeyにした同じ仕組みで排他する。
`prepare`はworktreeの作成とlock reasonの確立まで、そして`READY`へ移す最後の区間だけこのロックを保持し、その間のコピー・link・prepare commandは保持せずに行う。
このため同じリポジトリの別slotは、先行slotのコピーやprepare commandの完了を待たずに準備できる。
ロックを取り直す区間の入口では、DB状態・root/path/marker/inode・Git登録・OID・lock reasonを検証し直す。

取得順序はslot、common directory、実行枠に統一する。
どちらのロックも待機に入る前に実行枠を返し、ロックを取得してから枠を取り直すので、Git管理操作を待つだけのジョブが無関係なリポジトリの枠を占有しない。
利用者向けの枠が1本でも、枠の取り直しがロック取得後であるため循環待ちにならない。
待機中もジョブの処理位置とlease更新は保たれ、ジョブ先頭からのretryにはならない。

## standby補充

`hot`なworkspaceのREADY slotを待機枠数まで補充する。
枠数は`workspaces.<root>.warm_count`、`pool.warm_per_workspace`の順に継承する。
workspace個別値は単一リポジトリではそのリポジトリのmain worktree、multi-repositoryではworkspace rootに適用する。
枠数0はそのworkspaceの補充を無効にするが、個数指定だけで`hot`へは変更しない。

`Store.HotRepositoryIDs`は`repositories.last_leased_at`で絞るが、貸出時の更新はworkspace単位なので、直後の補充では全リポジトリがhotになる。
リポジトリごとの利用に絞るなら、`session_repositories`への実利用の記録と、`HotRepositoryIDs`・GCの`ColdRepositoryCandidates`の変更が対になる。

### 待機枠の数え方

リトライ中の`FAILED` slotは待機枠に数える。
通常sessionの準備成功または検証済みREADY slotの貸出時に、その成功に紐付く除外記録で補充数の計算から外す。
除外記録はslotの状態や実体を変更せず、同じ成功の再処理で後発の失敗slotまで除外しない。
復元成功や`SessionStart`による`ACTIVE`遷移だけでは除外記録を作らず、補充の契機にもならない。

`QUARANTINED`は待機枠に数えないが、待機用PREPAREの失敗後は補充を停止することでGCとの作成・削除ループを防ぐ。
削除中の`REMOVING`も`READY`へ戻らないため数えない。
数えると返却直後の枠が削除の完了まで埋まり、その間に走った補充の確認が不足なしと判断して、次のreconcileまで待機枠が欠ける。
代わりに削除の完了時は`FinishRemoval`が補充の再確認を同じtransactionで予約する。
COLD化の`RETIRING`は完了後に`READY`へ戻るので枠に数える。

枠数を増やした設定の反映は次の保守一巡で不足分を補充する。減らした場合は準備中の処理を中断せず、完了後に余剰のREADY slotを既存GCが回収する。
貸出中slotは回収せず、保持期間によるCOLD化もworkspaceごとの実効値が正のときだけ行う。

### 再利用とSTALE化

再利用は既定で有効（`worktree.reuse_standby`、workspace個別値で上書き可）で、無効にするとOID・fingerprint完全一致だけを貸す動作になる。
有効な場合の定期reconcileはREADY slotを保存済みOIDと更新互換fingerprintで検証し、現在のmainとの差だけではSTALEにしない。
配置履歴を持たないREADY slotは更新に使えないため、この検証の対象から外し、現在のmainと完全一致でなければSTALEにする。

貸出時に更新不適格と判定した候補もSTALEにして回収・補充へ回す。残しても毎回Cold Startになる一方で待機枠を占有し続けるためである。
worktreeにtracked変更が残っていて棄却した候補も同じ扱いにする。次の貸出でも同じ理由で棄却されるので、定期reconcileを待つ間だけREADYの見かけと実態がずれるためである。
再試行で解消し得る理由（併走する遷移に負けた、Gitやファイル操作が失敗した）はSTALEにせず、候補を飛ばすだけにとどめる。
`--branch`指定の貸出では回収しない。main向けのstandbyをbranch要求のために捨てないためである。

OIDと配置の更新は貸出要求時と保守一巡のidle更新で行い、その時点のOID・配置計画・copy modeをDBへ固定する。
貸出要求のUPDATEは利用者向け実行枠を使い、slot・STARTING session・jobの予約を同じtransactionで確定する。

### idle更新

idle更新は、完全一致しないが更新適合なREADY standbyを貸出を待たずに現在の要求へ合わせる。
`.worktreeinclude`対象の書き換えのようにfingerprintだけがずれた待機枠を残すと、次の貸出がUPDATEの待ちを払い、`wx status`のREADYも実態とずれるためである。
予約（`ReserveIdleStandbyUpdate`）はsessionを作らず`owner_session_id`を空のままPREPARINGへ移すので、更新中のslotは貸出候補から外れ、併走する貸出予約とは`slots`のcompare-and-swapで排他になる。
jobはsessionを持たないため保守用の実行枠で走り、利用者向けの枠を奪わない。

歯止めは3つで、1巡につき1件だけ始める、待機枠が全てREADYに落ち着いたworkspaceだけを対象にする、workspaceごとに一定のcooldownを空ける。
更新中はそのworkspaceのREADYが一時的に1本減るため、貸出が進行中のworkspaceでは始めない。
完了は`FinishIdleStandbyUpdate`がREADYへ戻し、書込み開始後の中断は貸出付きの更新と同じく隔離する（自動再実行はしない）。
更新に使えない候補はidle更新では回収せず、READYのまま残して貸出時の判断に委ねる。

入口は[`internal/daemon/standby_idle_update.go`](../internal/daemon/standby_idle_update.go)である。
貸出前にREADYが現在のmainへ揃うことは[`TestIdleStandbyRefreshUpdatesMismatchedReadyBeforeLease`](../internal/daemon/standby_idle_update_test.go)が固定している。

### 補充停止

補充停止は`replenish_suspensions`に永続化し、定期reconcileと補充ジョブの双方で参照する。
停止理由によらず、解除はそのworkspaceの手動起動（貸出・resume）の成功か`wx retry-standby`だけとし、既存sessionの返却では解除しない。
`wx clear --all`は補充が有効な全workspaceを一度に止めるため、`wx retry-standby --all`で停止行のある全workspaceをまとめて戻せる。
準備に失敗したFAILED slotは待機枠に数えるので、解除しただけでは不足が0のままになる。`wx retry-standby`は補充を予約する前にFAILED slotを削除予約へ載せ、REMOVINGへ移してから枠を数え直させる。

## clearとGC

`clean.go`は受付時点で全workspace・全root世代から対象を確定し、対象と期限を永続化する。
`Manager.driveClean`はbackgroundで既存ジョブを監視し、workerを占有したまま別ジョブを待たない。
削除は通常の`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`へ載せる。
隔離slotはmodeによらず`ScheduleQuarantinedRemoval`へ載せる。
`--discard`は保存を省略して削除を予約し、modeに永続化して再起動後も維持する。
使用中のdetached lease（`wx new`）も、返却と同じtransactionでSNAPSHOTを積まずREMOVEへ載せ、保存待ちを経ずに削除待ちへ進める。
実行中runへ合流できるのは対象範囲が同じmodeの再実行だけとする。

終了要求は`--all`だけが`session_termination_requests`へ期限付きで記録し、heartbeatとagent登録の応答でclientへ渡す。
signalを送るのはclientだけで、daemonは記録されたPIDへ触れない。
期限内に停止を確認できない対象は失敗として閉じ、遅れた終了は通常の返却へ戻す。
生きたclientもagentも持たない貸出（`wx new`）は終了要求の宛先がないため、`advancePending`は要求を積まずその場で返却して保存経路へ移す。
`--all`無しで残す場合のskip理由も、停止を待つ`--all`ではなく`wx release <id>`を案内する。
`wx shell` / `wx run`の強制停止は既存の`--all`経路で成立するので、`session_termination_requests`は貸出用に拡張しない。

run実行中は`assertNoActiveClean`が貸出・復元・待機用作成の書き込みトランザクションを断り、対象が新しいsessionへ渡るのを防ぐ。
待機用slotを対象に含めるのは`--standby`と`--all`だけで、削除後に補充を停止するのもその範囲に限る。
安全な処理境界の待機は`cleanBoundaryWait`で制限し、貸出を断ったまま無期限に待たない。

GCの候補選択と削除の入口は[`internal/daemon/gc.go`](../internal/daemon/gc.go)で、隔離slotも通常の`REMOVE`で登録範囲を回収する。
登録だけを根拠に隔離slotを回収することは[`TestGCRemovesRegisteredQuarantineWithoutCachedIdentity`](../internal/daemon/gc_integration_test.go)が固定している。

### 忘れたrepository記録の回収

`wx forget`は`workspaces`行と同じtransactionで、どの登録からも参照されなくなった`repositories`行を消す。
以前の版が残した記録はGCの`PruneRepositories`が同じ条件で回収する（`wx gc`と保守一巡の両方で走り、dry-runでは何も消さない）。
消してよいのは、`workspace_repositories`にもsnapshotにも現れず、参照する slot が全て`ARCHIVED`、session が全て`EXPIRED`で、
どちらもworkspace紐付けを失っている場合だけである。その組み合わせでは`ValidateWorktreeOwnership`がworkspace linkを欠いて必ず失敗し、
履歴の`slot_repositories`・`session_repositories`行を残しても証明には使えない。Git リポジトリの実体には触れない。

## reconcileと障害時の運用

DBと実体を照合し、素性の分からないpath・refは隔離する。
clientとagentの両プロセスが死んだsessionは返却する。
返却の実装は`Manager.releaseLeaseWithoutToken`に集約し、orphan回収・期限掃引・親連動・`wx release`が共有する。
`Manager.reconcileExpiredLeases`は周期処理と起動時一巡の両方に繋ぎ、`lease.ttl`の到来と親sessionの終了をここで拾う。
`Manager.Release`の成功後にも子貸出の返却を呼ぶが、これは待ち時間の最適化であり、正しさの根拠は周期処理側に置く。

隔離slotを持つsessionは`DRAINING`へ進めず、`EXPIRED`で終端させslotのownerだけを外す。
slotは`QUARANTINED`のままworktree・snapshotを保持し、同じ返却の失敗が繰り返されるのを防ぐ。
この扱いは`Release`の全経路に適用する。

### recovery refを失ったworkspace

記録したrecovery refがソースリポジトリに無いとき（リポジトリを消して同じpathに作り直した場合）は、`QuarantineMissingRecoveryRef`がsnapshot・session・slotを隔離する。
この隔離からの出口は`wx discard-recovery <workspace-path>`だけで、GCもreconcileも隔離したsnapshotを自動では捨てない。
refが無いsnapshotからは復元できないため`Manager.DiscardRecovery`で失う復元手段は無いが、slotのworktreeにある未保存の作業は消えるので、`--dry-run`で対象とpathを出せるようにしている。
これを経ないと`wx forget`の前提（sessionは`EXPIRED`、snapshot行は無し、slotは`ARCHIVED`）を永久に満たせない。
隔離するとref照合の期待一覧（`sn.status='ARCHIVED'`だけを見る）から外れて他のfindingが消えるため、行き止まり自体は`Manager.quarantinedRecoveryFindings`がworkspace単位のproblemとして報告する。
復旧snapshotを作らない返却は`Store.ReleaseWithOutcome`で区別してWarnへ記録する（clientはRelease応答を読まない）。

### root世代とdegraded

root世代登録が失敗するとallocationが`ErrOwnership`で落ち続けるため、周期処理はdescriptorを取り直して再登録を試みる。
これによりroot再作成やvolume再mountによる回復をdaemon再起動なしで拾い、同じ理由の連続失敗のログは1回に抑える。
使用量の測定契機とcacheは[使用量とCoWの観測](storage-usage.md)を参照する。
SQLiteを開けなくても`DegradedHandler`が`Status`・`Doctor`・`RequestStop`を受け付ける。
この場合は状態変更RPCの予約がないため、`RequestStop`はidleゲートを通さない。

### 中断した準備の扱い

通常準備の開始と全先行配置の完了は`slots`へSQL CASで記録する。
Early Readyの間もslotはPREPARINGであり、hookが使うWaitReadyは成功しない。
WaitEarlyReadyは認証と終端状態を検査し、過去の完了時刻だけで失敗・隔離・終了済みのsessionを起動しない。

二段階準備がdaemon crashなどで中断した場合は、部分checkoutや外部hookの完了を推測せず、自動で先頭から再実行しない。
貸出先sessionを持たない待機枠は`STALE`にしてGCの回収と補充へ回し、隔離して残さない。待機枠には利用者の作業が無いためである。
貸出先sessionを持つslotは利用者が結果を待っているので、黙って作り直さず隔離する。
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

daemon接続なしで成立する検査は[`internal/diag`](../internal/diag/diag.go)に置き、storeを要する検査はdaemon側に残す。
worktree rootのpath検査と登録検査は別のfindingとして両方保持し、登録状態でpath検査の結果を上書きしない。
登録済みworkspaceに属さず照合すべきsnapshotも持たないrepository記録は、refsを読めなくてもproblemにせずinfoに留める。
この記録はGCの`PruneRepositories`が回収するまでの一時的なもので、errorにするとその間doctorが失敗し続ける。
登録済みworkspaceに属する repository でrefsを読めない場合は、その`repositories`行1件のproblemとして対象pathつきで報告し、
他のrepositoryの照合と他の検査は続ける（1件の失敗を検査全体のuncheckedにしない）。
準備・保存・復元の失敗は、上位の処理名で言い換えず`jobs.error_message`・`error_detail_path`から具体的な失敗理由と詳細ログの場所まで引き継ぐ。
原因が記録されていない場合は特定できていないことを明示し、推測を原因として表示しない。

### 未解消かどうかの判定

判定の置き場所は失敗の種類ごとに違う。
SNAPSHOTの失敗は保存対象のsession自身で判定し、後続のSNAPSHOTが成功・実行待ちならそこで解消とする。

RESTOREの失敗は復元先sessionの状態では判定しない。
復元先は失敗後にEXPIREDへ落ちるため、その条件では復元できていない状態がすべて解消済みに見える。
代わりに復元元（`parent_session_id`）がARCHIVEDのまま、同じ元sessionへの後続RESTOREが成功・実行待ちのどちらでもないことを未解消の条件にする。
元sessionは復元が成功して初めてEXPIREDになり、隔離slotを残した失敗も復元できていない事実は変わらないので除かない。

補充計画（`ENSURE_STANDBY`）の失敗は`replenish_suspensions`に停止を残さない。
manifestの不正のようにslotを作る前で落ちる失敗は補充を止めず、次の貸出と保守tickで同じ失敗を繰り返すためである。
これを見落とさないよう、workspaceごとの最新の失敗した`ENSURE_STANDBY`を`standby_replenishment`の検査へ停止と同じ列で載せ、`wx status`にも注記として出す。
判定は最新の失敗であることと待機枠が今も足りないことの両方で行い、後続の計画が枠を満たしていれば残ったFAILED行は報告しない。

### --probe

`--probe`は実際に貸し出して準備する動的検査で、駆動はCLIが`wx bench`と同じRPC列で行い、daemonにprobe専用の処理を持たない。
検査結果は他と同じ`Finding`としてfindingsへ合流させ、所要時間とディスク使用量は失敗ではないので`Reply.Probes`へ分けて出す。
benchと同じstandby退役を必ず伴うため、実行中と直後は対象workspaceの起動が遅くなる。

## 準備時間の計測

`wx bench`は貸出からEARLY READY・FULL READYまでをclient側で測り、daemonが記録した区間内訳を添えて出す。
区間はPREPAREジョブの実行中に`workspace.PhaseTimings`が集計し、`internal/daemon/measurement.go`がdaemonのメモリに直近の一定件数だけを持つ。
計測は診断であって状態ではないので、`state.Store`にもスキーマにも入れない。daemon再起動で消えるのは仕様である。

ドットを含む区間名はCoW共有の並列worker間の合計で、親区間の実時間を超えることがある。
区間の合計はEARLY/FULL READYと一致しない。所有権証明・キュー待ち・貸出解決のように計測していない時間が残るためである。

使用量は返却の直前に`wx slots`から引き、貸出を要求した時刻より前の`measured_at`は前の準備の値として採らず次の測定を待つ（測定契機は[使用量とCoWの観測](storage-usage.md)）。

cold startを測るため、既定では対象workspaceの待機中READY slotを`RetireStandby`でSTALEにする。
実体は通常のGCが回収し、補充が作り直す（貸出中のslotには触れない）。

失敗せずに出力だけを残した区間は`workspace.PrepareNotices`が集め、計測と同じ経路で引ける。
exit 0のpost-checkout hookが内部の失敗を飲み込んでも、出力を捨てるとwxからは正常と区別できないためである。
本文はdaemon logへwarnで出し、全文は失敗のstderrと同じ詳細ログへ書く。

## restart / stopのidleゲート

明示的なrestart/stopとバイナリ差し替えの自動検知はpendingを立て、同じidleゲートへ合流する。
`runPendingLifecycle`が周期処理から駆動し、in-flight RPCとjobsがともに0になってから実行する。
SIGTERMは応答前のRPC接続も閉じるため、このゲートで保護する。
複数sessionのheartbeatが位相をずらして続くと待機が解けなくなるため、RPC間のquiet periodは要求しない。
接続の隙間の置換は`rpc.Client.ConnectRetry`、状態変更RPCの再送は`CallWithKey`の冪等キーで扱う。

lifecycle要求もin-flightとして数えるが、応答の`inflight_requests`からは除き、待機要求自身を待機理由として表示しない。
応答フレームは`Handler.Handle`の後に書かれるため、応答直後の短い猶予の間ゲートを閉じ、受理応答がsignalで切れるのを防ぐ。
restartは二重起動を避けるため`underLaunchd()`を要求し、stopは要求しない。

要求は互いを打ち消して同時にpendingにしないが、signal配送後の逆要求は`conflict`として断る。
`RequestStart`は起動済みdaemonにも送り、CLIの待機終了後に残ったstopを配送前なら取り消す。
配送後ならCLIは終了を待ってlaunchd経由で起動し直す。
ゲート通過後はrestartの`launchd.Kickstart`またはstopの自プロセスへのSIGTERMを1度だけ発行する。

CLIのstop/start待ちはsocketへのdialだけを使い、RPCでゲートを塞がない。
restartの完了は`Ping`が返すPIDの変化で判定し、PIDを返さない旧daemonのために`Status`へのフォールバックを残す。
短いlistener断の観測は成功条件にしない。
LaunchAgentの`ThrottleInterval`を短くし、連続再起動時のlaunchdの待機を詰める。

入口は[`internal/daemon/restart.go`](../internal/daemon/restart.go)である。
ゲートがjobsとRPCの両方を待つことは[`TestPendingRestartWaitsForJobsAndRequests`](../internal/daemon/restart_test.go)が固定している。

## テストの並行化

`internal/daemon`のトップレベルテストは、専用の一時ディレクトリ・DB・Managerだけを使うものに`t.Parallel()`を付ける。
`t.Setenv`を自身かサブテストで呼ぶテスト、プロセス全体のgoroutine・fdを数えるテスト、短い待機に依存するテストは直列のまま残す。
