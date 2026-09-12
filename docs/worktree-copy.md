# worktreeのコピーとリンク

`repository_defaults.storage.copy_mode`と`repository_defaults.cow_min_size_kib`の取り得る値・既定値・fallbackは`wx config --help`を参照する。
どちらも membership 単位で個別指定でき、貸出1回の`--config`上書き、`workspaces.<root>.repositories.<relative>`、workspaceの `repository_defaults`、global defaults の順に解決する。
fingerprintにはrepositoryごとに解決した実効値を入れるので、schemaを上げずに、値が変わったrepositoryのREADY slotだけを無効にできる。

共有には配置と置換の二方式がある。
新規準備は配置方式で、共有できるtracked fileをcheckoutせずmainからcloneして置く。
復元とHot StandbyのUPDATEは置換方式で、checkout済みのファイルを同内容のcloneと入れ替える。
どちらも共有下限未満のファイルと、main側indexのblob OIDが宛先indexと異なるpathを走査の前に共有対象外とする。
小さいファイルはブロック共有で減る容量より判定の定数費用が勝ち、OIDが違うpathは内容まで一致することが稀だからである。
ただしOIDの一致は共有の根拠ではなく候補を絞る事前skipにすぎず（mainがdirtyなら内容は違う）、不一致でも内容が一致する回の共有は諦めている。

`cow`は全ファイルの共有や削減容量を保証する指定ではない。
共有下限を0にしても無くなるのは下限による除外だけで、上の事前skipは残る。

下限は `repository_defaults.cow_min_size_kib` と、membership 個別の `workspaces.<root>.repositories.<relative>.cow_min_size_kib`（未指定なら上位を継承）で決まる。
個別指定を許すのは、最適な下限がrepositoryのファイルサイズ分布に依存するためである。
fingerprintにはrepositoryごとに解決した実効値が入るので、個別指定を足しても、実効値が変わらなかった他repositoryのREADY slotは再利用できる。

## 配置方式（新規準備）

残りのcheckoutより前に候補をcloneし、置けたpathをcheckoutの対象から外す。
checkoutしてから同内容へ差し替えるのに比べ、同じbytesの書き出しと読み比べが1往復ぶん要らない。

内容が要求OIDと一致するかは自前で比較せず、cloneの直後にGitのtracked検査へ判定させ、一致しないpathだけを`checkout-index --force`でやり直す。
したがってmainがdirtyなpathやclone中にmainが変わったpathは通常checkoutへ落ちるだけで、準備は成功し共有されないpathが増える。
やり直しても一致しないpathが残る回だけ準備を失敗させる。
この検査はcloneしたファイルがindexにstat情報を持たないために内容を実際に読むので、貸出前の`tracked-status-refresh`はstatの確認だけで済む。

tracked検査はclean filterを通した一致しか見ないため、変換の入るpathは配置の候補から外す。
外さないと、mainの未コミット内容がblobへ戻る限り検査を通り、通常checkoutと違うbytesが宛先に残る。
除外の対象は`check-attr --cached --all`が変換系の属性を報告するpathで、`core.autocrlf`が有効な回とcheckoutの属性を要求OIDから読む回は1件も置かない。

cloneはmode・xattr・ACLを元の実体から複製する。
modeの差は要求OIDとのtracked検査で通常checkoutへ落ちるが、file flagsが付いた実体は置くとslotが書換えも削除もできなくなるため候補から外す。
xattrとACLは複製したまま貸し出すので、置換方式が持つmetadata一致の条件までは揃えない。

置けなかった候補が1件でも残る回は、貸出前の置換方式を省かない。
省くと、配置から外れた候補をどちらの方式でも共有しないまま貸し出すことになる。

配置方式は既存のファイルを消さないので、`.wx-cow-*`の一時ファイルを作らない。

## 置換方式（復元・Hot StandbyのUPDATE）

対象はmain worktreeの同じpathにある通常ファイルのうち、cloneしたbytesと宛先の最終bytesが一致するものだけである。
mainとcommitが異なっていても同内容のファイルは共有でき、dirtyなmainの変更は宛先へ持ち込まない。
metadata（所有者・mode・flags・ACL・xattr）の不一致、mainのtree形状の差、mainの処理中の変化は、そのファイルだけを共有対象外として残りの処理を続ける。

候補はindex全体だが、Hot StandbyのUPDATEだけは今回の更新が書き直したpathへ限定する（`updateCOWScope`）。
限定の根拠は「checkoutが触らなかったpathの実体は前回の準備のまま残る」ことだけで、OIDの一致は根拠にしない。
集合は旧OIDと要求OIDの差分に、`slot_placements`の旧履歴と新計画に挙がったpathを足したものである。
したがって共有の水準は準備時に決まり、更新では増えない。前回共有できなかったpathをmainの状態が変わってから拾い直すことはしない。
復元と新規準備は限定しない。宛先に共有済みの実体が無いため、絞ると共有が減るだけになる。

