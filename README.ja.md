# wx

[English](README.md) | 日本語 | [简体中文](README.zh-CN.md)

Claude Code や Codex を、コマンドひとつで専用の Git worktree に起動します。
`wx` が macOS 上の作業環境を準備・管理するので、元のリポジトリでブランチを切り替えずにエージェントへ作業を任せられます。

## 特長

- **作業環境を分離** — エージェントは detached な Git worktree で作業し、元の作業ツリーの HEAD・index・追跡ファイルを変更しません。
- **すぐに作業を開始** — バックグラウンドのデーモンが、最近使ったリポジトリの作業環境を準備して待機します。
- **セッションを復元** — `wx sessions` と `wx resume` でセッションを確認し、アーカイブした作業を再開できます。
- **いつものコマンドで操作** — Claude Code や Codex の引数をそのまま使え、開始元のブランチも指定できます。

## インストール

**macOS**、**Git**、**Go 1.26.6 以降**、および **Claude Code または Codex** が必要です。
各コマンドをインストールし、`PATH` を通しておいてください。

```sh
git clone https://github.com/HappyOnigiri/WX.git
cd WX
make install
export PATH="$HOME/.local/bin:$PATH"
wx daemon install
```

実行ファイルは `~/.local/bin/wx` にインストールされます。
新しいターミナルでも使えるよう、上記の `export PATH` をシェル設定（例: `~/.zshrc`）に追加してください。
`wx daemon install` は LaunchAgent を登録し、ログイン時にデーモンを起動するようにします。

更新時は clone した WX ディレクトリで `git pull --ff-only` と `make install` を実行してください。
続けて `wx daemon install` と `wx daemon restart` を実行します。

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
- **セッション管理・クリーンアップ:** `wx sessions`、`wx resume`、`wx gc --dry-run`。
- **設定:** `wx config` で設定の確認や個別の値の変更ができます。
- **エージェント連携:** グローバルなエージェント hook で、エージェント側からのセッション再開や作業環境の準備完了チェックに対応します。
  `WX_SESSION_ID` が設定されている場合だけ、`wx hook session-start`、`wx hook user-prompt-submit`、`wx hook pre-tool-use`、`wx hook session-end` を呼び出すよう設定してください。
  hook の設定は wx とは別に管理します。

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
