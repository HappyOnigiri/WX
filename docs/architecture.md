# アーキテクチャ

`AGENTS.md`の不変条件が、コードのどこでどう実現されているかを示す。

## 層とパッケージ

依存はおおむね上から下へ流れるが、厳密な一方向ではない（例えば`internal/state`は`internal/discovery`のworkspace型を受け取る）。
lintが機械的に守るのは`.golangci.yml`のdepguardが書いた`internal/domain`と`internal/state`の2つぶんだけで、それ以外の辺は規約でしか守られていない。

- **入口** — `cmd/wx`、`internal/cli`。
  引数解析、daemonへのRPC、エージェント子プロセスの起動と信号中継。
- **エージェント連携** — `internal/agent`、`internal/hookconfig`。
  hookの実行と、hook設定が同期的な準備完了契約を満たしているかの判定。
- **転送** — `internal/rpc`。
  Unix socket上の長さ前置きJSON、冪等性キーによる重複実行の抑止。
- **調整** — `internal/daemon`。
  ジョブのworker、貸出と返却、standby補充、reconcile、GC、status/doctor。
- **業務** — `internal/workspace`、`internal/archive`、`internal/pool`。
  worktreeの準備と削除、スナップショットと復元、branch指定の解決。
- **永続化** — `internal/state`、`migrations`。
  SQLiteの状態と所有権証明。
- **探索** — `internal/discovery`。
  workspaceとリポジトリの解決。
  `state`がこの型を受け取るので、層としては`state`より下に位置する。
- **基盤** — `internal/domain`、`internal/gitx`、`internal/fdexec`、`internal/config`、`internal/launchd`。
  path・ID・descriptorの原始的操作、Git実行、fchdir束縛、設定、LaunchAgent。

`internal/daemon/manager.go`はstatus/doctorの組み立ても持つ。
診断用の独立したパッケージは、分割の便益が間接参照の増加を上回らないので作っていない。

`wx`のバイナリは3つの役割を兼ねる。
CLI、`wx daemon`のサーバー本体、そして`internal/fdexec`のexecトランポリン（隠しコマンド`__wx_exec_at_fd`）である。
descriptor束縛でGitやエージェントを起動する経路は、必ず自分自身を再execする形になっている。

## 1セッションのライフサイクル

1. **貸出** — `wx claude`はworkspace rootのworktree方針を先に解決する。
   未定義の対話起動は`internal/tui.Select`で選択を保存し、対象外なら現在のCWDで通常起動する。
   worktreeを使う場合は`ensureDaemon`でdaemonの生存を確認し（応答が遅いだけの生きたdaemonをlaunchdで再起動しないよう、接続自体に失敗したときだけkickstartする）、`ResolveAndLease`を呼ぶ。
   daemonはcwdからworkspaceを解決し、要求したOIDと完全一致するREADY slotがあれば再利用、無ければPREPAREジョブを積んで準備中のpathを返す。
   一致しなければ再利用せずcold startになる。
2. **起動** — clientはleaseのpathをdescriptorとして開き、`internal/fdexec`経由でエージェントをそのdescriptorのディレクトリで起動する。
   子プロセスには`WX_SESSION_ID`・`WX_SESSION_TOKEN`・`WX_DAEMON_SOCKET`などが渡り、以降のhookはこれを持つ場合だけ動く。
3. **準備完了のゲート** — 準備が終わっていないworktreeでエージェントが動き出さない仕組みは2通りある。
   hookが入っていれば、`wx hook user-prompt-submit`と`wx hook pre-tool-use`が`WaitReady`をブロッキングで呼ぶので、準備とエージェント起動を重ねられる。
   hookが無ければ、client側が起動前に前面で`WaitReady`を待つ（`hookconfig.Available`で分岐）。
   `wx hook session-start`は`BindAgentSession`系でエージェント側のネイティブなセッションIDをwxのセッションへ結び付ける。
