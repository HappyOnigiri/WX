# Mutation testing

mutation huntは、行を通過したことだけでは見つからない境界比較の取りこぼしを、手動で調べるための補助線である。

- mutatorを境界系へ絞るのは、既定の否定変異が`err != nil`のような分岐を大量に占め、既存のカバレッジゲートと同じ軸を重ねてしまうためである。
- `make ci`へ接続しないのは、変異ごとにテスト全体を複製して実行するコストと、判定対象を人が読み解く時間を通常のゲートへ持ち込まないためである。
- `internal/state`の中核ロジックへ届かない変異があるのは、遷移の比較がSQL文字列にあり、GoのAST変異対象ではないためである。
- パッケージ単位のmatrixに分けるのは、変異1件ごとの実行時間がパッケージのテスト重量に比例し、重量級だけを独立して観測する必要があるためである。

各manifestはschema 2で、変異のソース上の同一性を表す`id`を持つ。
IDは`wx-mutation-id-v1`、repository-relative path、関数名、mutator、行、列、
元のtoken、変異後のtokenを順に改行で連結し、SHA-256を16進化した値である。
run、attempt、profile、test commitはIDへ含めないため、同じ変異を複数profileが観測しても
reporterは1件へ集約し、profileごとの観測情報を残す。

`mutation-exclusions.txt`は`<repository-relative path><TAB><mutation-id><TAB><reason>`の形式で記録する。
除外は関数ではなくIDへ適用され、同じ関数の別変異は生存変異として残る。
実行profileに適用される行のIDがGremlins結果に存在しない場合はstale exclusionとして
manifest生成を失敗させる。除外IDが現在`KILLED`でも、同じソース変異である限り有効である。

`make mutation-check PKG=./internal/config`はパッケージ全体をGremlinsへ渡し、生成した結果を
mutationreportで判定する。`FILE=internal/config/duration.go`を追加するとそのファイルの
変異だけを合否判定し、`MUTATION_ID=<64桁のID>`を追加すると対象が結果中で一意に
`KILLED`のときだけ成功する。局所指定はGremlins自体の探索範囲を狭めるものではない。
`FILE`と`MUTATION_ID`は同時に指定できない。
