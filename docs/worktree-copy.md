# worktreeのコピーとリンク

`storage.copy_mode`の値・既定値・fallbackと、共有下限`storage.cow_min_size_kib`の意味は`wx config --help`を参照する。

共有には配置と置換の二方式がある。
新規準備は配置方式で、共有できるtracked fileをcheckoutせずmainからcloneして置く。
復元とHot StandbyのUPDATEは置換方式で、checkout済みのファイルを同内容のcloneと入れ替える。
どちらも共有下限（`storage.cow_min_size_kib`、既定16KiB）未満のファイルと、main側indexのblob OIDが宛先indexと異なるpathは走査の前に共有対象外とする。
小さいファイルはブロック共有で減る容量より判定の定数費用が勝ち、OIDが違うpathは内容まで一致することが稀だからである。
下限は0（全ファイルを対象）から広げる方向まで設定でき、`storage.copy_mode`と同じくfingerprintに含めるため、変更後の貸出では以前の下限で作ったREADY slotを再利用しない。
`cow`は共有対象のclone失敗をエラーにする指定であり、全ファイルの共有や削減容量を保証する指定ではない。

## 配置方式（新規準備）

`internal/workspace/cow_place.go`が、残りのcheckoutより前に候補をcloneし、置けたpathをcheckoutの対象から外す。
checkoutしてから同内容へ差し替えるのに比べ、同じbytesの書き出しと読み比べが1往復ぶん要らない。
候補はmain側の実体が共有下限以上の通常ファイルであるpathで、OIDの一致は共有の根拠ではなく候補を絞る事前skipにすぎない。

内容が要求OIDと一致するかはcloneの直後にGitのtracked検査へ判定させ、一致しないpathだけを`checkout-index --force`でやり直す。
したがってmainがdirtyなpathやclone中にmainが変わったpathは通常checkoutへ落ちるだけで、準備は成功し共有されないpathが増える。
やり直しても一致しないpathが残る回だけ準備を失敗させる。
この検査はcloneしたファイルがindexにstat情報を持たないために内容を実際に読むので、貸出前の`tracked-status-refresh`はstatの確認だけで済む。

tracked検査はclean filterを通した一致しか見ないため、変換の入るpathは配置の候補から外す。
外さないと、mainの未コミット内容がblobへ戻る限り検査を通り、通常checkoutと違うbytesが宛先に残る。
`check-attr --cached --all`が`text`・`eol`・`crlf`・`ident`・`filter`・`working-tree-encoding`を報告するpathを外し、`core.autocrlf`が有効な回とcheckoutの属性を要求OIDから読む回は1件も置かない。

cloneはmode・xattr・ACLを元の実体から複製する。
modeの差は要求OIDとのtracked検査で通常checkoutへ落ちるが、file flagsが付いた実体は置くとslotが書換えも削除もできなくなるため候補から外す。
xattrとACLは複製したまま貸し出すので、置換方式が持つmetadata一致の条件までは揃えない。

置けなかった候補が1件でも残る回は、貸出前の置換方式を省かない。
省くと、配置から外れた候補をどちらの方式でも共有しないまま貸し出すことになる。

走査はpath順に連続したrunの塊へ分け、塊ごとにdirectory descriptorを共通接頭辞のぶん持ち越す。
entryごとにrootから全成分をたどると、深さに比例したopenatがdirectory数だけ繰り返される。
成分は必ず1つずつ`O_NOFOLLOW`で開くため、走査中にsymlinkを差し込まれてもpinした外へは出ない。
下限を超えるleafが1つも無いdirectoryでは宛先を作らず、後段のcheckoutに任せる。
所有権証明はSQLの照会・worktree identityの検査とも、塊の前後と一定件数ごとに行う。
配置方式は既存のファイルを消さないので、`.wx-cow-*`の一時ファイルを作らない。

## 置換方式（復元・Hot StandbyのUPDATE）

対象はmain worktreeの同じpathにある通常ファイルで、cloneしたbytesと宛先の最終bytesが一致するものだけである。
mainとcommitが異なっていても同内容のファイルは共有でき、dirtyなmainの変更は宛先へ持ち込まない。
新規・内容不一致・空ファイル・symlink・submodule・複数hard linkを持つ宛先は通常方式のまま残す。
OIDの一致は共有の根拠には使わない（mainがdirtyなら内容は違う）。逆に不一致でも内容が一致する回の共有は諦める。
mainのtree形状が異なる場合や、mainがこの処理中に変化した場合も、そのファイルだけを共有対象外として残りの処理を続ける。
所有者・mode・flags・ACL・xattrが一致しないものも共有対象外とする。

