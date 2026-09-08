#!/bin/bash
set -euo pipefail

# 開発ビルドの VERSION 推定を使わず、CI のリリースブランチから決めたタグだけを受け取る。
release_version=${RELEASE_VERSION:-}
[[ "$release_version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
  echo 'release: RELEASE_VERSION must be vX.Y.Z' >&2
  exit 1
}
go_command=${GO:-go}
release_dir=${RELEASE_DIR:-artifacts/release}
script_directory=$(cd "$(dirname "$0")" && pwd)
scratch=$(mktemp -d "${TMPDIR:-/tmp}/wx-release.XXXXXX")
trap 'rm -rf "$scratch"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 "$go_command" build -trimpath \
  -ldflags "-s -w -X github.com/HappyOnigiri/WX/internal/version.Version=$release_version -X github.com/HappyOnigiri/WX/internal/version.BuildMeta=" \
  -o "$scratch/wx-darwin-arm64" ./cmd/wx
sed "s/@WX_RELEASE_VERSION@/$release_version/g" "$script_directory/install.sh" > "$scratch/install.sh"
(cd "$scratch" && shasum -a 256 wx-darwin-arm64 > checksums.txt)
mkdir -p "$release_dir"
cp "$scratch/wx-darwin-arm64" "$scratch/install.sh" "$scratch/checksums.txt" "$release_dir/"