4. **返却** — `wx hook session-end`が`Release`を呼ぶ。
   ただしsession-end hookはエージェント本体より先に走ることがあるため、clientまたはエージェントのプロセスが生きている間は返却しない。
   前面clientの終了通知か、daemonのorphan reconcileが「もう書き手がいない」ことを確認してから先へ進む。
5. **アーカイブ** — SNAPSHOTジョブが2種類のスナップショットを取る。
   リポジトリごとのHEAD・index・worktreeは`refs/wx/recovery/<session>/<repo>/*`として**ソースリポジトリ側の**保護オブジェクトとrefになる。
   multi-repository workspaceではさらに、workspace root自体のtarをwxのworktree root配下（`recovery/workspace-snapshots/`）へ書き、`workspace_snapshots`行が指す。
   refの公開はDB行の永続化の後に行う。
   逆順だと、reconcileから見て正常なアーカイブが素性不明のrefに見える窓が開く。
6. **再開** — `wx resume`または各エージェントのネイティブなresumeが`Resume`/`AllocateResumeSlot`を通り、RESTOREジョブがスナップショットを新しいslotへ戻す。
   ネイティブresumeはhookが揃っていることを要求し、揃っていなければ前面で待つ`wx resume <wx-session-id>`へ倒す。
   復元後のworktreeはtracked changesを含むため、貸出前の検査はcleanなworking treeを要求しない`ValidateOwnership`を使う。
   READY slotの再利用側は`ValidateReady`で、こちらはtracked cleanまで求める。

## daemonの内部

- **ジョブ** — 永続ジョブは`jobs`テーブルにあり、種別は`PREPARE`、`ENSURE_STANDBY`、`SNAPSHOT`、`RESTORE`、`REMOVE`、`REMOVE_REPOSITORY`。
  workerがリースを取って実行する。
  失敗の包み方は`retryableJobError`と`dependencyPendingError`の2種である。
  **所有権失敗はどちらにも包まず終端させる**（証明できない実体に対する再試行は危険側に倒れる）。
- **standby補充** — worktree方針が`hot`で、直近に使われたworkspaceに対して、READYなslotを`pool.warm_per_workspace`まで先回りで用意する。
  `Store.HotRepositoryIDs`は`repositories.last_leased_at`でリポジトリ単位に絞る形をしている。
  その列を更新する側は5箇所すべてworkspace単位であるため、同じworkspaceのリポジトリを一律に同じ時刻へ揃える。
  そのため、貸出直後の`ENSURE_STANDBY`では全リポジトリがhotと判定され、このフィルタは実質効かない。
  粒度を上げるには、貸出時に実際に使ったリポジトリを`session_repositories`側へ記録し、`HotRepositoryIDs`とGCの`ColdRepositoryCandidates`の両方をそこから読ませる必要がある。
- **clean** — `wx clean`は保持期限を待たずにworktreeを削除する。
  受付時点で全workspace・全root世代のslotから対象を確定し、`clean_runs`・`clean_targets`へ永続化するので、daemon再起動後も同じ対象と期限で再開する。
  進行は`Manager.driveClean`のbackground goroutineが既存ジョブの完了を監視するだけで、workerを占有したまま別ジョブを待たない。
  削除そのものは通常の返却・保存・削除経路（`Release`→`SNAPSHOT`→`ScheduleRemoval`→`REMOVE`）に載せるので、GCと二重の削除実装を持たない。
  `--all`は`session_termination_requests`へ期限付き（30秒）の終了要求を記録し、heartbeatとagent登録の応答でclientへ渡す。
  signalを送るのはclientだけで、daemonは記録されたPIDへ触れない。
  期限内に停止を確認できない対象は失敗として閉じ、遅れて終了しても通常の返却処理に戻す。
  run実行中は`assertNoActiveClean`が貸出・復元・待機用作成の書き込みトランザクションを断るので、予約した対象が新しいsessionへ渡ることはない。
  削除後の補充停止は`replenish_suspensions`に永続化し、`ensureStandby`が定期reconcileと補充ジョブの双方で参照する。
  解除はそのworkspaceの貸出・resumeが成功した時点だけで、既存sessionの返却では解除しない。
  安全な処理境界を待つ対象には`cleanBoundaryWait`（5分）の上限を置く。
  無期限に待つと、貸出を断ったままworkspace全体が使えなくなるためである。