`internal/workspace/cow.go`が復元の完了前に処理し、Gitのfilter、checkout hook、prepare commandによる結果を保持する。
indexはstat情報のrefreshだけを行い、staged/unstagedの区別は変えないため、復元した区別も保たれる。
宛先の日時はFD経由で復元し、元ファイルとcloneをatomic swapしてから元inodeを検証して削除する。
走査は同一ディレクトリの連続したentryをrunとしてまとめ、runをバッチにして並列に処理する。
所有権証明はSQLの照会・worktree identityの検査ともバッチの前後で行う。
宛先への書込みはpin済みdescriptor経由なので、バッチの途中でslotのディレクトリが差し替わっても別のinodeへは書かない。
共有下限の判定は宛先のlstatだけで行い、下限未満のpathでは両側を開かない。
バッチが失敗した回は着手済みのバッチを完走させてから止めるため、共有できたファイルの集合は回ごとに変わる。
所有権不明は`auto`でもfallbackせずQUARANTINEDとして実体を残す。
中断して残った未追跡の`.wx-cow-*`も自動削除せず隔離するため、この名前は予約する。
この検査は無視されたtreeを走査しないので、`.wx-cow-*`をgitignoreで無視すると残骸を検出できなくなる。

## 共通

Darwinでは`Fclonefileat`を使い、Linuxでは`auto`が通常方式、`cow`がエラーになる。
clone元と宛先は同じ対応volumeにある必要があり、通常checkout1個分の一時容量は必要である。
コピー方式はfingerprintに含めるため、設定変更後の貸出では以前の方式で作ったREADY slotを再利用しない。
`.worktreeinclude`、workspace rootのコピー、生成物、Git objectsや復旧snapshotの容量は、この設定の対象外である。
`auto`が通常コピーへ落ちた回はdaemonのログにwarnとして残る。

Hot StandbyのUPDATEは旧HEAD・tracked clean・所有権を確認してから、要求時に固定したOIDへdetachedのまま切り替える。
更新用Git操作だけは`core.hooksPath=/dev/null`をコマンド単位で指定し、checkout filterと属性処理は維持する。
通常差分ではGitが変更したfileだけを置き換える。
`.gitattributes`の差、submodule構成・gitlink変更、未登録のuntracked/ignored pathとの衝突、更新互換fingerprintの不一致は書込み前にCold Startへ戻す。
`.gitattributes`を除外するのは、`checkout-index`が内容の同じfileをstat cacheの一致で書き直さず、属性だけ変わったfileが旧属性のまま残るためである。
更新では`prepare.command`を実行しない。
lockfileのようにOIDへ依存する生成物は更新後も旧OIDのまま残るので、都度の再生成が必要な場合は`workspaces.<root>.reuse_standby: false`で更新を止める。

## include / link

`.worktreelink`に列挙したpathは、main worktree側の実体へ直接symlinkする（`createLinksAt`）。
sourceが存在しない項目は、ファイル・ディレクトリを問わずその準備では省略し、sourceの出現・消失でslot再利用をfingerprintの存在状態変更により止める。
sourceがsymlinkの項目と、ソースリポジトリのignore対象でない項目も同じく省略し、省略した対象と理由をdaemon logにwarnで残す。
path逸脱・権限エラーや宛先衝突は省略せず、準備を失敗させる。
同じ扱いはworkspace rootのcopy/link source（`MaterializeRootAt`）と`.worktreeinclude`の一致にも適用し、既定名と明示名で挙動を分けない。
workspace内の相対位置を保って再構成する処理は持たず、必要になったら`~/.config/git/hooks/worktreelink-post-checkout`に実装済みのアルゴリズムを移植する。

新規準備は実際に配置したcopy/linkをfile単位で`slot_placements`へ記録する。
UPDATEは現行ruleとcopy元から作る新計画を旧履歴と比較し、追加・変更・削除とcopy/link切替を反映する。
削除対象は記録済みpathだけなので、同じdirectoryにある履歴外生成物は保持する。
配置履歴のない既存slotは完全一致なら貸出せるが、更新には使わない。

## 起動用ファイルの先行配置

