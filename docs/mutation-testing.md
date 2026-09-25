# Mutation testing

mutation huntは、行を通過したことだけでは見つからない境界比較の取りこぼしを、手動で調べるための補助線である。

- mutatorを境界系へ絞るのは、既定の否定変異が`err != nil`のような分岐を大量に占め、既存のカバレッジゲートと同じ軸を重ねてしまうためである。
- `make ci`へ接続しないのは、変異ごとにテスト全体を複製して実行するコストと、判定対象を人が読み解く時間を通常のゲートへ持ち込まないためである。
- `internal/state`の中核ロジックへ届かない変異があるのは、遷移の比較がSQL文字列にあり、GoのAST変異対象ではないためである。
- 重量級パッケージをファイル単位のshardへ分けるのは、変異1件ごとの実行時間がパッケージのテスト重量に比例し、単一パッケージの処理を並列化するためである。

各manifestはschema 3で、変異のソース上の同一性を表す`id`を持ち、計測できた場合はGremlins実行の実測秒も記録する。
IDは`wx-mutation-id-v1`、repository-relative path、関数名（関数外は予約名`<package>`）、mutator、行、列、
元のtoken、変異後のtokenを順に改行で連結し、SHA-256を16進化した値である。
run、attempt、profile、test commitはIDへ含めないため、同じ変異を複数profileが観測しても
reporterは1件へ集約し、profileごとの観測情報を残す。

`mutation-exclusions.txt`は`<repository-relative path><TAB><ast-v1:SHA-256><TAB><reason>`の形式で記録する。
除外keyはレシーバ付き関数名、宣言の正規化token列、対象tokenの宣言内位置、mutator、変異前後の
tokenから作る。空白・コメント・宣言外の編集では変わらず、宣言内部の処理が変われば失効する。
測定前に全keyをソース上で一意に解決し、測定後は選択対象に対応する観測用mutation IDだけを
Gremlins結果と照合する。同じ関数の別演算子への自動付け替えは行わない。

`make mutation-check PKG=./internal/config`はパッケージ全体をGremlinsへ渡し、生成した結果を
mutationreportで判定する。`FILE=internal/config/duration.go`を追加するとそのファイルの
変異だけを合否判定し、`MUTATION_ID=<64桁のID>`を追加すると対象が結果中で一意に
`KILLED`のときだけ成功する。局所指定はGremlins自体の探索範囲を狭めるものではない。
`FILE`と`MUTATION_ID`は同時に指定できない。

## workflowの観測契約

planは、実行するshardのIDとそのshardが生成するmanifestの`profile`一覧を機械可読な
出力としてreportへ渡す。reportはartifact名のshard ID・run ID・attemptとmanifestの
`profile`を照合し、重複・予期しないshard・不正なmanifestを副作用のない検証で拒否する。
この検証が終わるまで、mutation labelを含むGitHub Issues APIの書き込みは行わない。
測定期限超過やrunner停止で欠けたshardは整合性の問題と分け、検証を通ったshardの生存変異を
起票してから欠落を全件summaryへ出してreportを失敗させる。起票はcreate・comment・reopenだけで
issueを閉じないため、欠けたshardの既存issueには影響しない。
Gremlinsがexit 0で結果ファイルを作らない場合は、測定済みの
mutation 0件として`mutationreport`の空結果モードでschema 2のmanifestを生成する。
空manifestも通常と同じprofile、実行情報、command、exclusion、shard、repository境界の
検証を通し、全shardが有効なmanifestを生成して初めて正常な空結果になる。非0終了、0バイト
結果、不正JSON、artifactやmanifestの欠落は空結果へ読み替えず失敗として扱う。

workflow_dispatchでは起動時にissue起票を抑止できる。抑止中もshardの整合検証と集計は同じ経路で行われ、起票候補はstep summaryへ記録される。

`internal/fdexec`はMutation Huntの対象外である。`unix.Close`、`unix.Exec`、`os.Exit`を
含むprocess/OS adapterがhosted runnerの通信断や終了を起こし得るため、通常の列挙でも
明示指定でもGremlinsを起動せず、planの診断へ除外理由を残す。

`internal/archive`はsourceの追加・削除・重複割り当てをplan時に検証しつつ、ファイル分割せず
パッケージ単位で実行する。mutation IDとmanifestの`profile`はshard分割前と同じ契約を保つ。

daemonはplan時にproduction Go sourceを列挙し、1ファイルを1つのjobへ割り当てる。
cliとworkspaceはGremlinsのdry-run結果からRUNNABLE変異数を数え、従来どおりファイル単位の
LPTで担当範囲を決める。担当外のsourceは`--exclude-files`で除外し、manifest生成時にも担当
ファイル集合を検証するため、除外漏れやshard間の結果混入を検出できる。

lightweight packageのgroup配分は、manifestの実測秒をfull runのartifactから
`make mutation-weights`で集計した重みを使う。重みファイルは人が確認してコミットし、
未計測のprofileはplanで中央値へ退避してnoticeに記録する。partial dispatchのartifactから
重みを更新しない。

jobの上限は変異ごとの`timeout-coefficient`と分離し、測定期限には専用process groupをTERM、
猶予後にKILLする。実行結果JSONは正常完了、除外不整合、Gremlins失敗、結果不正、測定期限超過を
区別する。変異で暴走したtestがrunnerのメモリを使い切るとjobごと停止して結果が残らないため、
Gremlins配下のprocess全体をメモリ上限付きのcgroupで起動し、上限超過をテスト失敗として扱わせる。
processごとのrlimitでは並列に動くtest processの合計を抑えられず、swapへ逃がすとrunnerが応答を失うため、
cgroupの上限はswapを含めて掛ける。
stdout/stderr、dry-run、割り当て、PID・PPID・PGIDを含むheartbeatは診断artifactへ
分離する。
