# セッションと復元

1. **貸出** — `wx claude`はworkspace rootのworktree方針を先に解決する。
   未定義の対話起動は`internal/tui.Select`で選択を保存し、対象外なら現在のCWDで通常起動する。
   worktreeを使う場合は`ensureDaemon`でdaemonの生存を確認し（応答が遅いだけの生きたdaemonをlaunchdで再起動しないよう、接続自体に失敗したときだけkickstartする）、`ResolveAndLease`を呼ぶ。
   daemonはcwdからworkspaceを解決し、要求したOIDと完全一致するREADY slotがあれば再利用、無ければPREPAREジョブを積んで準備中のpathを返す。
2. **起動** — clientはleaseのpathをdescriptorとして開き、`internal/fdexec`経由でエージェントをそのdescriptorのディレクトリで起動する。
   子プロセスには`WX_SESSION_ID`・`WX_SESSION_TOKEN`・`WX_DAEMON_SOCKET`などが渡り、以降のhookはこれを持つ場合だけ動く。
3. **準備完了のゲート** — 準備が終わっていないworktreeでエージェントが動き出さない仕組みは2通りある。
   hookが入っていれば、`wx hook user-prompt-submit`と`wx hook pre-tool-use`が`WaitReady`をブロッキングで呼ぶので、準備とエージェント起動を重ねられる。
   hookが無ければ、client側が起動前に前面で`WaitReady`を待つ（`hookconfig.Available`で分岐）。
   `wx hook session-start`は`BindAgentSession`系でエージェント側のネイティブなセッションIDをwxのセッションへ結び付ける。
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
   確認は`resume.auto_fresh`が真なら省き、端末がなければnoticeを出して再開を続ける。
   やり直しは1回だけで、2回目の失敗はそのまま返す。
   ネイティブresumeは遅延バインドや`_unbound` slotを新規生成せず、clientが準備完了を前面で待ってから起動する。
   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

## 変更の入口と代表テスト

再開のclient側入口は[`internal/cli/client.go`](../internal/cli/client.go)と[`internal/cli/fresh.go`](../internal/cli/fresh.go)である。
daemon側の入口は[`internal/daemon/resume.go`](../internal/daemon/resume.go)である。
代表テストは`internal/daemon`の[`TestLeaseArchiveAndRestorePreservesGitState`](../internal/daemon/resume_integration_test.go)である。
これは貸出からsnapshot・復元までGit状態が保たれることを通す。
絞って動かすなら`make test-focus PKG=./internal/daemon RUN=TestLeaseArchiveAndRestorePreservesGitState`とする。
この実行は[部分検証](worktree-copy.md#部分検証)であり、最終判定は`make ci`とする。
