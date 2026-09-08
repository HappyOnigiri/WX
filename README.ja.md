# wx

[English](README.md) | 日本語 | [简体中文](README.zh-CN.md)

Claude Code や Codex を、コマンドひとつで専用の Git worktree に起動します。
`wx` が macOS 上の作業環境を準備・管理するので、元のリポジトリでブランチを切り替えずにエージェントへ作業を任せられます。

## 特長

- **作業環境を分離** — エージェントは detached な Git worktree で作業し、元の作業ツリーの HEAD・index・追跡ファイルを変更しません。
- **すぐに作業を開始** — バックグラウンドのデーモンが、最近使ったリポジトリの作業環境を準備して待機します。
- **セッションを復元** — `wx slots` でslotとセッションを確認し、`wx resume` でアーカイブした作業を再開できます。
- **いつものコマンドで操作** — Claude Code や Codex の引数をそのまま使え、開始元のブランチも指定できます。

## インストール

**Apple Silicon 搭載の macOS**、**Git**、および **Claude Code または Codex** が必要です。
各コマンドに `PATH` を通しておいてください。
リポジトリの clone や Go のインストールなしで、最新リリースを導入できます。

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

インストーラーは検証済みのバイナリを `~/.local/bin/wx` に配置し、デーモンを LaunchAgent に登録して起動します。
更新も同じコマンドで行い、最新リリースへの置き換え後にデーモンを再起動します。
続けて、現在のターミナルで `wx` を使えるようにします。

```sh
export PATH="$HOME/.local/bin:$PATH"
```

新しいターミナルでも使えるよう、上記の行をシェル設定（例: `~/.zshrc`）に追加してください。
続けてセットアップを完了します。

```sh
wx setup
```

`wx setup` は wx が管理する項目（worktree root、シェルの PATH 設定、LaunchAgent、エージェントの hook、デーモン）を順に確認し、選んだ操作を適用します。
セットアップ済みの環境で再実行しても何も変わりません。
詳細やソースからのビルド方法は[バージョンとリリース](docs/release.md)を参照してください。

## アンインストール

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

アンインストーラーは削除対象の worktree を一覧表示して確認を求め、承諾後に worktree・wx の hook エントリ・LaunchAgent・設定ファイル・`~/.local/bin/wx` を削除します。
確認を省くには `--yes` を渡してください。
削除はデーモン越しに行うため、完了するまでデーモンは起動したままにしてください。

次の 3 つは利用者に委ねられ、それぞれの具体的なコマンドはアンインストーラーが表示します。
シェル設定の `PATH` の行、状態データベースとログディレクトリ、そして worktree root です。
後の 2 つには wx が保存した作業が残っています。
wx を実行したリポジトリには、そのスナップショットに対応する `refs/wx/recovery/*` も残ることがあります。

## 使い方

作業したいリポジトリで実行します。

```sh
wx claude
wx codex
```

選んだエージェントが、wx の管理する worktree で起動します。
`claude` または `codex` より後ろの引数は、そのままエージェントへ渡されます。
開始元のブランチを指定する場合は、エージェント名の前に wx のオプションを置きます。

```sh
wx --branch feature/api codex
```

## その他の機能

- **状態確認・診断:** `wx status`、`wx doctor`。
- **セッション管理・クリーンアップ:** `wx slots`、`wx resume`、`wx gc --dry-run`、`wx clear`。
- **設定:** `wx config` で設定の確認や個別の値の変更ができます。
- **エージェント連携:** グローバルなエージェント hook で作業環境の準備完了を確認し、エージェントのセッションを wx に結び付けて返却します。
  hook の登録は `wx setup` が行います。Claude と Codex の hook 設定のうち wx のエントリだけを wx が所有し、それ以外の内容には触れません。
  Claude の `--resume` と Codex の `resume` は通常の引数のまま使えます。

コマンドやオプションの詳細は、`wx --help` と `wx <command> --help` を参照してください。

```sh
wx --help
wx config --help
wx daemon --help
```

## コントリビュート

コントリビュートを歓迎します！
不具合報告やアイデアは [Issues](https://github.com/HappyOnigiri/WX/issues) へ、改善は [Pull Request](https://github.com/HappyOnigiri/WX/pulls) でお寄せください。
ドキュメントの改善や翻訳も歓迎です。
