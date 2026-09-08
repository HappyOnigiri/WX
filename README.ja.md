# wx

[English](README.md) | 日本語 | [简体中文](README.zh-CN.md)

Claude Code や Codex を、コマンドひとつで専用の Git worktree に起動します。
`wx` が macOS 上の作業環境を準備・管理するので、元のリポジトリでブランチを切り替えずにエージェントへ作業を任せられます。

## 特長

- **作業環境を分離** — エージェントは detached な Git worktree で作業し、元の作業ツリーの HEAD・index・追跡ファイルを変更しません。
- **すぐに作業を開始** — バックグラウンドのデーモンが、最近使ったリポジトリの作業環境を準備して待機します。
- **ディスク容量を節約** — APFS の Copy on Write を活用し、元の作業ツリーと同じ内容のファイルのデータを共有して、worktree のディスク使用量を抑えます。
- **いつものコマンドで操作** — Claude Code や Codex の引数をそのまま使え、開始元のブランチも指定できます。

## インストール

**Apple Silicon 搭載の macOS**、**Git**、および **Claude Code または Codex** が必要です。

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

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

Claude Code と Codex は起動パスごとにセッションログを管理するため、worktree が変わると標準の continue / resume では過去のセッションを見つけにくくなります。
wx はこれらをラップし、過去のセッションを選んで再開できる UI を提供します。

```sh
wx claude --resume
wx codex resume
```

## その他の機能

- **状態確認・診断:** `wx status`、`wx doctor`。
- **セッション管理・クリーンアップ:** `wx slots`、`wx resume`、`wx gc --dry-run`、`wx clear`。
- **設定:** `wx config` で設定の確認や個別の値の変更ができます。

コマンドやオプションの詳細は、`wx --help` と `wx <command> --help` を参照してください。

## アンインストール

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

wx が管理する worktree（未保存の作業を含む）・hook・LaunchAgent・設定ファイル・実行ファイルを削除します。

## コントリビュート

コントリビュートを歓迎します！
不具合報告やアイデアは [Issues](https://github.com/HappyOnigiri/WX/issues) へ、改善は [Pull Request](https://github.com/HappyOnigiri/WX/pulls) でお寄せください。
ドキュメントの改善や翻訳も歓迎です。
