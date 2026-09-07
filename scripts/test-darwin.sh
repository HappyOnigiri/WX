#!/bin/sh
set -eu

# make test-darwin から export された GO・PKG・VERBOSE だけを読み、macOS実機向けの部分検証を1回実行する。
# 実行環境と一時ディレクトリのfilesystemを表示し、前提が成り立たないときはgo testを始めずに失敗する。
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
for tool in uname sw_vers df diskutil plutil mktemp; do
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

# 専用のTMPDIRを作り、filesystemを判定した場所とテストが使う場所を一致させる。
# 前提確認の失敗・テスト失敗・中断のいずれでも残さない。
work_directory=$(mktemp -d "${TMPDIR:-/tmp}/wx-test-darwin.XXXXXX") || fail "mktemp -d failed"
trap 'rm -rf "$work_directory"' EXIT HUP INT TERM

device=$(df -P "$work_directory" | awk 'NR == 2 { print $1 }') || fail "df -P $work_directory failed"
[ -n "$device" ] || fail "df -P reported no device for $work_directory"
device_info=$(diskutil info -plist "$device") || fail "diskutil info -plist $device failed"
filesystem=$(printf '%s' "$device_info" | plutil -extract FilesystemType raw -o - -) ||
  fail "diskutil did not report FilesystemType for $device"
[ -n "$filesystem" ] || fail "diskutil reported an empty FilesystemType for $device"
echo "test-darwin: tmpdir=$work_directory device=$device filesystem=$filesystem" >&2

# CoWのテストはclonefileが効くAPFSを前提にする。非APFSでは前提未成立としてテスト前に終える。
case $(printf '%s' "$filesystem" | tr '[:upper:]' '[:lower:]') in
  apfs) ;;
  *) fail "the CoW tests require an APFS temporary directory, but $work_directory is $filesystem on $device" ;;
esac

# RUNは空に固定し、この入口が常にパッケージ全体を対象にすることを呼び出し側から見て明らかにする。
status=0
TMPDIR="$work_directory" GO="$go_command" PKG="$package" RUN='' VERBOSE="$verbose" \
  "$script_directory/test-focus.sh" || status=$?
exit "$status"