置換は復元の完了前に行い、Gitのfilter、checkout hook、prepare commandによる結果を保持する。
indexはstat情報のrefreshだけを行い、staged/unstagedの区別は変えないため、復元した区別も保たれる。

所有権不明は`auto`でもfallbackせずQUARANTINEDとして実体を残す。
中断して残った未追跡の`.wx-cow-*`も自動削除せず隔離するため、この名前は予約する。
この検出は無視されたtreeを走査しないので、`.wx-cow-*`をgitignoreで無視すると残骸を検出できなくなる。

## 共通

Darwinでは`Fclonefileat`を使い、Linuxでは`auto`が通常方式、`cow`がエラーになる。
`auto`が通常コピーへ落ちた回はdaemonのログにwarnとして残す。握り潰すと、CoWが効いていないことを知る手立てが無くなる。
clone元と宛先は同じ対応volumeにある必要があり、通常checkout1個分の一時容量は必要である。

コピー方式と共有下限はfingerprintに含めるため、設定変更後の貸出では以前の設定で作ったREADY slotを再利用しない。
`wx config`での保存はdaemonのreloadを起こし、reloadは保守を即時に一巡させるので、fingerprintが変わると全workspaceの既存READYがSTALEになりcoldで補充される。
設定を戻した場合も同じくreloadが走り、作り直しの費用（cold準備1本ぶん × `workspaces.<root>.warm_count`）がもう一度かかる。
`.worktreeinclude`、workspace rootのコピー、生成物、Git objectsや復旧snapshotの容量は、この設定の対象外である。

Hot StandbyのUPDATEは旧HEAD・tracked clean・所有権を確認してから、要求時に固定したOIDへdetachedのまま切り替える。
更新用Git操作だけは`core.hooksPath=/dev/null`をコマンド単位で指定し、checkout filterと属性処理は維持する。
`.gitattributes`の差、submodule構成・gitlink変更、未登録のuntracked/ignored pathとの衝突、更新互換fingerprintの不一致は書込み前にCold Startへ戻す。
`.gitattributes`を除外するのは、`checkout-index`が内容の同じfileをstat cacheの一致で書き直さず、属性だけ変わったfileが旧属性のまま残るためである。
更新では`prepare.command`を実行しない。
lockfileのようにOIDへ依存する生成物は更新後も旧OIDのまま残るので、都度の再生成が必要な場合は`workspaces.<root>.reuse_standby: false`で更新を止める。

`.worktreeinclude`対象のファイルは内容のhashがfingerprintに入るため、1 byteの書き換えでもREADY全本が不一致になる。
この不一致は貸出時のUPDATEで解消できるが、待たせないよう保守の一巡が待機中のREADYを先回りで更新する（[daemonの補充と回収](daemon-maintenance.md)の「standby補充」）。
editorが書き換えるような設定ファイルを更新の契機にしたくない場合は、`.worktreelink`へ移すとsourceの実体へのsymlinkになり、内容はfingerprintに入らない。

## include / link

`.worktreelink`に列挙したpathは、main worktree側の実体へ直接symlinkする。
sourceが存在しない項目は、ファイル・ディレクトリを問わずその準備では省略し、sourceの出現・消失はfingerprintの存在状態変更としてslot再利用を止める。
sourceがsymlinkの項目と、ソースリポジトリのignore対象でない項目も同じく省略し、省略した対象と理由をdaemon logにwarnで残す。
path逸脱・権限エラーや宛先衝突は省略せず、準備を失敗させる。
同じ扱いはworkspace rootのcopy/link sourceと`.worktreeinclude`の一致にも適用し、既定名と明示名で挙動を分けない。

ignore判定は`git check-ignore`に委ねるので、ディレクトリを列挙するときは`/dir/*`ではなく`/dir`の形のruleが要る。
また配下にtracked fileを1つでも持つディレクトリは、ignoreを通せても対象にならない。tracked fileのcheckoutが実体を作り、次の宛先衝突で準備が失敗するためである。

workspace内の相対位置を保って再構成する処理は持たず、必要になったら`~/.config/git/hooks/worktreelink-post-checkout`に実装済みのアルゴリズムを移植する。

### 非Gitのworkspace root

