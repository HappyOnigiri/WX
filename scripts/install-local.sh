#!/bin/bash
set -euo pipefail

# make install が置いた開発ビルドを、既定の配置先へ入れたときだけ実際に使われる状態へ揃える。
# 判定を Makefile の recipe に書くと shellcheck の対象（scripts/*.sh）から外れるため、ここへ置く。
#
# 状態ごとの install / update / start / restart の選び分けは wx setup が既に持っているので、
# ここでは推奨操作を当てるだけにして、同じ判定を shell へ書き写さない。

install_dir=${1:-}
[ -n "$install_dir" ] || {
  echo "install-local.sh: the install directory is required" >&2
  exit 2
}

# 既定以外の配置先は make smoke のような検査用の呼び出しなので、副作用を一切起こさない。
# 一致しない側へ倒すのが安全側である。
[ "$install_dir" = "$HOME/.local/bin" ] || exit 0

# LaunchAgent と launchctl は macOS にしかなく、CI の runner は全て linux である。
[ "$(uname -s)" = Darwin ] || exit 0

binary="$install_dir/wx"
[ -x "$binary" ] || {
  echo "install-local.sh: $binary is not executable" >&2
  exit 1
}

# launchd.ResolveBinary と hookconfig.ResolveHookBinary は PATH 上の wx を先に見る。
# 配置先を先頭へ置かないと plist と hook に repository 内の bin/wx が焼き付き、
# 以後の setup が開発ビルドの不一致として警告し続ける。
export PATH="$install_dir:$PATH"

# plist を書き換えると launchd.Install が bootout/bootstrap するため、daemon の状態は
# その後に見る必要がある。この順序は変えない。
for item in launch_agent daemon hooks.claude hooks.codex; do
  "$binary" setup --item "$item" --action recommended
done

# setup の daemon 項目は未起動の起動と、応答が壊れている daemon の入れ替えしか見ない。
# 置いたばかりの binary への入れ替えはここで同期的に待つ。
if ! "$binary" daemon restart; then
  # state.db を開けない daemon は RequestRestart を拒む。stop はその状態でも通るので、
  # 止めてから launchd に起動し直させる。
  "$binary" daemon stop
  "$binary" daemon start
fi
