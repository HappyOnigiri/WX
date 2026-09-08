# wx

[English](README.md) | [日本語](README.ja.md) | 简体中文

只需一条命令，即可在独立的 Git worktree 中启动 Claude Code 或 Codex。
`wx` 负责在 macOS 上准备和管理工作环境，让你无需切换原仓库的分支，就能把任务交给智能体。

## 特性

- **独立的工作环境** — 智能体在 detached HEAD 状态的 Git worktree 中工作，不会更改原工作区的 HEAD、暂存区和已跟踪文件。
- **随时开始工作** — 后台守护进程会为最近使用的仓库预先准备工作环境。
- **节省磁盘空间** — 利用 APFS 的 Copy on Write，与原工作区中内容相同的文件共享数据，减少 worktree 的磁盘占用。
- **熟悉的命令** — 直接使用 Claude Code 或 Codex 的常用参数，也可以指定起始分支。

## 安装

需要 **搭载 Apple Silicon 的 macOS**、**Git**，以及 **Claude Code 或 Codex**。

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/install.sh | bash
```

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

Claude Code 和 Codex 按启动路径管理会话日志，因此 worktree 发生变化后，标准的 continue / resume 命令可能难以找到过去的会话。
wx 对这些命令进行封装，提供用于选择并恢复过去会话的 UI。

```sh
wx claude --resume
wx codex resume
```

## 更多功能

- **状态与诊断：** `wx status`、`wx doctor`。
- **会话管理与清理：** `wx slots`、`wx resume`、`wx gc --dry-run`、`wx clear`。
- **配置：** 使用 `wx config` 查看配置或修改单个配置值。

命令和选项的详细用法，请参阅 `wx --help` 和 `wx <command> --help`。

## 卸载

```sh
curl -fsSL https://github.com/HappyOnigiri/WX/releases/latest/download/uninstall.sh | bash
```

删除 wx 管理的 worktree（包括未保存的工作）、hook 条目、LaunchAgent、配置文件和可执行文件。

## 参与贡献

欢迎参与贡献！
你可以通过 [Issues](https://github.com/HappyOnigiri/WX/issues) 报告问题或提出想法，也可以提交 [Pull Request](https://github.com/HappyOnigiri/WX/pulls)。
同样欢迎改进文档和翻译。