multi-repository workspaceのrootにはcheckoutが無く、root直下の実体はcopy/link ruleに載ったものだけがslotへ入る（既定名とroot直下manifestの書式は`wx config --help`）。
agentはslotのworkspace rootをCWDとして起動するので既定でagent資産を持ち込むが、`.claude`を丸ごとは入れない。
`settings.local.json`やmailboxのように実行中に書き換わる実体をfingerprintへ混ぜると、standbyの更新が止まらなくなるためである。
欠落を準備失敗にするのは`workspaces.<root>.copy`で明示したpathだけで、既定名とincludeのglobは0件を許す。
既定名は`.worktreelink`が所有するpathを譲り、rule衝突にしない。利用者が書いていない暗黙の追加が、明示したlinkを止めてはならないためである。
includeのglobと`.worktreelink`に同じpathを書いた場合は利用者が明示した矛盾なので、そのまま準備失敗にする。
rootのlinkだけはignore判定を行わず（rootにGitが無い）、sourceの欠落も省略ではなく準備失敗として扱う。

root直下のmanifestは、workspace rootがrepositoryのmain worktreeそのものである場合には読まない。
そこはGitがcheckoutする領域で、配置しない実体をfingerprintへ混ぜると無関係なREADY slotを一斉に無効化する。

rule解決は1箇所に集め、fingerprint・配置計画・配置履歴が同じ結果を見るようにする。
workspace rootのtar・復元での除外はruleで決めない。
snapshotはslot内でsymlinkだったpathだけを外し、復元の除外はarchiveが持つpathから決める。
ruleを読み直して除外を決めると、貸出中のrule変更で実体のある作業がtarから落ちる。

新規準備は実際に配置したcopy/linkをfile単位で`slot_placements`へ記録する。
記録はその準備が配置に使った計画そのものから作り、include/linkのruleを読み直さない。
1つのPREPARE jobがruleを2度読むと、その間のrule変更で配置済みの実体と記録が食い違い、slotごと隔離される。
copyの`content_sha256`もsourceではなく配置先から読み、記録がworktreeの実体を表すようにする。
配置時に省略したlinkは実体が無いので記録に載せない。
UPDATEは現行ruleとcopy元から作る新計画を旧履歴と比較し、追加・変更・削除とcopy/link切替を反映する。
削除対象は記録済みpathだけなので、同じdirectoryにある履歴外生成物は保持する。
配置履歴のない既存slotは完全一致なら貸出せるが、更新には使わない。

## submodule

linked worktreeではGitがsubmoduleのgitdirを`$GIT_DIR/modules/<name>`（=`.git/worktrees/<id>/modules/<name>`）に解決するため、mainが持つ`.git/modules/<name>`を再利用できない。
何もしないとsubmoduleは空ディレクトリのまま残り、ネットワークclone以外に埋める手段がない。
そこでwxはmain側の`.git/modules/<name>`をclone元として実体化する。
objectsはローカルcloneのhardlinkで共有され、共有`.git`側のディスクは増えない。

cloneに必要なconfigは必ず`-c`引数で渡す。
`internal/gitx`の環境サニタイズが`GIT_CONFIG_*`を落とすため、repo-local configや環境変数では子のcloneプロセスに効かない。
この形は共有`.git/config`へ何も書かないので、ソースリポジトリは不変のまま保たれる。
この性質に依存しているため、`internal/workspace/submodules_test.go`の`TestPrepareLeavesSourceRepositoryUnchanged`で恒久的に固定する。

cloneの直後にsubmoduleの`origin`をローカルmoduleの`remote.origin.url`へ戻す。
戻さないと`git push`がmainの`.git/modules`へ入る。`.gitmodules`のurlは`../child`のような相対表記の解決がsuperprojectのremote基準になるので、自前で解決するとGitと食い違う。

`.gitmodules`のurl欠落、ローカルmoduleの不在、ローカルmoduleにgitlink OIDが無いこと、ローカルmoduleのorigin欠落は、**書き込む前に**判定して省略し、warnを残して準備は成功させる。
gitlink OIDを解決できないまま実体化を始めると親がdirtyな`M <path>`で残り、その`$GIT_DIR/modules/<name>`は次回以降も古いgitdirを掴む。
この状態に到達させないことが設計の要点なので、判定は全て書込み前に済ませ、書き始めた後の失敗（clone・checkout・set-url）とnameの不正は省略せず準備を失敗させる。
`.git/worktrees/<id>/modules/<name>`はpinしたroot descriptorの外なので、wxはここを個別に削除しない（[所有権証明](ownership.md)）。

実行位置は残りの展開のpost-checkoutより前で、EARLY READYには含めない。
post-checkoutより前にするのは、ユーザーのhookがsubmoduleの中身を前提にできるようにし、hook側の`git submodule update`もno-opで済ませるためである。
単発準備・restore経路では`completePrepare`のinclude配置より前に実体化し、`.worktreeinclude`やprepare commandがsubmodule配下を前提にできるようにする。
standbyのUPDATE経路は再同期しない。`rejectChangedGitlinks`が`.gitmodules`とgitlink OIDの完全一致しか通さないため、更新で実体が陳腐化することはない。

