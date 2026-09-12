# セッションと復元

1. **貸出** — `wx claude`はworkspace rootのworktree方針を先に解決し、worktreeを使う場合だけ`ensureDaemon`でdaemonの生存を確認して`ResolveAndLease`を呼ぶ。
   launchdでのkickstartは接続自体に失敗したときだけ行う。応答が遅いだけの生きたdaemonを再起動しないためである。
   daemonはcwdからworkspaceを解決し、要求OIDと準備条件が完全一致するREADY slotを優先する。
   一致候補がなく `workspace_defaults.reuse_standby`（または workspace 個別値）が有効なら更新適合なHot StandbyをUPDATEジョブへ予約し、無ければPREPAREジョブでCold Startする。
   更新適格の判定と不適格候補のSTALE化・補充は[daemonの補充と回収](daemon-maintenance.md)にある。

   完全一致しなかった候補は、UPDATE予約・STALE化・cold startへの後退のいずれでも理由をdaemon logへ残す（`internal/daemon/lease_mismatch.go`）。
   理由は比較したその場で組み立てる。予約後は`slot_repositories`が更新後の値へ入れ替わり、差を後から復元できないためである。
   この診断はUPDATEやcold startへ落ちた後だけ動かし、完全一致した貸出にはinclude内容のhashとGit起動を持ち込まない。

   cwdがwx管理外のlinked worktreeのとき、貸出はrepositoryのmain worktreeへ解決されるのでcwd側のHEADは反映されない。
   HEADが食い違う場合はclientが貸出の前に、main worktreeのHEADで借りてよいかをYes既定で確認する。
   確認を出せない起動（`--json`、端末が無い起動）はnoticeをstderrへ出して従来どおり続ける。
   fullscreenのagentが起動すると標準出力のnoticeは流れてしまうため、端末があるときは起動**前**の確認にする。
   wxが作ったslotは貸出とsnapshotでHEADが動くのが前提なので、この確認の対象にしない。

2. **起動** — clientはleaseのpathをdescriptorとして開き、`internal/fdexec`経由でエージェントをそのdescriptorのディレクトリで起動する。
   子プロセスにはセッションID・token・daemon socketが渡り、以降のhookはこれらを持つ場合だけ動く。
   起動位置は`leasePath`が決めるslot側の起点（単一repositoryならslot内のworktree、それ以外はworkspace root）で、sourceのサブディレクトリから起動しても同じ位置になる。
   呼び出し時のcwdは`WX_SOURCE_CWD`にだけ入るので、同じ相対位置で実行したいコマンドは自分でcdする。

3. **準備完了のゲート** — 準備が終わっていないworktreeでエージェントが動き出さない仕組みは2通りある。
   `repository_defaults.readiness.mode: early`では、hookが使える通常起動はGit登録と起動用ファイルの配置までを待って起動する。
   以降の`user-prompt-submit`・`pre-tool-use` hookが全準備の完了まで操作を止める。
   `repository_defaults.readiness.mode: full`またはhookが無い起動（`internal/hookconfig`が判定する）は、clientが起動前に全準備を待つ。
   modeとtimeoutは `workspaces.<root>.repository_defaults.readiness.*` または `workspaces.<root>.repositories.<relative>.readiness.*` で上書きできる。
   clientはrepositoryのmain pathを知らないため、daemonが貸出応答へ合成済みの実効値を載せる。
   合成はslot内のいずれかが`full`なら`full`、timeoutは最長を採る。`full`要求の早期起動は約束を破るが、`early`要求を待たせるのは遅いだけで、最短のtimeoutでは最も遅いrepositoryが必ず失敗するためである。
   checkout hookやprepare commandが起動用の設定・指示を生成・更新する運用では、先行配置がその生成物を含められないため`full`を使う。
   完全一致したwarm slotは両方式とも即時起動する。

   UPDATE中はEarly Readyを公開せず、通常起動も`wx shell/run/new`も全repository・workspace rootの更新完了を待つ。
   resume・restoreは全準備を待ち、UPDATE経路を使わない。
   起動前の待機中もheartbeat・終了要求・失敗時のReleaseを維持する。

   hookの登録は`wx setup`が行い、`internal/hookconfig`が判定と書き込みを同じ受理条件で持つ。
   受理条件は`<絶対パス> hook <event>`ちょうどの形なので、agent設定側に条件分岐ラッパーを挟んでも受理されない。
   `WX_SESSION_ID`の有無による素通りはwx側（`internal/agent/hook.go`）で判定するため、ラッパーはそもそも不要である。

   `wx hook session-start`はエージェント側のネイティブなセッションIDをwxのセッションへ結び付ける。
   Codexのrewind / forkは、transcript metadataの`forked_from_id`とhook payloadの新IDを照合できた場合だけ旧IDから新IDへmappingを移管し、照合できなければ通常のbindとして扱う。
   RESTORING中に届いたIDは`pending_agent_session_id`として保持し、復元成功後に親から新しいセッションへ移譲する。

