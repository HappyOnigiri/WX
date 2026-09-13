# セッションと復元

1. **貸出** — `wx claude`は起動場所のworkspace rootのworktree方針を先に解決し、worktreeを使う場合だけ`ensureDaemon`でdaemonの生存を確認して`ResolveAndLease`を呼ぶ。
   会話IDを指定した再開だけは方針の解決より先に会話を解決する（「再開」参照）。
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

   branchを指定しない貸出では、明示された設定を最優先し、未設定ならGitの既定branchの参照とローカルの候補を順に検証して起点を決める。
   根拠が一つも成立しない場合は自動で選ばず、remoteの既定参照を設定するかbranchを明示するよう診断して停止する。

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

   `pre-tool-use` hookは準備完了を待つほか、agentが単独で実行しようとした`git worktree add`をツール入力の書き換えで`wx new`へ写す。
   素のworktreeはslotの準備も保存も返却も受けられないためで、安全に写せない形は写し方を案内して拒否する。
   判定はコマンド文字列の静的な解析だけで行い、候補を同定できない入力は通す。

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

   停止中rebaseの進行情報はtreeにもindexにも現れないため、worktree専用gitdirの制御ファイルを別のrefで保存する。
   ファイル内容を持つtreeと、復元後のHEADから到達できないcommit（`orig-head`・`onto`・`stopped-sha`など）を親に並べたcommitを1本作り、他のrecovery refと同じ経路で保護・回収する。
   `rebase -i`のedit停止はworking treeがcleanなので、この保存はclean短絡の側でも行う。
   未解消indexを伴う停止（conflict停止）は対象外で、従来どおり`write-tree`の失敗として隔離する。

   submoduleは子1件を1本のcommitへ畳んで保存し、公開先だけが親と違う（[worktreeのコピーとリンク](worktree-copy.md)）。
   保存できた子は未保全の記録から外れるため、`unsaved_submodules`に残るのは「wxが保存できない条件」だけになる。

   indexに`skip-worktree`か`assume-unchanged`が付いたpathはsnapshotの対象外で、HEADの内容として記録する（[所有権証明](ownership.md)）。
   hookが個人版の設定や認証情報をslotごとに置き換える運用ではこれらのflagが常時立つため、clean短絡が効かなくなる。
   flag付きpathへの編集は保存されないが、両flagは「このファイルのローカル差分を見ない」という宣言なので、その責任は立てた側にある。

   sparse範囲外に現れた実体は、このflag方針の例外としてsnapshotに含める。
   範囲外のtracked pathは実体を持たないので編集しようがなく、保存されるのは範囲外に新しく作られたものだけで、flag付きpathの扱いとは競合しない。
   resumeはsparse条件を保ったまま戻すので、範囲外でもHEADと差の無いpathは実体を持たないままになる。

