# セッションと復元

1. **貸出** — `wx claude`はworkspace rootのworktree方針を先に解決する。
   未定義の対話起動は`internal/tui.Select`で選択を保存し、対象外なら現在のCWDで通常起動する。
   worktreeを使う場合は`ensureDaemon`でdaemonの生存を確認し（応答が遅いだけの生きたdaemonをlaunchdで再起動しないよう、接続自体に失敗したときだけkickstartする）、`ResolveAndLease`を呼ぶ。
   daemonはcwdからworkspaceを解決し、要求したOIDと準備条件が完全一致するREADY slotを優先する。
   一致候補がなく`worktree.reuse_standby`が有効なら、更新適合条件と配置履歴を満たすHot StandbyをUPDATEジョブへ予約し、無ければPREPAREジョブでCold Startする。
   更新不適格と判定した候補は（`--branch`指定でなければ）STALEにして補充へ回すので、次の貸出では作り直したstandbyが使える。
   完全一致しなかった候補は、UPDATE予約・STALE化・cold startへの後退のいずれでも理由をdaemon logへ残す。
   `mismatch`は分類（`oid` / `fingerprint` / `repository_set` / `repository_state` / `worktree`）で、`mismatch_detail`は対象を指す。
   `fingerprint`の`mismatch_detail`は`slot_placements`の記録と現在のinclude/link計画の差をpathで示し、追加を`+`、削除を`-`、内容や配置方式の変更を`~`で表す。
   配置差が無い場合はmanifest・コピー方式・CoW共有下限のような配置に現れない入力が変わったことを示す。
   予約後は`slot_repositories`が更新後の値へ入れ替わり差を復元できないため、理由は比較したその場で組み立てる。
   この診断はUPDATEやcold startへ落ちた後だけ動かし、完全一致した貸出には余分なGit起動とファイル読み取りを持ち込まない。
   cwdがwx管理外のlinked worktreeのとき、貸出はrepositoryのmain worktreeへ解決されるのでcwd側のHEADは反映されない。
   HEADが食い違う場合はclientが貸出の前に`internal/cli/linked_worktree.go`で検出し、main worktreeのHEADで借りてよいかをYes既定で確認する。
   確認を出せない場合（`wx new --json`、端末が無い起動）はnoticeをstderrへ出して従来どおり続ける。
   fullscreenのagentが起動すると標準出力のnoticeは流れてしまうため、端末があるときは起動前の確認にする。
   wxが作ったslot（`storage.worktree_root`配下）は貸出とsnapshotでHEADが動くのが前提なので、この確認の対象にしない。
2. **起動** — clientはleaseのpathをdescriptorとして開き、`internal/fdexec`経由でエージェントをそのdescriptorのディレクトリで起動する。
   子プロセスには`WX_SESSION_ID`・`WX_SESSION_TOKEN`・`WX_DAEMON_SOCKET`などが渡り、以降のhookはこれを持つ場合だけ動く。
3. **準備完了のゲート** — 準備が終わっていないworktreeでエージェントが動き出さない仕組みは2通りある。
   既定の`readiness.mode: early`では、hookが使える通常起動は`WaitEarlyReady`でGit登録と起動用ファイルの配置完了を待つ。
   その後の`wx hook user-prompt-submit`と`wx hook pre-tool-use`は従来どおり`WaitReady`を呼び、全準備が完了するまで操作を止める。
   `readiness.mode: full`またはhookが無い起動は、clientが起動前に`WaitReady`を待つ（`hookconfig.Available`で判定）。
   完全一致したwarm slotは両方式とも即時起動する。
   UPDATE中はEarly Readyを公開せず、通常起動と`wx shell/run/new`のどちらも全repository・workspace rootの更新完了を待つ。
   resume・restoreは従来どおり全準備を待ち、UPDATE経路を使わない。
   起動前の待機中もheartbeat・終了要求・失敗時のReleaseを維持する。
   hookの登録は`wx setup`が行い、`internal/hookconfig`が判定と書き込みを同じ受理条件で持つ。
   `WX_SESSION_ID`の有無による素通りは`internal/agent/hook.go`のwx側で判定するため、agent設定側での条件分岐ラッパーは不要である。
   受理条件はちょうど3トークンの`<絶対パス> hook <event>`なので、そうしたラッパーはそもそも受理されない。
   `wx hook session-start`は`BindAgentSession`系でエージェント側のネイティブなセッションIDをwxのセッションへ結び付ける。
   Codex の rewind / fork は transcript metadata の `forked_from_id` と hook payload の新IDを照合できた場合だけ、旧 native IDから新IDへmappingを移管する。照合できない場合は通常のbindとして扱う。
   RESTORING中に届いたIDは`pending_agent_session_id`として保持し、復元成功後に親から新しいセッションへ移譲する。
   resumeの`session-start`は`previous_worktree`を返し、`source == "resume"`のとき旧pathを使わないよう通知文を1行出す。
