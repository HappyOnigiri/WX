# daemonの補充・回収・再起動

ジョブ配送は`internal/daemon/jobs.go`、周期処理は`maintenance.go`、実体照合は`reconcile.go`を参照する。
準備・復元の所有権失敗は終端させ、削除はDB登録済みの範囲を回収する。

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
