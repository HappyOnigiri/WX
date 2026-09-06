# 使用量とCoWの観測

使用量はdaemonが測った値だけを`wx slots`と`wx status`が`measured_at`とともに返し、要求のたびには走査しない。
測る契機は2つで、lifecycleが`Discovery.ReconcileInterval`ごとにroot全体を測り直すのに加え、slotの準備が終わった直後にそのslotだけをbackgroundで測る。
slot単位の測定はそのslotのsubtreeしか歩かず、貸出の応答に走査時間を持ち込まないようbackgroundで走る。
周期測定は対象一覧を撮った時刻より新しい実測を上書きせず、準備直後の測定結果が次の周期まで消えないようにする。

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

root合計は周期測定だけが更新し、最初の測定が終わるまでは`measurement=pending`として0を実測値に見せない。
表示の組み立ては`internal/cli`、測定は`internal/daemon/usage.go`と`internal/workspace/usage.go`を参照する。