4. **返却** — `wx hook session-end`が`Release`を呼ぶ。
   ただしsession-end hookはエージェント本体より先に走ることがあるため、clientまたはエージェントのプロセスが生きている間は返却しない。
   前面clientの終了通知か、daemonのorphan reconcileが「もう書き手がいない」ことを確認してから先へ進む。

5. **アーカイブ** — SNAPSHOTジョブが2種類のスナップショットを取る。
   リポジトリごとのHEAD・index・worktreeは**ソースリポジトリ側の**保護オブジェクトとrefになる。
   multi-repository workspaceではさらに、workspace root自体のtarをwxのworktree root配下へ書き、`workspace_snapshots`行が指す。
   rootのtarから落とすのはslot内でsymlinkだったpathだけで、実体のある作業はruleの変更にかかわらずtarへ入る。

   refの公開はDB行の永続化の後に行う。逆順だと、reconcileから見て正常なアーカイブが素性不明のrefに見える窓が開く。

   indexに`skip-worktree`か`assume-unchanged`が付いたpathはsnapshotの対象外で、HEADの内容として記録する（[所有権証明](ownership.md)）。
   hookが個人版の設定や認証情報をslotごとに置き換える運用ではこれらのflagが常時立つため、clean短絡が効かなくなる。
   flag付きpathへの編集は保存されないが、両flagは「このファイルのローカル差分を見ない」という宣言なので、その責任は立てた側にある。

6. **再開** — `wx resume`、`claude --resume`、`codex resume`はclientがagent session IDを解決し、`Resume`または`ResolveAndLease`へ合流させる。
   選択した会話と明示的な`wx resume <wx-session-id>`は同じRESTORE経路を使う。

   当時のworktreeを復元できないときは会話の再開を優先し、新しいworktreeで再開してよいかをYes既定で確認して`--fresh`と同じ経路へ倒す。
   復元不能はdaemonが`recovery=unavailable`を失敗メッセージに載せて伝え、clientはRESTORE系のfailure codeとEXPIRED snapshotの両方をこの確認に集約する。
   確認は`resume.auto_fresh`が真なら省き、端末がなければnoticeを出して再開を続ける。
   やり直しは繰り返さず、新しいworktreeでの失敗はそのまま返す。

   再開前の`ResumeStatus`はarchive本文までは検証せず、`integrity`に`not_checked`を返す。
   archive本文のSHA256はRESTORE予約でGC保護を得た後、復元workerがtargetを変える前に1度だけ検証し、検証済みdescriptorをそのまま展開へ渡す。
   破損・置換・読み取り障害は`SNAPSHOT_CORRUPT`で隔離する。
   これは`recovery=unavailable`を伴わないため、`resume.auto_fresh`や非端末でも新しいworktreeへ自動では倒れない。

   ネイティブresumeは遅延バインドや`_unbound` slotを新規生成せず、clientが準備完了を前面で待ってから起動する。

   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

   clean baseを作る`post-checkout` hookが`skip-worktree`・`assume-unchanged`を付ける場合があるが、復元先のindex flagは外さず、2本の`read-tree`で消えた分を立て直すだけにする。
   外すとgitがflag付きの実ファイルをtreeの内容で上書きし、hookが置いたslot側の個人設定を失う。
   snapshotがflag付きpathをHEADの内容で記録しているためentryは一致し、flagを保ったままの`read-tree --reset -u`も拒否されない。
   ただし`assume-unchanged`の実ファイルは、flagを保っていても`read-tree --reset -u`がtreeの内容で書き戻す（git側の仕様でwxからは防げない）。

貸出からsnapshot・復元までGit状態が保たれることは、`internal/daemon`の[`TestLeaseArchiveAndRestorePreservesGitState`](../internal/daemon/resume_integration_test.go)が固定している。

