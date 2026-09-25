#!/usr/bin/env bash
# 自身をメモリ上限付きのcgroup v2へ移してから、引数のcommandへexecする。
# 使い方: with-memory-cgroup.sh <cgroup名> <上限bytes> -- <command> [args...]
#
# 上限はprocessごとのrlimitではなく配下のprocess全体の合計に掛かる。
# 超過時はcgroup内のprocessだけがOOM killerで停止し、runner本体とhost全体のswapは守られる。
# sudoは移動の書込みだけに使い、commandは呼び出し元のuid・環境・process groupのまま動かす。
set -euo pipefail

if [[ $# -lt 4 || $3 != -- ]]; then
  echo "usage: $0 <cgroup-name> <memory-max-bytes> -- <command> [args...]" >&2
  exit 2
fi
name=$1
limit=$2
shift 3
case "$name" in '' | *[!A-Za-z0-9._-]*) echo "invalid cgroup name: $name" >&2; exit 2 ;; esac
case "$limit" in '' | *[!0-9]*) echo "invalid memory limit: $limit" >&2; exit 2 ;; esac

cgroup=/sys/fs/cgroup/$name
sudo mkdir -p "$cgroup"
echo "$limit" | sudo tee "$cgroup/memory.max" >/dev/null
# swapへ逃がすとOOMにならずhost全体が極端に遅くなり、runnerの応答が途絶えるため禁止する。
echo 0 | sudo tee "$cgroup/memory.swap.max" >/dev/null
echo $$ | sudo tee "$cgroup/cgroup.procs" >/dev/null
exec "$@"
