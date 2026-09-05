# wx

[English](README.md) | [日本語](README.ja.md) | 简体中文

只需一条命令，即可在独立的 Git worktree 中启动 Claude Code 或 Codex。
`wx` 负责在 macOS 上准备和管理工作环境，让你无需切换原仓库的分支，就能把任务交给智能体。

## 特性

- **独立的工作环境** — 智能体在 detached HEAD 状态的 Git worktree 中工作，不会更改原工作区的 HEAD、暂存区和已跟踪文件。
- **随时开始工作** — 后台守护进程会为最近使用的仓库预先准备工作环境。
- **恢复会话** — 使用 `wx leases` 查看会话，使用 `wx resume` 恢复已归档的工作。
- **熟悉的命令** — 直接使用 Claude Code 或 Codex 的常用参数，也可以指定起始分支。

## 安装

需要 **macOS**、**Git**、**Go 1.27.1 或更高版本**，以及 **Claude Code 或 Codex**。
请先安装这些工具，并确保可以通过 `PATH` 访问它们。

```sh
git clone https://github.com/HappyOnigiri/WX.git
cd WX
make install
export PATH="$HOME/.local/bin:$PATH"
wx daemon install
```

可执行文件会安装到 `~/.local/bin/wx`。
请将上面的 `export PATH` 添加到 shell 配置文件（例如 `~/.zshrc`），以便在新终端中使用。
`wx daemon install` 会注册 LaunchAgent，让守护进程在登录时启动。

更新时，在克隆的 WX 目录中运行 `git pull --ff-only` 和 `make install`，然后运行 `wx daemon install` 和 `wx daemon restart`。

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
- **会话管理与清理：** `wx leases`、`wx resume`、`wx gc --dry-run`、`wx clear`。
- **配置：** 使用 `wx config` 查看配置或修改单个配置值。
- **智能体集成：** 全局智能体 hook 会检查工作环境是否就绪，将智能体会话绑定到 wx，并在工作结束后归还。
  请配置为仅在设置了 `WX_SESSION_ID` 时调用 `wx hook session-start`、`wx hook user-prompt-submit`、`wx hook pre-tool-use` 和 `wx hook session-end`。
  Claude 的 `--resume` 和 Codex 的 `resume` 可按原本方式使用。
  hook 配置由用户单独管理，不包含在 wx 中。

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