6. **再開** — `wx resume`、`claude --resume`、`codex resume`、`codex exec resume`はclientがagent session IDを解決し、`Resume`または`ResolveAndLease`へ合流させる。
   選択した会話と明示的な`wx resume <wx-session-id>`は同じRESTORE経路を使う。

   会話IDを指定した再開は、worktreeを使うかを起動場所ではなく会話の側で決める。
   記録済みsessionの復元先は当時のworkspaceで起動場所と無関係なので、起動場所の方針は判断材料にならない。
   `-w` / `-n` / `-s`を明示した起動だけはその指定を優先し、この判定へ入らない。
   wx管理外の会話は、会話に記録されたcwdのworkspaceの方針で決める。
   判定を`WorktreePolicy`としてdaemonへ置くのは、畳まれたslotのpathを登録済みのworkspace rootへ読み替えられるのがdaemonだけだからである。
   worktreeを作らない方針のときと、cwdもworkspaceも解決できないときは、worktree無しで会話だけ再開しnoticeで理由と`wx config --workspace <root> worktree cold`を伝える。
   会話の再開自体はcwdに依存しないので、ここで失敗にはしない。
   起動場所の方針で決め続けると、巨大なmulti-repository workspaceのcwdを持つ会話を再開しただけでそこにworktreeを作ることになる。

   wxもagentの履歴も引けないIDは、新しい会話の開始として扱わずworktree無しでagentへ渡す。
   履歴の走査はagentの記録形式に依存し、実在する会話を取りこぼし得るのでwx側で「存在しない」と断定しない。
   実在すれば再開でき、実在しなければagent自身が理由を示して非0で終わる。どちらもworktreeを消費しない。

   当時のworktreeを復元できないときは会話の再開を優先し、新しいworktreeで再開してよいかをYes既定で確認して`--fresh`と同じ経路へ倒す。
   復元不能はdaemonが`recovery=unavailable`を失敗メッセージに載せて伝え、clientはRESTORE系のfailure codeとEXPIRED snapshotの両方をこの確認に集約する。
   確認は`resume.auto_fresh`が真なら省き、端末がなければnoticeを出して再開を続ける。
   やり直しは繰り返さず、新しいworktreeでの失敗はそのまま返す。

   復元はsubmoduleを親より先に戻す。親のsnapshotは子の移動後HEADをgitlinkとして持つため、後に回すと親の一致検証が必ず不一致になる。
   子の復元失敗は復元全体の失敗として扱い、原因を隔離に残す。中途半端に戻した子を黙って抱えるより、利用者が気づける方を選ぶ。

   再開前の`ResumeStatus`はarchive本文までは検証せず、`integrity`に`not_checked`を返す。
   archive本文のSHA256はRESTORE予約でGC保護を得た後、復元workerがtargetを変える前に1度だけ検証し、検証済みdescriptorをそのまま展開へ渡す。
   破損・置換・読み取り障害は`SNAPSHOT_CORRUPT`で隔離する。
   これは`recovery=unavailable`を伴わないため、`resume.auto_fresh`や非端末でも新しいworktreeへ自動では倒れない。

   ネイティブresumeは遅延バインドや`_unbound` slotを新規生成せず、clientが準備完了を前面で待ってから起動する。

   停止中rebaseの制御ファイルは、HEAD・index・worktreeの一致検証をすべて終えた最後に書き戻す。
   先に書くと、その後に走るresume prepareとtree一致検証がrebase中のリポジトリを相手にすることになる。
   書き戻しの前には対象pathを必ず削除するので、再利用されたslotが前の貸出の進行情報を引き継がない。

   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

   clean baseを作る`post-checkout` hookが`skip-worktree`・`assume-unchanged`を付ける場合があるが、復元先のindex flagは外さず、2本の`read-tree`で消えた分を立て直すだけにする。
   外すとgitがflag付きの実ファイルをtreeの内容で上書きし、hookが置いたslot側の個人設定を失う。
   snapshotがflag付きpathをHEADの内容で記録しているためentryは一致し、flagを保ったままの`read-tree --reset -u`も拒否されない。
   ただし`assume-unchanged`の実ファイルは、flagを保っていても`read-tree --reset -u`がtreeの内容で書き戻す（git側の仕様でwxからは防げない）。

   sparse範囲外の作業だけは、flagを外して実体を書き出す。
   対象はsnapshotのworktree treeがHEADと差を持つpathに限るので、範囲外でも作業の無いpathはflagを保ち実体を持たない。
   書き出せるのはindexがsnapshotのworktree treeを指す間だけなので、2本の`read-tree`の間で行う。

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
   `wx release`は返却の受付と保存・削除の完了が別で、既定では受付だけを返す。`--wait`を指定すると、その返却で積まれた保存または削除の完了まで待てる。
3. 設定`lease.ttl`の経過。`Store.ExpiredLeaseCandidates`が拾う。

期限が来ても保存されてから返却され、返却後も workspace の `retention.ended_worktree` の間は実体が残り`wx shell --resume <id>`で復元できる。
ただしsnapshot後の編集は保存されない。
`Manager.snapshotSession`の`processAlive(AgentPID)`ガードは`path`貸出では効かないため、期限到来時にSubAgentがまだ編集中のworktreeのsnapshotを取ることは起こり得る。
`wx new`の主返却契機は親sessionの終了に置き、終わったら`wx release`で返す。

この経路の要は[`TestPathLeaseSurvivesOrphanReconcileAndExpiresThroughSnapshot`](../internal/daemon/leasekind_test.go)である。
`wx new`の貸出がorphan回収を生き延び、期限到来で保存経路を通ることを固定している。