4. **返却** — `wx hook session-end`が`Release`を呼ぶ。
   ただしsession-end hookはエージェント本体より先に走ることがあるため、clientまたはエージェントのプロセスが生きている間は返却しない。
   前面clientの終了通知か、daemonのorphan reconcileが「もう書き手がいない」ことを確認してから先へ進む。
5. **アーカイブ** — SNAPSHOTジョブが2種類のスナップショットを取る。
   リポジトリごとのHEAD・index・worktreeは`refs/wx/recovery/<session>/<repo>/*`として**ソースリポジトリ側の**保護オブジェクトとrefになる。
   multi-repository workspaceではさらに、workspace root自体のtarをwxのworktree root配下（`_recovery/workspace-snapshots/`）へ書き、`workspace_snapshots`行が指す。
   refの公開はDB行の永続化の後に行う。
   逆順だと、reconcileから見て正常なアーカイブが素性不明のrefに見える窓が開く。
6. **再開** — `wx resume`、`claude --resume`、`codex resume`はclientがagent session IDを解決し、`Resume`または`ResolveAndLease`へ合流させる。
   選択した会話と明示的な`wx resume <wx-session-id>`は同じRESTORE経路を使う。
   `--fresh`は会話を同じIDで再開しつつ現在のbaseからslotを作り、`--branch`は`--fresh`との併用時だけ使う。
   当時のworktreeを復元できないときは会話の再開を優先し、新しいworktreeで再開してよいかをYes既定で確認して`--fresh`と同じ経路へ倒す。
   復元不能はdaemonが`recovery=unavailable`を失敗メッセージに載せて伝え、clientはRESTORE系のfailure codeとEXPIRED snapshotの両方をこの確認に集約する。
   再開前の`ResumeStatus`はsnapshot行の状態・期限・repositoryの充足と、workspace archiveのmetadata・実体の種別までしか見ず、`integrity`に`not_checked`を返す。
   archive本文のSHA256はRESTORE予約でGC保護を得た後、復元workerがtargetを変える前に1度だけ検証し、検証済みdescriptorをそのまま展開へ渡す。
   破損・置換・読み取り障害は`SNAPSHOT_CORRUPT`で隔離する。
   これは`recovery=unavailable`を伴わないため、`resume.auto_fresh`や非端末でも新しいworktreeへ自動では倒れない。
   確認は`resume.auto_fresh`が真なら省き、端末がなければnoticeを出して再開を続ける。
   やり直しは1回だけで、2回目の失敗はそのまま返す。
   ネイティブresumeは遅延バインドや`_unbound` slotを新規生成せず、clientが準備完了を前面で待ってから起動する。
   clean baseを作る`post-checkout` hookが`skip-worktree`・`assume-unchanged`を付ける場合があるため、復元は`read-tree`の前に復元先indexのflagを外し、tree適用後に同じpathへ戻す。
   flagが残ったままの`read-tree`はfileの書き換えを拒否するか素通りするので、外す操作はtree適用より前に置く。
   flagの出所は復元先indexだけで、snapshotは一覧を持たない。session中に手で付けたflagは引き継がれず、そのpathの内容はflagのないtracked changeとして現れる。
   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

## エージェント起動以外への貸出

