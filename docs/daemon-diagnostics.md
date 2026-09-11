# daemonの診断と再起動

## doctorの診断

`wx doctor`は検査ごとに種別つきのfinding（`internal/diag`の`Finding`）を返し、表示側は文面から重大さを判定しない。
終了コードはproblem（利用者の対処が必要）とunchecked（前提の故障で実施できず）のどちらかがあれば1とし、実施できなかった検査を成功として扱わない。
`--json`は`-v`によらず全findingを返す。消費側の契約なので、絞り込みを足すなら`state.JSONSchemaVersion`を上げる。
`findings`を返せない古いdaemonの応答は正常と読ませず、CLIが`wx daemon restart`を促すproblemを足す。

daemon接続なしで成立する検査は[`internal/diag`](../internal/diag/diag.go)に置き、storeを要する検査はdaemon側に残す。
daemonへ接続できない場合とdegradedの場合はstore依存の検査をuncheckedで並べ、同じ故障を検査ごとに繰り返さない。

1件の故障は検査全体へ広げない。
worktree rootのpath検査と登録検査は別のfindingとして両方保持し、登録状態でpath検査の結果を上書きしない。
登録済みworkspaceに属するrepositoryでrefsを読めない場合も、その1件のproblemとして対象pathつきで報告し、他のrepositoryの照合と他の検査は続ける。
登録済みworkspaceに属さず照合すべきsnapshotも持たないrepository記録は、refsを読めなくてもproblemにせずinfoに留める。
GCの`PruneRepositories`が回収するまでの一時的な記録であり、errorにするとその間doctorが失敗し続けるためである。

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
