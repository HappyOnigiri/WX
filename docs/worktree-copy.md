# worktreeのコピーとリンク

`storage.copy_mode`の値・既定値・fallbackは`wx config --help`を参照する。

対象はmain worktreeの同じpathにある通常ファイルで、cloneしたbytesと宛先の最終bytesが一致するものだけである。
mainとcommitが異なっていても同内容のファイルは共有でき、dirtyなmainの変更は宛先へ持ち込まない。
新規・内容不一致・空ファイル・symlink・submodule・複数hard linkを持つ宛先は通常方式のまま残す。
mainのtree形状が異なる場合や、mainがこの処理中に変化した場合も、そのファイルだけを共有対象外として残りの処理を続ける。
所有者・mode・flags・ACL・xattrが一致しないものも共有対象外とする。
`cow`は共有対象のclone失敗をエラーにする指定であり、全ファイルの共有や削減容量を保証する指定ではない。

`internal/workspace/cow.go`が準備・復元の完了前に処理し、Gitのfilter、checkout hook、prepare commandによる結果を保持する。
indexはstat情報のrefreshだけを行い、staged/unstagedの区別は変えないため、復元した区別も保たれる。
宛先の日時はFD経由で復元し、元ファイルとcloneをatomic swapしてから元inodeを検証して削除する。
所有権不明は`auto`でもfallbackせずQUARANTINEDとして実体を残す。
中断して残った未追跡の`.wx-cow-*`も自動削除せず隔離するため、この名前は予約する。
この検査は無視されたtreeを走査しないので、`.wx-cow-*`をgitignoreで無視すると残骸を検出できなくなる。

Darwinでは`Fclonefileat`を使い、Linuxでは`auto`が通常方式、`cow`がエラーになる。
clone元と宛先は同じ対応volumeにある必要があり、通常checkout1個分の一時容量は必要である。
コピー方式はfingerprintに含めるため、設定変更後の貸出では以前の方式で作ったREADY slotを再利用しない。
`.worktreeinclude`、workspace rootのコピー、生成物、Git objectsや復旧snapshotの容量は、この設定の対象外である。
`auto`が通常コピーへ落ちた回はdaemonのログにwarnとして残る。

## include / link

`.worktreelink`に列挙したpathは、main worktree側の実体へ直接symlinkする（`createLinksAt`）。
sourceが存在しない項目は、ファイル・ディレクトリを問わずその準備では省略し、sourceの出現・消失でslot再利用をfingerprintの存在状態変更により止める。
sourceがsymlinkの項目と、ソースリポジトリのignore対象でない項目も同じく省略し、省略した対象と理由をdaemon logにwarnで残す。
path逸脱・権限エラーや宛先衝突は省略せず、準備を失敗させる。
同じ扱いはworkspace rootのcopy/link source（`MaterializeRootAt`）と`.worktreeinclude`の一致にも適用し、既定名と明示名で挙動を分けない。
workspace内の相対位置を保って再構成する処理は持たず、必要になったら`~/.config/git/hooks/worktreelink-post-checkout`に実装済みのアルゴリズムを移植する。
