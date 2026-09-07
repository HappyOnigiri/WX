#!/bin/sh
set -eu

# make test-darwin から export された GO・PKG・VERBOSE だけを読み、macOS実機向けの部分検証を1回実行する。
# 実行環境を表示し、hostがmacOSでないかクロス設定が残っているときはgo testを始めずに失敗する。
# 実行自体は scripts/test-focus.sh へ委譲し、引数の引用と終了状態の伝播を二重に実装しない。
go_command=${GO:-go}
package=${PKG:-}
verbose=${VERBOSE:-}
script_directory=$(dirname "$0")

fail() {
  echo "test-darwin: $1" >&2
  exit 1
}

if [ -z "$package" ]; then
  fail "PKG is required; run this through make test-darwin"
fi

# 前提確認に使うコマンドはmacOS標準だが、PATHの欠落を値の欠落と区別して報告する。
for tool in uname sw_vers mktemp; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required for the macOS preflight"
done

host_kernel=$(uname -s) || fail "uname -s failed"
[ "$host_kernel" = Darwin ] || fail "this target verifies darwin-only code on a real macOS host, got $host_kernel"

# クロスビルド設定が残っているとdarwin専用のテストファイルがそもそもビルド対象にならない。
# 「実行したが対象が0件」を成功と誤解させないため、hostと違うGOOS/GOARCHは前提未成立として拒否する。
host_goos=$("$go_command" env GOHOSTOS) || fail "go env GOHOSTOS failed"
host_goarch=$("$go_command" env GOHOSTARCH) || fail "go env GOHOSTARCH failed"
target_goos=$("$go_command" env GOOS) || fail "go env GOOS failed"
target_goarch=$("$go_command" env GOARCH) || fail "go env GOARCH failed"
go_version=$("$go_command" env GOVERSION) || fail "go env GOVERSION failed"
[ "$host_goos" = darwin ] || fail "go reports GOHOSTOS=$host_goos, which is not a macOS toolchain"
[ "$target_goos" = "$host_goos" ] || fail "GOOS=$target_goos differs from the host $host_goos; unset it before running this target"
[ "$target_goarch" = "$host_goarch" ] || fail "GOARCH=$target_goarch differs from the host $host_goarch; unset it before running this target"
[ -n "$go_version" ] || fail "go env GOVERSION returned no value"

os_version=$(sw_vers -productVersion) || fail "sw_vers -productVersion failed"
os_build=$(sw_vers -buildVersion) || fail "sw_vers -buildVersion failed"
[ -n "$os_version" ] && [ -n "$os_build" ] || fail "sw_vers returned no version"
echo "test-darwin: macOS $os_version ($os_build) arch=$target_goarch go=$go_version" >&2

# 専用のTMPDIRを作り、前提確認の失敗・テスト失敗・中断のいずれでも残さない。
# APFSであることは internal/workspace の TestMain が判定するため、ここでは場所だけを決める。
work_directory=$(mktemp -d "${TMPDIR:-/tmp}/wx-test-darwin.XXXXXX") || fail "mktemp -d failed"
trap 'rm -rf "$work_directory"' EXIT HUP INT TERM
echo "test-darwin: tmpdir=$work_directory" >&2

# RUNは空に固定し、この入口が常にパッケージ全体を対象にすることを呼び出し側から見て明らかにする。
status=0
TMPDIR="$work_directory" GO="$go_command" PKG="$package" RUN='' VERBOSE="$verbose" \
  "$script_directory/test-focus.sh" || status=$?
exit "$status"
