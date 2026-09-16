# daemonの補充と回収

準備・復元の所有権失敗は終端させ、削除はDB登録済みの範囲を回収する。
ここで隔離・停止した状態をどう報告するかは[daemonの診断と再起動](daemon-diagnostics.md)にある。

## ジョブの分類と実行枠

実行枠は利用者向け（config v2 の `system.pool.preparation_concurrency`）と保守用（`maintenanceJobSlots`）に分かれ、保守へ利用者向けの枠を貸さない。
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
枠数は`workspaces.<root>.warm_count`、`workspace_defaults.warm_count`の順に継承する。
保持期間も同じく`workspaces.<root>.retention.hot_standby`、`workspace_defaults.retention.hot_standby`の順に継承する。
終了worktreeの保持は`workspaces.<root>.retention.ended_worktree`、`workspace_defaults.retention.ended_worktree`の順に継承する。
workspace個別値は単一リポジトリではそのリポジトリのmain worktree、multi-repositoryではworkspace rootに適用する。
枠数0はそのworkspaceの補充を無効にするが、個数指定だけで`hot`へは変更しない。
保持期間0も同じく補充を無効にする。補充とGCは同じ実効値で判断するので、作っては即座に回収する往復は起きない。
worktree方針も同じ対応で、`hot`以外のworkspaceはGCの待機枠数を0として扱い、方針を変える前のREADYを残さない。

GCの問い合わせは最短の保持期間から作ったcutoffだけをSQLへ置き、workspaceごとの正確な判定はGo側で行う。
最長で絞ると保持期間の短いworkspaceのslotが問い合わせから落ちる。
workspaceに紐付かないslot（`slots.workspace_id`がNULL）はglobal値で判定する。

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

再利用は既定で有効（`workspace_defaults.reuse_standby`、workspace個別値で上書き可）で、無効にするとOID・fingerprint完全一致だけを貸す動作になる。
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
入力変更に応じた`prepare.command`の再実行も、貸出時の更新と同じくidle更新で行う。
更新に使えない候補はidle更新では回収せず、READYのまま残して貸出時の判断に委ねる。

入口は[`internal/daemon/standby_idle_update.go`](../internal/daemon/standby_idle_update.go)である。
貸出前にREADYが現在のmainへ揃うことは[`TestIdleStandbyRefreshUpdatesMismatchedReadyBeforeLease`](../internal/daemon/standby_idle_update_test.go)が固定している。

### 補充停止

補充停止は`replenish_suspensions`に永続化し、定期reconcileと補充ジョブの双方で参照する。
停止理由によらず、解除はそのworkspaceの手動起動（貸出・resume）の成功か`wx retry-standby`、`wx clear --replenish`だけとし、既存sessionの返却では解除しない。
`wx clear --all`は補充が有効な全workspaceを一度に止めるため、`wx retry-standby --all`で停止行のある全workspaceをまとめて戻せる。
準備に失敗したFAILED slotは待機枠に数えるので、解除しただけでは不足が0のままになる。`wx retry-standby`は補充を予約する前にFAILED slotを削除予約へ載せ、REMOVINGへ移してから枠を数え直させる。

## clearとGC

`clean.go`は受付時点で対象と期限を確定し、永続化する。
対象範囲はworkspaceで絞れるが、root世代では絞らない。「保持期間を待たず今消す」という契約と、世代をまたいで同じ`slots.workspace_id`を持つ実装の双方に一致するためである。
帰属を確定できないslotは、範囲を指定した命令では対象にしない。
`Manager.driveClean`はbackgroundで既存ジョブを監視し、workerを占有したまま別ジョブを待たない。
削除は通常の`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`へ載せる。
隔離slotはmodeによらず`ScheduleQuarantinedRemoval`へ載せる。
`--discard`は保存を省略して削除を予約し、modeに永続化して再起動後も維持する。
使用中のdetached lease（`wx new`）も、返却と同じtransactionでSNAPSHOTを積まずREMOVEへ載せ、保存待ちを経ずに削除待ちへ進める。
実行中runへ合流できるのは、modeと対象範囲がどちらも同じ再実行だけとする。

`--replenish`の補充再開はrunを閉じた後に置く。停止解除は貸出側と同じ検査を通るので、RUNNINGの間は必ず断られるためである。
再開する対象はrunが記録した停止行から引く。targetから復元し直すと、受付時点でどのworkspaceを止めたかを再現できない。
失敗・隔離が残ったworkspaceは戻さない。停止は「作って即消す往復」を防ぐためにあり、環境の回復確認は`wx retry-standby`に委ねる契約だからである。

`--unmanaged`はこのrun機構に載せない。`clean_targets`はslot IDを要求し、登録外の実体は持たないためである。
専用のRPCで列挙と削除をその場で終えるので、modeも進捗の問い合わせも持たず、`assertNoActiveClean`による貸出の停止も補充停止も伴わない。
他のmodeとは対象が重ならないので併用は受け付けず、削除後は使用量の測り直しだけを要求する。

