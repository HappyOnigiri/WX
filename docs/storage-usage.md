# 使用量とCoWの観測

## 測る契機

使用量はdaemonが測った値だけを`wx slots`と`wx status`が`measured_at`とともに返し、要求のたびには走査しない。
走査量はroot配下のファイル数に比例するため、貸出・`wx clear`・status表示のどの要求経路にも持ち込まず、測定はすべてbackgroundで走らせる。

測る契機は`Discovery.ReconcileInterval`ごとの周期測定と、使用量が変わった直後の測り直し要求である。
要求はslotの準備・snapshotの保存・`wx clear`のrunが閉じた時点で出し、周期測定を待たずにroot合計を追随させる。
要求は1本に畳み、走っている間に届いた分は次の1巡へまとめる。walkを要求の数だけ重ねないためである。
slotの準備が終わった直後はそのslotだけを先に測る。
このため周期測定は、対象一覧を撮った時刻より新しい実測を上書きしない。上書きすると準備直後に測った値が次の周期まで消える。

slotの削除が終わった時点では測り直さず、そのslotの実測分をroot合計から引く。
実体はもう無いので走査する意味がなく、`measured_at`はrootの他の部分の鮮度を表すため据え置く。
`wx clear`は削除に加えて保存でsnapshotを増やすので、差し引きだけでは足りない増減をrunが閉じた後の測り直しで合わせる。

書き込みの途中にあるslotは、走査しても配置の途中経過しか見えないため、その値をそのslotの使用量として公開しない。
ただし実体はroot合計に数えたままにして、準備中の割当量が`Unmanaged`（登録外の実体）へ移らないようにする。
この結果、再び準備へ入ったslotは前の準備で測った値を次の周期で落とし、`measurement`は`pending`へ戻る。

最初の測定が終わるまでは`measurement=pending`とし、0を実測値に見せない。pendingを長く出さないよう、最初の測定はdaemonの起動直後に1度走らせる。
走査中に消えたentryはその1件だけを飛ばす。GCやclearと並走した回にroot全体の集計を捨てると、部分集計を実測として出すか測れないままになるかのどちらかになる。

## CoW共有の判定

コピー方式は準備時の記録ではなく、測定時点の実体から決める。貸出後の書き換えで共有が解けた分も現れるようにするためである。
割当量（`allocated_bytes`）はAPFSのcloneを割り引かず、blockを共有していてもファイル1個分を満額で数えるため、この値だけではCoWと通常コピーを区別できない。
そこでmain worktreeの同じpathを開き、`F_LOG2PHYS_EXT`で得た物理offsetの一致をblock共有の証拠として使う。

比較するのは先頭と末尾の2点だけのsamplingなので、途中のblockだけが書き換わったファイルは共有と見える。
したがって`shared_bytes`は上限側、`exclusive_bytes`は下限側の推定である。
判定できない事情はすべて共有なしとして扱い、測定の失敗で準備や貸出の結果を変えない。
LinuxではCoW自体を行わないため`measurement`は`unsupported`になり`shared_bytes`は常に0で、この0は共有が無いことの証明ではない。

判定はslot側とmain worktree側それぞれの`(dev, ino, ctime)`でcacheする。
共有を壊す書き込みは必ずctimeを更新するので、両側が変化していないファイルは再判定を省いてfstatatだけで済ませられる。
判定はどちらのファイルも読むだけで、内容もmetadataも変更しない。

走査はdirectoryのdescriptor相対に1成分ずつ降り、path名をrootから辿り直さない。
`os.Root`のpath指定は成分ごとにopenatを重ねるため、深さに比例した費用がファイル数だけ積み上がる。

走査はslot境界に加えてrepository境界も知っているため、slot内のrepository directoryごとの内訳を同じ1巡で集計する。
repositoryの外に置かれたslot直下のファイルはどの内訳にも入らないので、内訳の合計はslot合計と一致しない。
内訳は`--json`と`wx doctor --probe`にだけ出し、`wx slots`の表には出さない。

## 表示する使用量の方針

`wx status`の`Disk`はDB登録済みの非ARCHIVED slotとworkspace snapshotを集計対象とする。
終了済み・隔離済みslotも含み、登録外の実体は`Unmanaged`として別に出す。
診断用の`quarantined_artifacts`だけに記録されたpathは管理対象に含めない。

表示する値は常にexclusive（共有blockを除いた専有分）とし、`wx slots`のSIZE列とDisk行で同じ量を指す。
そろえることでSIZE列の合計とroot合計が同じ意味になり、どちらもslotを消したときに実際に空く量の下限を示す。
表示ではexclusiveやallocatedのような内訳の語を出さず、単に使用量として扱う。
利用者に2つの数字を並べて選ばせない方が、どちらが本物かという判断を持ち込まずに済むためである。
Disk行に付く`managed`は管理対象と登録外の区別であり、専有分と満額の区別ではない。

`allocated_bytes`は`du`との突き合わせにしか使わないため、`--json`と`wx status --verbose`にだけ残す。
`du -sh`はcloneを割り引かず登録外の実体も数えるので、必ずDisk行より大きく出る。
差の内訳はmain worktreeと共有しているblock（slotを消しても解放されない）と、`Unmanaged`に出る登録外の割当量である。
