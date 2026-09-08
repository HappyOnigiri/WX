# wx

[English](README.md) | [日本語](README.ja.md) | 简体中文

只需一条命令，即可在独立的 Git worktree 中启动 Claude Code 或 Codex。
`wx` 负责在 macOS 上准备和管理工作环境，让你无需切换原仓库的分支，就能把任务交给智能体。

## 特性

- **独立的工作环境** — 智能体在 detached HEAD 状态的 Git worktree 中工作，不会更改原工作区的 HEAD、暂存区和已跟踪文件。
- **随时开始工作** — 后台守护进程会为最近使用的仓库预先准备工作环境。
- **恢复会话** — 使用 `wx slots` 查看槽位与会话，使用 `wx resume` 恢复已归档的工作。
- **熟悉的命令** — 直接使用 Claude Code 或 Codex 的常用参数，也可以指定起始分支。

## 安装

需要 **搭载 Apple Silicon 的 macOS**、**Git**，以及 **Claude Code 或 Codex**。
请确保这些命令可通过 `PATH` 访问。
无需克隆仓库或安装 Go，即可安装最新发布版本：

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

安装脚本会验证可执行文件，将其安装到 `~/.local/bin/wx`，并注册和启动 LaunchAgent 守护进程。
更新时运行同一条命令，即可安装最新发布版本并重启守护进程。
然后在当前终端中设置 `PATH`：

```sh
export PATH="$HOME/.local/bin:$PATH"
```

请将上面的行添加到 shell 配置文件（例如 `~/.zshrc`），以便在新终端中使用。
然后完成初始化设置：

```sh
wx setup
```

`wx setup` 会依次确认 wx 管理的各项（worktree root、shell 的 PATH 设置、LaunchAgent、智能体 hook、守护进程），并应用你选择的操作。
在已完成设置的环境中再次运行不会有任何改动。
更多信息及源码构建方法，请参阅[版本与发布说明](docs/release.md)。

## 卸载

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

卸载程序会先列出将要删除的 worktree 并请求确认，同意后删除这些 worktree、wx 的 hook 条目、LaunchAgent、配置文件以及 `~/.local/bin/wx`。
传入 `--yes` 可跳过确认。
删除通过守护进程执行，因此在卸载完成前请保持守护进程运行。

以下三项交由你自行处理，卸载程序会打印每一项对应的命令：
shell 配置中的 `PATH` 行、状态数据库与日志目录，以及 worktree root。
后两项保存着 wx 为你保留的工作内容。
你用过 wx 的仓库中还可能残留对应这些快照的 `refs/wx/recovery/*`。

## 快速开始

在你要处理的仓库中运行：

```sh
wx claude
wx codex
```

所选智能体会在 wx 管理的 worktree 中启动。
`claude` 或 `codex` 后面的参数会原样传递给智能体。
要指定起始分支，请将 wx 选项放在智能体名称之前：

```sh
wx --branch feature/api codex
```

## 更多功能

- **状态与诊断：** `wx status`、`wx doctor`。
- **会话管理与清理：** `wx slots`、`wx resume`、`wx gc --dry-run`、`wx clear`。
- **配置：** 使用 `wx config` 查看配置或修改单个配置值。
- **智能体集成：** 全局智能体 hook 会检查工作环境是否就绪，将智能体会话绑定到 wx，并在工作结束后归还。
  hook 的注册由 `wx setup` 完成。Claude 与 Codex 的 hook 配置中只有 wx 自己的条目由 wx 拥有，文件中的其他内容不会被改动。
  Claude 的 `--resume` 和 Codex 的 `resume` 可按原本方式使用。

命令和选项的详细用法，请参阅 `wx --help` 和 `wx <command> --help`：

```sh
wx --help
wx config --help
wx daemon --help
```

## 参与贡献

欢迎参与贡献！
你可以通过 [Issues](https://github.com/HappyOnigiri/WX/issues) 报告问题或提出想法，也可以提交 [Pull Request](https://github.com/HappyOnigiri/WX/pulls)。
同样欢迎改进文档和翻译。