終了要求は`--all`だけが`session_termination_requests`へ期限付きで記録し、heartbeatとagent登録の応答でclientへ渡す。
signalを送るのはclientだけで、daemonは記録されたPIDへ触れない。
期限内に停止を確認できない対象は失敗として閉じ、遅れた終了は通常の返却へ戻す。
生きたclientもagentも持たない貸出（`wx new`）は終了要求の宛先がないため、`advancePending`は要求を積まずその場で返却して保存経路へ移す。
`--all`無しで残す場合のskip理由も、停止を待つ`--all`ではなく`wx release <id>`を案内する。
`wx shell` / `wx run`の強制停止は既存の`--all`経路で成立するので、`session_termination_requests`は貸出用に拡張しない。

run実行中は`assertNoActiveClean`が貸出・復元・待機用作成の書き込みトランザクションを断り、対象が新しいsessionへ渡るのを防ぐ。
待機用slotを対象に含めるのは`--standby`と`--all`だけで、削除後に補充を停止するのもその範囲に限る。
安全な処理境界の待機は`cleanBoundaryWait`で制限し、貸出を断ったまま無期限に待たない。

終了worktreeの候補からは、未保全のsubmodule作業を記録したslotを外す（[所有権証明](ownership.md)）。
除外は候補選択に置き、`wx clear`・`wx forget`と共有する`ScheduleRemoval`は変えない。自動と明示の削除の境界がそこにあるためである。

GCの候補選択と削除の入口は[`internal/daemon/gc.go`](../internal/daemon/gc.go)で、隔離slotも通常の`REMOVE`で登録範囲を回収する。
登録だけを根拠に隔離slotを回収することは[`TestGCRemovesRegisteredQuarantineWithoutCachedIdentity`](../internal/daemon/gc_integration_test.go)が固定している。

### forgetが自分で消すもの

`wx forget`は、貸出中のもの（`LEASED`などのslot・終了していないsession・実行待ちのjob）が残る間は断り、案内はsessionの停止か`wx release`にする。
それ以外の未貸出slot（`READY`・`STALE`・`FAILED`）は利用者の作業を含まないため、断らずにその場で回収する。
回収のたびに`FinishRemoval`が補充を予約するので、先に`SuspendReplenish`で止めてから消す。
止めずに消すと`hot`なworkspaceでは補充が走り、解除が実行中のjobで落ちるか待機枠が復活する。
解除に成功すれば停止行も一緒に消えるため、残っている`FORGET`の停止は途中で失敗したforgetを指し、`wx doctor`がそう報告する。

復元資産（`ARCHIVED`のsessionとそのsnapshot・recovery ref・保存ファイル、隔離したsession・slot）は既定では消さず、断る理由を`--discard-recovery`の案内付きで返す。
`wx clear`が既定で復旧データを残し`--discard`を要求するのと同じ分担で、名前が示す「管理の解除」に破棄を含めない。
破棄する場合も、GCの期限切れ処理と違いソースリポジトリや保存先を開けなくても解除は止めない。
root ごと消えた登録ではその削除が必ず失敗し、止めると登録を消す経路が無くなるためである。
消し残したrefとファイルは警告に残す。

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

保存の失敗で隔離するのは、所有権検証に落ちたものと、worktreeやarchiveへ書き始めた後に落ちたものだけとする。
rule解決や設定の読み出しのように書込み前で副作用の無い失敗はjobを`FAILED`にするだけで、slotは`SNAPSHOTTING`のまま残す。
原因を直せば次の一巡の`EnsureRecoveryJobs`が保存を作り直して終わるので、手動の出口しか持たない隔離へ倒さない。
待つ間の理由は`wx doctor`がjobの記録から出す。

隔離slotを持つsessionは`DRAINING`へ進めず、`EXPIRED`で終端させslotのownerだけを外す。
slotは`QUARANTINED`のままworktree・snapshotを保持し、同じ返却の失敗が繰り返されるのを防ぐ。
この扱いは`Release`の全経路に適用する。

### recovery refを失ったworkspace

記録したrecovery refがソースリポジトリに無いとき（リポジトリを消して同じpathに作り直した場合）は、`QuarantineMissingRecoveryRef`がsnapshot・session・slotを隔離する。
GCもreconcileも隔離したsnapshotを自動では捨てず、出口は`wx discard-recovery <workspace-path>`と`wx forget --discard-recovery`の2つだけとする。
登録を残したまま隔離だけを解消する用途があるので、`wx discard-recovery`は`wx forget`に統合せず残す。
refが無いsnapshotからは復元できないため`Manager.DiscardRecovery`で失う復元手段は無いが、slotのworktreeにある未保存の作業は消えるので、`--dry-run`で対象とpathを出せるようにしている。
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

中断ではなく準備そのものが失敗した場合も、Early Readyを過ぎた貸出は隔離しない。
エージェントは既にそのworktreeで作業しており、隔離すると返却が`LEASED`を通らず`DRAINING`へ進めないため、作業がsnapshotに届かないからである。
失敗は`slots`の失敗記録として残したまま`LEASED`へ進め、PREPARE jobは成功として終える。
worktree directoryの作成とidentityの記録はEarly Readyまでに終わっているので、`slot_repositories`もREADYへ進め、削除・返却が要求する状態を満たす。
配置履歴は完成していないので記録せず、`placement_history_complete`が0のまま待機枠の再利用・更新から外れる。
owner sessionを持たない待機枠の補充・更新の失敗は、これまでどおり隔離する。
