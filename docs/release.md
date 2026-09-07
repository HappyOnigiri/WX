# バージョンとリリース

## バージョンの真実源

`wx`のバージョンはリリースタグ`vX.Y.Z`が持つ。
Goのリポジトリにバージョンを書いたmanifestを置かず、タグだけを唯一の記録とする。

ビルド時のバージョンはMakefileの`VERSION`が`git describe --tags --match 'v[0-9]*' --always --dirty`で求め、`-ldflags`で`internal/version.Version`へ埋め込む。
`internal/version.BuildMeta`は`dev`固定で、`wx --version`は`wx version v1.2.3-dev`のように表示する。
手元の`make build`・`make install`が作るのは常に開発ビルドであり、リリース成果物そのものだとは名乗らない。
`-dev`の手前の部分が、そのビルドの基にしたリリースを示す。

タグを取得していないshallow checkoutではコミットのabbrevへ退避するため、CIでも`--version`は空にならない。
`make version-check`はこの表示が`VERSION`と一致することを確かめる。
接頭辞だけの確認では`-X`のパスが変わって`undefined`のままでも気付けないため、`make ci`と CI の build ジョブから呼ぶ。

## リリースの流れ

リリースは[HappyOnigiri/ReleaseActions](https://github.com/HappyOnigiri/ReleaseActions)の共通Actionに委ね、この repository では 3 つのworkflowが呼び出しだけを持つ。

1. `.github/workflows/release.yml`をGitHubのActions画面から手動実行する。
   `version`にsemverを直接指定するか、`bump`に`auto`・`major`・`minor`・`patch`のいずれかを指定する。
   両方の指定と両方の省略はエラーになる
2. `prepare-release`が`release/vX.Y.Z`ブランチと、changelogを本文に持つ`release: vX.Y.Z`のPRを作る
3. そのPRのCIが通ったらmainへmergeする
4. `.github/workflows/publish-release.yml`が`publish-release`を呼び、タグ`vX.Y.Z`とGitHub Releaseを作ってリリースブランチを消す

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
- リリース成果物はタグとGitHub Releaseだけで、バイナリは添付しない。
  導入はソースからの`make install`である。