## エージェント起動以外への貸出

`wx shell` / `wx run` / `wx new`はagentを起動せずにworktreeを借りる。
貸出の性質は`sessions.lease_kind`で表し、session stateは`STARTING` / `ACTIVE`のままとする。
新しいstateを作ると返却の分岐やorphan判定をはじめ`state IN (...)`を持つ全SQLを一斉に触ることになり、返却が保存経路から外れる危険があるためである。
`agent_kind`にはwx自身を表す値を入れ、`wx slots`の表示と`--resume`の照合に使う。

`wx shell`と`wx run`はagent起動と同じ`internal/cli/client.go`の`launch`を共有するので、`defer Release`やheartbeatといった返却の仕掛けが分岐しない。
実行するプログラムを`RegisterAgentProcess`で`agent_pid`に登録するので、snapshot前の生存確認も同じに効く。
`wx new`だけはプロセスに随伴せず、`client_pid`を持たずheartbeatも張らない。
そのため`Store.OrphanCandidates`は`lease_kind`が`path`の行を除外する。
除外を忘れると`wx new`のworktreeはheartbeat切れとして保存・返却されGCの対象になるため、ここがこの経路で最も静かに壊れる箇所である。

3つとも、貸出の取得から準備待ちの間だけ`signal.NotifyContext`で中断signalを捕まえる。
既定のdispositionのままCtrl-Cで即死すると、返却の`defer`が走らないまま貸出だけがdaemonに残るためである。
捕捉はagentの起動直前に返し、以降のsignalは従来どおりagentへ中継する。

client側の捕捉が効かない中断（`kill -9`・端末ごとの消滅）に備えて、daemon側でも`path`貸出だけを回収する。
`internal/rpc`は接続ごとに切断通知をhandler ctxへ載せ（`rpc.PeerClosed`）、`Handler.waitReady`はREADY前に接続が切れたら待機を打ち切って返却する。
対象を`path`に絞るのは、他の貸出は`client_pid`とheartbeatで回収できるのに対し、`wx new`だけがpathを渡す前の未受領のまま誰にも返されずに残るためである。

`wx new`の返却契機は3つで、`wx release --discard`を除きどれも既存の返却経路（session `RELEASING`→slot `DRAINING`→SNAPSHOTジョブ）へ載る。
どれも利用者へpathを渡せた後の話で、渡す前に中断された貸出は上の2経路がその場で返す。

1. 親sessionの終了。`WX_SESSION_ID` / `WX_SESSION_TOKEN`を持つ環境からの要求は`sessions.lease_owner_session_id`へ親を記録し、親が使用中でなくなると`Store.OrphanedChildLeases`が拾う。
   resume chain専用の`parent_session_id`は流用しない。`internal/state/standby.go`が「親がEXPIRED」を条件にしているため、流用すると子貸出のstandby補充成功記録が親の終了まで入らない。
2. `wx release <id>`の明示指定。session tokenを持たない経路なので、生きたclient / agentを持つ貸出は拒否する。
   `--discard`だけは例外で、`Store.ReleaseDiscardingWithOutcome`が返却と同じtransactionでSNAPSHOTを積まずREMOVEを積む（session `EXPIRED`→slot `REMOVING`）。
   保存を待たずに1回で削除が予約されるので、再実行の案内も workspace の `retention.ended_worktree` の猶予も無い。slotが`PREPARING`で予約できないときだけ、通常の返却と同じく保存経路へ載る。
3. 設定`lease.ttl`の経過。`Store.ExpiredLeaseCandidates`が拾う。

期限が来ても保存されてから返却され、返却後も workspace の `retention.ended_worktree` の間は実体が残り`wx shell --resume <id>`で復元できる。
ただしsnapshot後の編集は保存されない。
`Manager.snapshotSession`の`processAlive(AgentPID)`ガードは`path`貸出では効かないため、期限到来時にSubAgentがまだ編集中のworktreeのsnapshotを取ることは起こり得る。
`wx new`の主返却契機は親sessionの終了に置き、終わったら`wx release`で返す。

この経路の要は[`TestPathLeaseSurvivesOrphanReconcileAndExpiresThroughSnapshot`](../internal/daemon/leasekind_test.go)である。
`wx new`の貸出がorphan回収を生き延び、期限到来で保存経路を通ることを固定している。
