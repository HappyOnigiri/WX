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
シェル設定とエージェントのhook設定は変更しない。

開発用checkoutの`make install`はバイナリを置くだけなので、daemonの登録と更新反映は`wx daemon install`・`wx daemon restart`を自分で実行する。

## アンインストール

アンインストーラーは削除の順序だけを持ち、削除そのものは`wx clear --all --discard`・`wx daemon stop`・`wx setup --remove`へ委ねる。
`wx clear`はdaemon越しに動き、`wx setup --remove`はLaunchAgentの解除でdaemonをbootoutするため、この順序は入れ替えられない。
逆にするとworktreeとソースリポジトリのgit worktree登録が残る。

`wx setup --remove`が消すのは、hookエントリ・LaunchAgent・`config.yaml`だけである。
shell起動ファイルは対象外とし、状態DB・ログ・worktree rootは保存済みの作業と記録を含むので削除の判断を利用者に残し、`leftover`で始まる行として報告するだけにする。
アンインストーラーはこの`leftover`行を読んで`rm -rf`の候補として表示する。
`config.yaml`を消した後はworktree rootを引けないため、この受け渡しは`setup --remove`の出力経由でしか成立しない。
snapshotに対応する`refs/wx/recovery/*`は、現行DBが説明する限り`wx prune`の対象にならないため、リポジトリ側で消すコマンドを案内するだけにする。