- **reconcile** — 定期的にDBと実体を突き合わせ、素性の分からないpath・refを隔離側へ倒す。
  clientとエージェントの両プロセスが死んでいるsessionはここで返却される。
- **degraded運用** — SQLiteが開けないときも`Status`・`Doctor`・`RequestStop`は`DegradedHandler`が答える。
  診断のためにdaemonを完全に沈黙させないためである。
  `RequestStop`だけは状態を変えるがゲートを通さない（状態を変えるRPCを一切受け付けない以上、守るべきin-flightの予約が無い）。
- **restart / stopのidleゲート** — `wx daemon restart`・`wx daemon stop`とバイナリ差し替えの自動検知は、いずれも`restartPending`・`stopPending`を立てるだけである。
  実行は`maintainJobs`から`runPendingLifecycle`が駆動する。
  ゲートは「in-flightのRPCが0」「最後のRPC終了から`lifecycleQuietPeriod`（5秒）経過」「`jobs`が0」を要求する。
  SIGTERMがin-flightのRPCを救わない（応答を書かずに接続が閉じる）以上、これが唯一の保護である。
  1回のwx起動が独立した複数のRPCの列である以上、in-flightが0になった最初の瞬間は安全な瞬間ではないので、quiet periodを別に要求する。
  ただしquiet periodを数える対象から`RequestStop`・`RequestRestart`・`RequestStart`自身は外す（`Handler.Handle`が`isLifecycleMethod`で振り分ける）。
  これらは守るべきユーザー操作ではなくゲートを待っている側であり、数えるとidleなdaemonでも自分が作ったquiet periodを5秒待つことになる。
  in-flightの数え上げからは外さない。
  応答フレームは`Handler.Handle`が戻った後に書かれるので、代わりに`lifecycleReplyGrace`（100ミリ秒）だけゲートを閉じたままにして、受理を伝える応答がそのsignalに切られないようにする。
  restartは`underLaunchd()`も要求する（手動起動のdaemonがkickstartすると二重起動になる）が、stopは要求しない。
  この判定は`launchd_managed`として応答にも載るので、CLIは来ない置換を待たずに断れる。
  要求は互いを打ち消し、同時に2つはpendingにならない。
  ただし打ち消せるのはsignalに渡される前までで、渡した後に届いた逆の要求は`conflict`として断る（配送済みのsignalは呼び戻せず、受理したように答えると呼び出し側が来ない状態を待ち続ける）。
  `wx daemon start`は起動済みのdaemonにも`RequestStart`を送る。
  CLIが待ちきれずに諦めたstopはpendingのまま残るので、signalに渡される前ならここで取り消す。
  渡された後は取り消せないため、CLIは終了を待ってからlaunchd経由で起動し直す。
  ゲートを通過したら、restartは`launchd.Kickstart`、stopは自プロセスへのSIGTERMを1度だけ発行する。
  同期待ちするCLI側は、stopとstartはsocketへのdial可否だけを見る（`Status`をpollingすると`lastRequestEnd`が更新され続けてゲートが永久に開かない）。
  restartだけは、listenerの消失を観測した後と、待ち切れた後の2箇所で`Status`のpidを読む。
  50msのプローブはkickstartが閉じて開き直す数ミリ秒を丸ごと取りこぼしうるので、置換の判定は観測した断ではなくpidの比較で行うためである。
  前者はkickstartが済んだ後なのでゲートに影響しないが、後者は諦める直前の1回で、restartがまだpendingならquiet periodを押し出す。
  待っている間、CLIは`stopping...`のような行を出す。
  ゲートが開くまでdaemonは何も言わないので、これが唯一の生存表示になる。
  ドットのアニメーションはstdoutが端末のときだけで、pipeやファイルへ出しているときは待機行を一切出さない（`interactiveOutput`）。