通常準備は全リポジトリのGit登録・先行配置、残りの配置の二巡で行う。
`worktree add --detach --no-checkout`で登録し、要求OIDのindexを構築して`checkout-index`へのNUL区切りパス入力で分割展開する。
tracked fileは元worktreeの未コミット内容を取り込まず、Gitのfilter・属性・実行権限・symlinkの形を保持する。
先行配置した未追跡ファイルに`.gitattributes`がある回だけ、checkoutの属性を要求OIDから読み、後段のfilterが変わることを防ぐ。
無い回に要求OIDから読み直さないのは、worktree上の`.gitattributes`が既に要求OIDの内容と一致し、treeからの属性再読込が大きなリポジトリではcheckout全体を数秒延ばすためである。
この回はcheckoutと配置後のtracked検査が違う属性を見るため、配置方式を使わず置換方式へ共有を任せる。
`.worktreelink`のlinkはソースリポジトリのignore対象に限るためtracked fileの祖先にならず、配下の`.gitattributes`は参照されないので数えない。
残りの展開ではGitのparallel checkoutを使い、並列度はリポジトリ設定に依らずwxが毎回指定する。
残りの展開は、共有できるtracked fileのclone、残りのcheckout、内容の照合の順で行う。
post-checkoutは全tracked fileの配置後、残りのinclude/link・prepare commandより前に一度だけ実行する。
最終検証は従来どおり最後に行う。

先行候補は`internal/workspace/includes.go`の`defaultEarlyPaths`に集約する。
既定include名、トップレベルのAGENTS.md・CLAUDE.md・GEMINI.md、各エージェントの設定ディレクトリとGitHubの指示・agentディレクトリが対象になる。
`readiness.early_paths`は候補への追加で、空配列でも既定候補を維持する。
ルート相対のリテラルパスで指定し、ディレクトリなら配下を含める。globは使わない。
通常のcheckout・include・linkで配置予定のものとの共通部分だけを先行させるため、追加指定してもコピー対象自体は増えない。
`src/AGENTS.md`のような深い指示ファイルは自動収集しない。
配置に必要なignore・attributeと、tracked symlinkが指す予定済みの内部パスも先行させる。
先行配置したファイルを、残りの配置で再checkout・再コピー・再リンクしない。
非Gitのmulti-repository workspace rootにも同じ選択規則を適用し、架空のGit登録は作らない。

prepare commandやcheckout hookが起動用の設定・指示を生成または更新する運用では、`readiness.mode: full`を使う。
設定ファイルから参照先を自動解析する機能はなく、必要な静的ファイルは通常の配置規則と`early_paths`へ追加する。
readiness設定の変更だけでは完成済みREADY slotの再利用を無効化しない。

## 変更の入口と代表テスト

新規準備の配置は[`internal/workspace/cow_place.go`](../internal/workspace/cow_place.go)が入口で、共有対象の判定と差し替えの起動は[`cow.go`](../internal/workspace/cow.go)が持つ。
cloneの呼び出しと、clone成功後にしか進まない比較・metadata照合・swap・後始末は[`cow_clone.go`](../internal/workspace/cow_clone.go)にまとめてある。
clonefileそのもののsyscallは[`cow_darwin.go`](../internal/workspace/cow_darwin.go)にある。
indexの解析と事前skipは[`cow_index.go`](../internal/workspace/cow_index.go)、run分割・並列実行・エラー集約は[`cow_share.go`](../internal/workspace/cow_share.go)が持つ。

`cow_clone.go`はlinuxでは`cloneCOW`がENOTSUPを返して到達しないため、`coverage-exclusions.txt`で理由付きにcoverageの分母から外している。
linuxでも実行される判定・分割・集約は除外していないので、clone後の処理を足すときは`cow_clone.go`へ置き、cloneの有無に依らず成立する契約は他のファイルへ置く。
代表テストは[`cow_darwin_test.go`](../internal/workspace/cow_darwin_test.go)で、macOSであれば`make test-darwin`に限らず`make ci`でも実行される（前提は後述の[部分検証](#部分検証)）。

## 部分検証

darwin専用実装（`cow_darwin.go`・`usage_darwin.go`）のテストは、macOSであれば`make ci`・`make test`・`make test-focus PKG=./internal/workspace`のいずれでも走る。
`cowAvailable`はdarwinで常にtrueを返しfilesystemを見ないため、非APFSの一時ディレクトリではclonefileが効かず、失敗が実装の不具合と区別できない。
そこで`internal/workspace`の`TestMain`が`statfs`でTMPDIRを判定し、APFSでなければテストを走らせずに前提未成立として終える。

これらの実装を触ったら、macOS実機で`make test-darwin`も実行する。
`make build-darwin`はarm64のクロスビルドだけを見るのでテストの実行を保証せず、CIのランナーはすべてlinuxのままとしてmacOS runnerは用意しない。
Linux側の退行は`CGO_ENABLED=0 GOOS=linux .tools/bin/golangci-lint run ./...`で確認する。
