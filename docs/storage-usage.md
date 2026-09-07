# 使用量とCoWの観測

使用量はdaemonが測った値だけを`wx slots`と`wx status`が`measured_at`とともに返し、要求のたびには走査しない。
測る契機は3つある。
lifecycleが`Discovery.ReconcileInterval`ごとにroot内の管理対象と登録外の割当量を測り直し、slotの準備が終わった直後にそのslotだけをbackgroundで測り、`wx clear`のrunが閉じた直後にもroot全体を測り直す。
slot単位の測定はそのslotのsubtreeしか歩かず、貸出の応答に走査時間を持ち込まないようbackgroundで走る。
周期測定は対象一覧を撮った時刻より新しい実測を上書きせず、準備直後の測定結果が次の周期まで消えないようにする。

slotの削除が終わった時点では測り直さず、その slot の実測分をroot合計から引いて`wx status`のDiskへ即座に反映する（`measured_at`はrootの他の部分の鮮度を表すため据え置く）。
`wx clear`は削除に加えて保存でsnapshotを増やすため、runが閉じた後の測り直しで差し引きに現れない増減も合わせる。
この測り直しはrunをDONEにした後のbackgroundで走るので、`wx clear`の応答はwalkを待たない。

コピー方式は準備時の記録ではなく、測定時点の実体から決める。
`allocated_bytes`（`st_blocks*512`）はAPFSのcloneを割り引かず、共有していてもファイル1個分を満額で数えるため、この値だけではCoWと通常コピーを区別できない。
そこでmain worktreeの同じpathを開き、`F_LOG2PHYS_EXT`で得た物理offsetの一致をblock共有の証拠として使う（`internal/workspace/usage.go`）。
1つでも共有しているファイルがあれば`cow`、比較できて1つも無ければ`copy`とする。

比較するのは先頭と末尾の2点だけのsamplingなので、途中のblockだけが書き換わったファイルは共有と見える。
`shared_bytes`は上限側の推定であり、`exclusive_bytes`は下限側の推定である。
判定は前回の`(dev, ino, ctime)`でcacheし、共有を壊す書き込みが必ずctimeを更新することを根拠に、変化していないファイルの再判定を省く。
どちらのファイルも読むだけで、内容もmetadataも変更しない。
判定できない事情（open失敗・size不一致・platform非対応）はすべて共有なしとして扱い、測定の失敗で準備や貸出の結果を変えない。
LinuxではCoW自体を行わないため、`measurement`は`unsupported`になり`shared_bytes`は常に0である。

root合計を更新するのは周期測定・clear後の測り直し・削除分の差し引きだけで、最初の測定が終わるまでは`measurement=pending`として0を実測値に見せない。
表示の組み立ては`internal/cli`、測定は`internal/daemon/usage.go`と`internal/workspace/usage.go`を参照する。

`Disk`はDB登録済みの非ARCHIVED slotとworkspace snapshotの割当量を集計する。
終了済み・隔離済みslotも含み、登録外の実体は`Unmanaged`として別表示する。
診断用の`quarantined_artifacts`だけに記録されたpathは管理対象に含めない。

## 表示する使用量の方針

wxがdisk使用量として表示する値は常にexclusive（共有blockを除いた専有分）とし、`wx slots`のSIZE列と`wx status`のDisk行で同じ量を指す。
どちらもexclusiveなので列の合計とroot合計が同じ意味になり、slotを消したときに実際に空く量の下限を示す。
表示ではexclusiveやallocatedのような内訳の語を出さず、単に使用量として扱う。
利用者に2つの数字を並べて選ばせない方が、どちらが本物かという判断を持ち込まずに済むためである。
Disk行に付く`managed`は管理対象と登録外（`Unmanaged`行）の区別であり、専有分と満額の区別ではない。

`allocated_bytes`は`du`との突き合わせにしか使わないため、`--json`と`wx status --verbose`にだけ残す。
`du -sh`はcloneを割り引かず登録外の実体も数えるので、必ずDisk行より大きく出る。
差の内訳はmain worktreeと共有しているblock（slotを消しても解放されない）と、`Unmanaged`に出る登録外の割当量である。