## 所有権の証明

破壊的操作の前に求める証明は、次の3つが同時に一致することである。

1. **DBの行** — `ValidateWorktreeOwnership`が`roots`・`slots`・`slot_repositories`・`workspaces`・`repositories`・`workspace_repositories`の6表を結合して1行を読む。
   突き合わせるのは絶対pathではない。
   root世代（`roots.id`）・root相対のslot path（`slots.rel_path`）・slot内のリポジトリ配置名（`slot_repositories.dir_name`）・inode identity（`dir_identity`）の4つである。
   これに加えて、slotとリポジトリのstate、common dir、workspace内の相対pathを確かめる。
   読み取り専用トランザクションが囲むのはSELECTだけで、一致判定はcommit後に走る。
   identityは**fail closed**で、descriptorを握っている呼び出し元がidentityを渡したのに記録が空なら不一致として扱う。
   identityを渡さないのは、開くべきディレクトリが無い2つの場合だけである。
   worktreeがまだ存在しないprepare前の検査と、worktreeの実体が既に消えていてGitの登録だけが残っている削除（`archive.Manager.RemoveWorktree`のmissing-registration分岐）である。
   slotディレクトリ自体の証明（`ValidateSlotOwnership`）も同じで、削除直前の検査はpin済みroot descriptorから読んだ実inodeを渡す。
   `slots.dir_identity`を読み直して渡すと同じ行を自分自身と比べることになり、identity層が実効を失う。
2. **ファイルシステム上のマーカー** — slotディレクトリ直下の`.wx-owner-<repository_id>`に、slot ID・root ID・repository ID・common dirをJSONで書く（`version: 2`）。
   内容が一致しないマーカーは所有の否定として扱う。
   マーカーはworktreeの**親**に置く。
   worktree削除が中断されても、再試行時に所有権を証明できる唯一のディスク側証拠がこれだからである。
3. **Gitのworktree lock** — wx自身が付けた`wx:<slot-id>:READY`・`PREPARING`・`RESTORING`のいずれかであること（`domain.ValidWxLockReason`）。
   認識できない理由でlockされたworktreeは、wxのものではない。

このうち2と3は、pinしたroot descriptor配下の相対pathに対して行う。
`domain.OpenOwnedRoot`がrootをinodeごとpinし、`domain.PhysicalPathInfo`が全成分のsymlinkを拒否するので、検査と実行の間にpathを差し替えられても、差し替え先へ操作が届かない。
1のDB側は「何が正しいか」を答え、descriptorは「いま触っているものが本当にそれか」を答える。
DBが持つidentityはdescriptorが返す`dev:ino`と同じ形式なので、2つの層が同じ対象を指していることを比較できる。
`workspace_repositories.relative_path`はソース側でのリポジトリ位置という本来の意味だけを担い、slot内の配置は`slot_repositories.dir_name`が持つ。

## ディスク上のレイアウト

worktree root（`storage.worktree_root`、既定`$HOME/wx`）配下は次の形になる。

```text
<worktree_root>/<workspace-id>/<slot-id>/<RepoName>/
<worktree_root>/_unbound/<slot-id>/
<worktree_root>/_recovery/workspace-snapshots/<安定ID>.tar
```

workspace-idとslot-idは6桁固定の小文字英数字（base36、`domain.NewShortID`）である。
slot-idはsession IDと同値なので、`wx sessions`が出すIDをそのまま`wx resume`に渡せる。
大文字を混ぜないのはAPFSが既定でcase-insensitiveなためで、同じ理由からslot内の配置名の衝突判定も小文字化して行い、衝突したら`-2`のサフィックスを付ける。