`wx shell` / `wx run` / `wx new`はagentを起動せずにworktreeを借りる。
貸出の性質は`sessions.lease_kind`（`agent` / `path` / `shell` / `command`）で表し、session stateは`STARTING` / `ACTIVE`のままとする。
新しいstateを作ると`state IN (...)`を持つ全SQL（返却の分岐・orphan判定・heartbeat・standby補充記録・`wx clear`の使用中判定）を一斉に触ることになり、返却が保存経路から外れる危険があるためである。
`agent_kind`には`wx-shell` / `wx-run` / `wx-path`を入れ、`wx slots`のAGENT列と`--resume`の照合に使う。

`wx shell`と`wx run`は`internal/cli/client.go`の`launch`をそのまま共有し、lease取得・`defer Release`・heartbeat・descriptor束縛・signal中継・`wx clear --all`への応答を既存経路から得る。
実行するプログラムを`RegisterAgentProcess`で`agent_pid`に登録するので、snapshot前の生存確認も同じに効く。
`wx new`だけはプロセスに随伴せず、`client_pid=0`でheartbeatも張らない。
そのため`Store.OrphanCandidates`は`lease_kind<>'path'`で除外する。
除外を忘れると`wx new`のworktreeは45秒で保存・返却されGCの対象になるため、ここがこの経路で最も静かに壊れる箇所である。

`wx new`の返却契機は3つで、`wx release --discard`を除きどれも既存の返却経路（session `RELEASING`→slot `DRAINING`→SNAPSHOTジョブ）へ載る。

1. 親sessionの終了。`WX_SESSION_ID` / `WX_SESSION_TOKEN`を持つ環境からの要求は`sessions.lease_owner_session_id`へ親を記録し、親が使用中でなくなると`Store.OrphanedChildLeases`が拾う。
   resume chain専用の`parent_session_id`は流用しない。`internal/state/standby.go`が「親がEXPIRED」を条件にしているため、流用すると子貸出のstandby補充成功記録が親の終了まで入らない。
2. `wx release <id>`の明示指定。session tokenを持たない経路なので、生きたclient / agentを持つ貸出は拒否する。
   `--discard`だけは例外で、`Store.ReleaseDiscardingWithOutcome`が返却と同じtransactionでSNAPSHOTを積まずREMOVEを積む（session `EXPIRED`→slot `REMOVING`）。
   保存を待たずに1回で削除が予約されるので、再実行の案内も`retention.ended_worktree`の猶予も無い。slotが`PREPARING`で予約できないときだけ、通常の返却と同じく保存経路へ載る。
3. 設定`lease.ttl`の経過。`Store.ExpiredLeaseCandidates`が拾う。

期限が来ても保存されてから返却され、返却後も`retention.ended_worktree`の間は実体が残り`wx shell --resume <id>`で復元できる。
ただしsnapshot後の編集は保存されない。
`Manager.snapshotSession`の`processAlive(AgentPID)`ガードは`path`貸出では効かないため、期限到来時にSubAgentがまだ編集中のworktreeのsnapshotを取ることは起こり得る。
`wx new`の主返却契機は親sessionの終了に置き、終わったら`wx release`で返す。

## 変更の入口と代表テスト

再開のclient側入口は[`internal/cli/client.go`](../internal/cli/client.go)と[`internal/cli/fresh.go`](../internal/cli/fresh.go)である。
daemon側の入口は[`internal/daemon/resume.go`](../internal/daemon/resume.go)である。
代表テストは`internal/daemon`の[`TestLeaseArchiveAndRestorePreservesGitState`](../internal/daemon/resume_integration_test.go)で、貸出からsnapshot・復元までGit状態が保たれることを通す。

エージェント起動以外への貸出の入口は[`internal/cli/lease.go`](../internal/cli/lease.go)と[`internal/daemon/leasekind.go`](../internal/daemon/leasekind.go)である。
代表テストは[`TestPathLeaseSurvivesOrphanReconcileAndExpiresThroughSnapshot`](../internal/daemon/leasekind_test.go)で、`wx new`の貸出がorphan回収を生き延び、期限到来で保存経路を通ることを通す。