方針は `repository_defaults.submodules` と、workspace の `repository_defaults.submodules` または membership 個別の `submodules` で切り替える。
準備用fingerprintと更新互換fingerprintの両方に混ぜて、方針変更後に旧方針のREADY slotを再利用しない。
更新互換側にも要るのは、更新経路がsubmoduleを実体化しないため`submodules=false`で作ったstandbyをtrue相当へ変換できないからである。

**worktree内のsubmoduleで作ったコミットはslot削除で失われる。**
snapshotはgitlinkしか記録できず（`internal/archive`の一時indexへの`add -A`も同じ）、救う手段を持たないためである。submodule側の変更はpushしてからslotを返す。
submodule checkoutのCoW共有も行わない。prepareのCoWフェーズより後に実体化するため、1 slotあたりのcheckout分は共有されない。
入れ子submoduleの再帰（`--recursive`）は扱わない。

## 起動用ファイルの先行配置

通常準備は全リポジトリのGit登録・先行配置、残りの配置の二巡で行う。
tracked fileは元worktreeの未コミット内容を取り込まず、Gitのfilter・属性・実行権限・symlinkの形を保持する。
残りの展開ではGitのparallel checkoutを使い、並列度はリポジトリ設定に依らずwxが毎回指定する。
残りの展開は、共有できるtracked fileのclone、残りのcheckout、内容の照合の順で行う。
post-checkoutは全tracked fileの配置後、残りのinclude/link・prepare commandより前に一度だけ実行する。

先行配置した未追跡ファイルに`.gitattributes`がある回だけ、checkoutの属性を要求OIDから読み、後段のfilterが変わることを防ぐ。
無い回に読み直さないのは、worktree上の`.gitattributes`が既に要求OIDの内容と一致し、treeからの属性再読込が大きなリポジトリではcheckout全体を目に見えて遅らせるためである。
この回はcheckoutと配置後のtracked検査が違う属性を見るため、配置方式を使わず置換方式へ共有を任せる。
`.worktreelink`のlinkはソースリポジトリのignore対象に限るためtracked fileの祖先にならず、配下の`.gitattributes`は参照されないので数えない。

先行候補は`internal/workspace/includes.go`の`defaultEarlyPaths`に集約する。
既定include名、トップレベルの指示ファイル、各エージェントの設定ディレクトリとGitHubの指示・agentディレクトリが対象になる。
`repository_defaults.readiness.early_paths`は候補への追加である。
workspace の `repository_defaults.readiness.early_paths` と membership 個別の同キーは上位 list の置き換えで、`defaultEarlyPaths`は置き換えても残る。
非Gitのworkspace rootのステージはglobal値を使う。rootの規則はどのrepositoryにも属さず、和集合は「早期に出さない」約束を破り、積集合は空になりやすいためである。
`src/AGENTS.md`のような深い指示ファイルは自動収集しない。
配置に必要なignore・attributeと、tracked symlinkが指す予定済みの内部パスも先行させる。
先行配置したファイルを、残りの配置で再checkout・再コピー・再リンクしない。
非Gitのmulti-repository workspace rootにも同じ選択規則を適用し、架空のGit登録は作らない。

設定ファイルから参照先を自動解析する機能はなく、必要な静的ファイルは通常の配置規則と`early_paths`へ追加する。
hookやprepare commandが生成するものは先行配置できないので、[セッションと復元](session-lifecycle.md)の起動ゲート側で扱う。
readiness設定の変更だけでは完成済みREADY slotの再利用を無効化しない。

## 実装の分割

`internal/workspace/cow_clone.go`はcloneの呼び出しと、cloneが成功した後にしか進まない処理だけを持つ。
linuxでは`cloneCOW`がENOTSUPを返して到達しないため、`coverage-exclusions.txt`で理由付きにcoverageの分母から外している。
linuxでも実行される判定・分割・集約は除外していないので、clone後の処理を足すときは`cow_clone.go`へ置き、cloneの有無に依らず成立する契約は他のファイルへ置く。

## 部分検証

darwin専用実装（`cow_darwin.go`・`usage_darwin.go`）のテストは、macOSであれば`make ci`・`make test`・`make test-focus PKG=./internal/workspace`のいずれでも走る。
`cowAvailable`はdarwinで常にtrueを返しfilesystemを見ないため、非APFSの一時ディレクトリではclonefileが効かず、失敗が実装の不具合と区別できない。
そこで`internal/workspace`の`TestMain`が`statfs`でTMPDIRを判定し、APFSでなければテストを走らせずに前提未成立として終える。

これらの実装を触ったら、macOS実機で`make test-darwin`も実行する。
`make build-darwin`はarm64のクロスビルドだけを見るのでテストの実行を保証せず、CIのランナーはすべてlinuxのままとしてmacOS runnerは用意しない。
Linux側の退行は`CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`で確認する。