`_`始まりはwxの予約プレフィックスで、workspace IDもリポジトリ配置名もこの接頭辞を拒否する。
孤児スキャン（`ownedRootArtifactPaths`）はこの規則の裏返しで、`_`始まりを除く全ての第1階層をworkspaceディレクトリとみなし、その直下をslotディレクトリとして列挙する。
生成側（`slotRelPath`）と列挙側は同時に直さなければならない。片方だけ直すと、孤児検出が無音で機能停止する。

エージェントへ貸し出す単位（`Lease.Path`）は、単一リポジトリworkspaceなら`<slot-id>/<RepoName>`、multi_repositoryなら`<slot-id>`である。
単一リポジトリでCWDをリポジトリ直下にすることで、`.wx-owner-*`がCWDの親に残りエージェントから見えない。
`Lease.Path`と`state.Slot.Path`（常にslotディレクトリ）を混同しないこと。
workspaceが確定していない`_unbound/<slot-id>`はslotディレクトリ自体を貸し出す。
リポジトリ名がその時点では決まらないためで、単一リポジトリworkspaceに束縛された後もworktreeはCWDの1つ下に現れる。

`RepoName`の決定順は`repositories.<main path>.dir_name` → `repositories.<main path>.dir_source` → `storage.repo_dir_source`（既定`remote`）→ main worktreeのディレクトリ名である。
`remote`は`git remote get-url origin`の出力から末尾の`.git`を除いたbasenameで、取れないときはディレクトリ名へ落ちる。
採用した値は`slot_repositories.dir_name`に記録され、以後はその値が権威になる。
設定やremote URLが後から変わっても既存slotは記録済みの名前で動き続け、`workspace.Fingerprint`（`schema=3`）が名前を含むので新規slotから新しい名前になる。

`storage.worktree_root`を変えても既存slotは移動しない。
`roots`テーブルがroot世代を持ち、slotは`root_id` + root相対pathで位置を表す。
変更後は新しいrootが`active=1`になり、それまでのrootは`active=0`で残る。
旧root配下のslotは（READYのwarm slotも含めて）そのまま寿命を全うし、新規slotだけが新rootに作られる。
実体を移動しない理由は3つある。
gitのworktreeメタデータが絶対pathなので`git worktree repair`が要ること、別ファイルシステムへの変更ではrenameが失敗すること、中断時の再開規則が必要になることである。
いずれも単一ユーザー・単一マシンという前提に釣り合わない。
参照するslotもスナップショットも無くなった`roots`行はGCが削除するが、ディレクトリの実体は消さない。

multi_repositoryのworkspaceスナップショットは、slotディレクトリ自体をbundle rootとしてtarに詰める。
そのためリポジトリのworktreeと`.wx-owner-*`はどちらも除外リストに載せる（`workspaceRecoveryExclusions`）。
除外に使うのは`slot_repositories.dir_name`で、ソース側の`workspace_repositories.relative_path`ではない。
マーカーを除外しないと、archiveが別slotのIDを運び、復元前のpruneが現在のslotの所有権証拠を消してしまう。

状態は`~/Library/Application Support/wx/state.db`が持ち、同じ場所の`state.db.backups/`にオンラインバックアップを世代保存する。

リポジトリ単位の復旧スナップショットだけは**wxの領域ではなくソースリポジトリのGitオブジェクトとref**として置かれるため、そのリポジトリを読めるプロセスからは中身が読める。

## 未実装のまま残しているもの

- `.worktreelink`に列挙したpathは、main worktree側の実体へ直接symlinkする（`createLinksAt`）。
  workspace内の相対位置を保って再構成する処理は持たない。
  必要になったら`~/.config/git/hooks/worktreelink-post-checkout`に実装済みのアルゴリズムを移植する。
- 生成器を持たない。
  help本文、config schema、SQLite migration、LaunchAgent plistはすべて手書きで維持し、shell completionは実装しない。
  `make`の`generated-check`が現状なにも検証していない理由と、それでも残している理由は`Makefile`の同targetのコメントにある。
