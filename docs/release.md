# バージョンとリリース

## バージョンの真実源

`wx`のバージョンはリリースタグ`vX.Y.Z`が持つ。
Goのリポジトリにバージョンを書いたmanifestを置かず、タグだけを唯一の記録とする。

開発ビルドのバージョンはMakefileの`VERSION`（`git describe`）を`-ldflags`で`internal/version.Version`へ埋め込み、`internal/version.BuildMeta`は`dev`固定とする。
手元の`make build`・`make install`が作るのは常に開発ビルドであり、`wx version v1.2.3-dev`の`-dev`の手前が基にしたリリースを示す。

タグを取得していないshallow checkoutではコミットのabbrevへ退避するため、CIでも`--version`は空にならない。
`make version-check`はこの表示が`VERSION`と一致することを確かめる。
接頭辞だけの確認では`-X`のパスが変わって`undefined`のままでも気付けないため、`make ci`と CI の build ジョブから呼ぶ。

## リリースの流れ

リリースの準備・公開は[HappyOnigiri/ReleaseActions](https://github.com/HappyOnigiri/ReleaseActions)の共通Actionを使う。
WX側のPublish Release workflowは、公開前に配布用の成果物をビルドする。

1. `.github/workflows/release.yml`をGitHubのActions画面から手動実行する。
   `version`にsemverを直接指定するか、`bump`に`auto`・`major`・`minor`・`patch`のいずれかを指定する。
   両方の指定と両方の省略はエラーになる
2. `prepare-release`が`release/vX.Y.Z`ブランチと、changelogを本文に持つ`release: vX.Y.Z`のPRを作る
3. そのPRのCIが通ったらmainへmergeする
4. `.github/workflows/publish-release.yml`がmergeコミットをcheckoutし、リリースブランチ名から取り出したタグで成果物をビルドする
5. `publish-release`が同じコミットにタグを作り、draft Releaseへ全成果物を添付してから公開し、リリースブランチを消す

`.github/workflows/release-reminder.yml`は、main宛のPRに前回リリース以降のmerge済みPR数をコメントする。
リリースPR自身とfork由来のPRは対象外である。

## この repository 固有の前提

- `version-bump-type`は`none`である。
  バージョンを書いたファイルが無いため、`bump`の起点は最新のリリースタグになり、リリースPRのコミットは差分の無い空コミットになる。
- 最初のリリースにはタグもGitHub Releaseも無く、`bump`は起点を持てない。
  `version`へ`0.1.0`のようにsemverを直接指定する。
- `bump: auto`は前回リリース以降のマージ済みPRのタイトルから上げ幅を決め、breaking changeを見つけたらエラーで止まる。
  major は`bump: major`か`version`の明示指定でしか上がらない。
- `release.yml`と`publish-release.yml`はrepository secretの`GH_TOKEN`（`contents:write`と`pull-requests:write`を持つPAT）を使う。
  `release-reminder.yml`だけは`GITHUB_TOKEN`で足りる。
- 成果物のビルド・添付はCIが自動で行い、リリース実行時にファイルパスを入力する必要はない。
- Publish Releaseは同時実行を直列化し、途中の実行をキャンセルしない。
  添付に失敗した場合はdraftのまま残り、同じworkflowの再実行で続行できる。
  公開済みのリリースを再実行しても成果物は上書きしない。

## 配布用ビルド

`make release RELEASE_VERSION=vX.Y.Z`がmacOS arm64バイナリ・チェックサム・インストーラー・アンインストーラーを生成する。
`install.sh`には同じリリースタグを埋め込み、何もダウンロードしない`uninstall.sh`は`scripts/uninstall.sh`をそのまま配る。

配布用ビルドは`Version`に明示したタグ、`BuildMeta`に空文字を埋め込み、`wx --version`は`wx version vX.Y.Z`となる。
この生成時にソースファイルを書き換えたり、バージョン更新のコミットを作ったりすることはない。
`make release-check`は配布用ビルドのOS・CPU・CGO設定とチェックサムを検査し、macOS arm64ではバイナリを実行してバージョン表示も確認する。
`make ci`とCIのbuildジョブから実行する。

## インストールと更新

READMEは`releases/latest/download/install.sh`の固定URLを案内する。
インストーラー内のバージョンはCIが配布物の生成時だけ埋め込み、取得するバイナリとチェックサムを同じタグへ固定する。
次のリリースが途中で公開されても取得物は混在せず、READMEの自動更新も不要になる。
この方式で最初のリリースを公開するまでは、固定URLからインストーラーを取得できない。

インストーラーはmacOS arm64を対象とし、GitとmacOS標準のコマンドだけを使う（GoやGitHub CLIは不要）。
チェックサムと`--version`の一致を確認してから、`~/.local/bin/wx`を同じディレクトリ内の一時ファイルからrenameで置き換える。
取得・検証の失敗では既存バイナリを変更しない。

初回や別パスからの移行、LaunchAgentが未登録の場合は`daemon stop`の完了を確認し、バイナリ配置後に`daemon install`と`daemon start`を実行する。
登録済みで同じ配置先の場合は`daemon restart`を使い、処理中のジョブやRPCは既存のidleゲートで待機する。
停止が失敗した場合はバイナリを置き換えず、登録・起動・再起動が失敗した場合は配置済みのバイナリを残して復旧コマンドを表示する。
シェル設定とエージェントのhook設定は変更しない。

## アンインストール

READMEは`releases/latest/download/uninstall.sh`の固定URLを案内する。
アンインストーラーは削除の順序だけを持ち、削除そのものは`wx clear --all --discard`・`wx daemon stop`・`wx setup --remove`へ委ねる。
`wx clear`はdaemon越しに動き、`wx setup --remove`はLaunchAgentの解除でdaemonをbootoutするため、この順序は入れ替えられない。
逆にするとworktreeとソースリポジトリのgit worktree登録が残る。

`wx setup --remove`が消すのは、hookエントリ・LaunchAgent・`config.yaml`だけである。
shell起動ファイルは対象外とし、状態DB・ログ・worktree rootは`leftover`で始まる行として報告するだけにする。
後者は保存済みの作業と記録を含むので、削除の判断を利用者に残す。
アンインストーラーはこの`leftover`行を読んで`rm -rf`の候補として表示する。
snapshotに対応する`refs/wx/recovery/*`は、現行DBが説明する限り`wx prune`の対象にならないため、リポジトリ側で消すコマンドを案内するだけにする。

## ソースからの開発ビルド

開発用のcheckoutでは`make install`（既定の配置先は`~/.local/bin/wx`、`INSTALL_DIR`で変更）を使う。
初回のdaemon登録は`wx daemon install`、バイナリ更新後は`wx daemon restart`が必要である。
