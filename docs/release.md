# バージョンとリリース

## バージョンの真実源

`wx`のバージョンはリリースタグ`vX.Y.Z`だけが持つ。
Goのリポジトリにバージョンを書いたmanifestを置かないため、リリースPRはバージョンを刻む差分の無い空コミットになる（共通Actionの`version-bump-type`を`none`にしている）。

開発ビルドは`git describe`の結果を`-ldflags`で`internal/version`へ埋め込み、`BuildMeta`を`dev`にする。
手元の`make build`・`make install`が作るのは常に開発ビルドであり、`-dev`の手前が基にしたリリースを示す。
タグを取得していないshallow checkoutではコミットのabbrevへ退避するため、CIでも`--version`は空にならない。

`make version-check`はこの表示が`VERSION`と完全一致することを確かめる。
接頭辞だけの確認では`-X`のパスが変わって`undefined`のままでも気付けないため、`make ci`とCIのbuildジョブから呼ぶ。

## リリースの流れ

リリースの準備・公開は[HappyOnigiri/ReleaseActions](https://github.com/HappyOnigiri/ReleaseActions)の共通Actionへ委ね、WX側のworkflowは入力の受け口と成果物のビルドだけを持つ。
例外は前回リリース以降のmerge済みPR数をmain宛PRへ知らせる`release-reminder.yml`である。

`release.yml`を手動実行すると、リリースブランチとchangelogを本文に持つリリースPRができる。
そのPRをmainへmergeすると、`publish-release.yml`がmergeコミットをcheckoutしてビルドし、同じコミットにタグを作って全成果物を添付したReleaseを公開する。
ビルドに使うタグはリリースブランチ名から取り出すため、成果物のバージョンはPRのブランチ名で決まる。

- 最初のリリースにはタグもGitHub Releaseも無く`bump`が起点を持てないため、`version`へsemverを直接指定する。
- `bump: auto`は前回リリース以降のマージ済みPRのタイトルから上げ幅を決め、breaking changeを見つけたらエラーで止まる。
  majorは`bump: major`か`version`の明示指定でしか上がらない。
- `release.yml`と`publish-release.yml`は`contents:write`と`pull-requests:write`を持つPAT（repository secretの`GH_TOKEN`）を使う。
  `release-reminder.yml`だけは`GITHUB_TOKEN`で足りる。
- Publish Releaseは同時実行を直列化し、途中の実行をキャンセルしない。
  添付に失敗した場合はdraftのまま残り、同じworkflowの再実行で続行できる。
  公開済みのリリースを再実行しても成果物は上書きしない。

## 配布用ビルド

`make release`はタグを`RELEASE_VERSION`で必ず受け取り（開発ビルドの`VERSION`推定は使わない）、`RELEASE_DIR`へmacOS arm64バイナリ・チェックサム・インストーラー・アンインストーラーを生成する。
`publish-release.yml`はこの4ファイルのpathを固定で参照するため、`RELEASE_DIR`とファイル名を変えると添付が静かに壊れる。
`install.sh`には同じリリースタグを埋め込み、何もダウンロードしない`uninstall.sh`は`scripts/uninstall.sh`をそのまま配る。

配布用ビルドはタグを明示し`BuildMeta`を空にするので、`wx --version`に`-dev`が付かない。
この生成でソースファイルを書き換えたり、バージョン更新のコミットを作ったりすることはない。
`make release-check`は配布物のOS・CPU・CGO設定とチェックサム、macOS arm64ではバージョン表示まで検査する。
`make ci`とCIのbuildジョブの両方から呼ぶのは、CIのランナーが全てlinuxでバージョン表示の確認が手元でしか走らないためである。

## インストールと更新

READMEは`releases/latest/download/`の固定URLを案内し、インストーラー側にはCIが生成時のタグを埋め込む。
取得するバイナリとチェックサムを同じタグへ固定できるため、次のリリースが途中で公開されても取得物は混在せず、READMEの自動更新も要らない。
代償として、この方式で最初のリリースを公開するまでは固定URLからインストーラーを取得できない。

インストーラーはmacOS arm64を対象とし、GitとmacOS標準のコマンドだけを使う（GoやGitHub CLIは不要）。
取得・検証の失敗では既存バイナリを変更しない。
LaunchAgentの登録が生きていないか別パスを指している場合は、`daemon install`のbootoutより先に`daemon stop`の完了を確認し、停止が失敗したらバイナリを置き換えない。
登録・起動・再起動が失敗した場合は配置済みのバイナリを残して復旧コマンドを表示する。
LaunchAgentに登録されるのは`launchd.ResolveBinary`がPATHから解決した`wx`なので、インストーラーは配置先をPATHの先頭に置いて`daemon install`を呼び、別のインストール先が登録されるのを防ぐ。
初回インストールで端末があるときだけ`wx setup`を対話で通し、利用者が「おすすめ設定」を選べばシェル起動ファイルへのPATH追記とエージェントのhook登録まで適用する。
更新と非対話のインストールでは`wx setup --update`だけを呼び、シェル設定とhook設定は変更しない。

LaunchAgentはそのバイナリを直接ではなく、利用者のログインシェルから`exec`して起動する。
daemonが実行するpost-checkout hookなどは対話シェルと同じツールチェーンを要求するのに対し、plistに書ける固定のPATHでは版管理ツール（fnmなど）が起動ファイルで組み立てるPATHを再現できないためである。
`exec`で置き換えるのでppidと`XPC_SERVICE_NAME`は変わらず、restartの`underLaunchd`判定と`KeepAlive`の条件は従来のままになる。
起動ファイルを読ませたくない場合は`daemon.login_shell`を無効にすると、固定PATHだけを書いた従来のplistへ戻る。

plistは`launchd.Render`の出力とbyte単位で比較するため、起動に使うシェルはplistの`EnvironmentVariables`へも書き戻す。
daemon自身も比較側になるので、書き戻さないとdaemonの環境に`SHELL`が無く、生成し直したplistが常にstaleに見える。
同じ理由で実効値は呼び出し側から持ち回らせず、`launchd.LoginShellEnabled`が設定ファイルだけを読む。

表示言語を含め、利用者への質問はインストーラーではなく`wx setup`が持つ。
選択肢の見え方を1箇所へ揃えるためで、インストーラーは初回に言語を書き込まない。
書き込むと設定済みと判定され、setupの言語の質問が出なくなる。
その代わり初回インストールではsetupが始まるまでの出力が英語になり、以降の案内はsetupが保存した値を読み戻して出す。
更新では既存のbinaryか`config.yaml`から読んだ言語を最初から使う。

開発用checkoutの`make install`は、既定の配置先（`$HOME/.local/bin`）へ入れたときだけ、LaunchAgentの登録・daemonの入れ替え・agent hookの追従まで済ませる。
判定とループは`scripts/install-local.sh`が持ち、状態ごとのinstall・update・startの選び分けは`wx setup --item <id> --action recommended`へ委ねて、同じ判定をshellへ書き写さない。
`INSTALL_DIR`を変えた呼び出し（`make smoke`など）とmacOS以外では何もせず、バイナリを置くだけで終える。
hookの陳腐化判定は`os.SameFile`なので、同じpathへの置き換えでは`make install`のたびに書き換わるわけではない。
値が変わるのはhookのイベント集合が増えた版と、hookが未登録の環境だけである。

## 更新の確認と適用

新しいタグが出たことは、daemonが保守の一巡に相乗りして日和見的に確認する。
CLIとTUIはdaemonが持つ結果を読むだけなので、対話起動がネットワーク待ちで遅くならない。
確認の失敗は記録して次回に回し、起動も画面も失敗させない。

`wx update --apply`は更新をGoで再実装せず、その時点の最新タグに添付された`install.sh`を取り直して実行する。
checksum検証・版番号の照合・atomicな置換・`daemon restart`・`wx setup --update`はすべて`install.sh`が持っており、初回導入と更新で導入経路を1つに保つためである。
daemonが持つキャッシュのタグではなく実行時に確認し直すのは、キャッシュが古いときに「現在より新しいが最新ではない」版を入れないためである。

開発ビルドでは確認も更新も行わない。
埋め込み版が`vX.Y.Z`ではなくリリースタグと比較できず、`install.sh`が固定している配置先が開発用の配置と一致するとも限らないためである。
`BuildMeta`の既定値が`dev`であることにより、テストバイナリも必ずこちら側に落ちる。

対話起動での案内はその版について1回だけ出す。
案内済みの版をstate.dbに記録し、条件付きUPDATEが1行を変えられたプロセスだけが案内する。
「このバージョンをスキップ」は作らない。
案内が版ごと1回に限られている以上、見送りたい利用者は何もしなければよく、後から確認したくなれば`wx update`を実行できるためである。

## アンインストール

アンインストーラーは削除の順序だけを持ち、削除そのものは`wx clear --all --discard`・`wx daemon stop`・`wx setup --remove`へ委ねる。
`wx clear`はdaemon越しに動き、`wx setup --remove`はLaunchAgentの解除でdaemonをbootoutするため、この順序は入れ替えられない。
逆にするとworktreeとソースリポジトリのgit worktree登録が残る。

`wx setup --remove`が消すのは、hookエントリ・LaunchAgent・`config.yaml`だけである。
shell起動ファイルは対象外とし、状態DB・ログ・worktree rootは保存済みの作業と記録を含むので削除の判断を利用者に残し、`leftover`で始まる行として報告するだけにする。
アンインストーラーはこの`leftover`行を読んで`rm -rf`の候補として表示する。
`config.yaml`を消した後はworktree rootを引けないため、この受け渡しは`setup --remove`の出力経由でしか成立しない。
snapshotに対応する`refs/wx/recovery/*`は、現行DBが説明する限り`wx prune`の対象にならないため、リポジトリ側で消すコマンドを案内するだけにする。
