// testrelease は配布用ビルドとインストーラーを、GitHub・実ユーザーの LaunchAgent を変更せず検査する。
package testrelease

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fakeBinary = `#!/bin/sh
if [ "$1" = --version ]; then
  echo "wx version ${FAKE_BINARY_VERSION:-v1.2.3}"
  exit 0
fi
if [ "$1" = setup ]; then
  echo "setup $2" >> "$FAKE_WX_LOG"
  [ "setup" != "${FAKE_FAIL:-}" ] || exit 23
  exit 0
fi
[ "$1" = daemon ] || exit 64
echo "$2" >> "$FAKE_WX_LOG"
[ "$2" != "${FAKE_FAIL:-}" ] || exit 23
if [ "$2" = install ]; then
  [ "$(command -v wx)" = "$HOME/.local/bin/wx" ] || exit 24
fi
`

const fakeCurl = `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://*) url=$1 ;;
    --output) shift; destination=$1 ;;
  esac
  shift
done
echo "$url" >> "$FAKE_CURL_LOG"
name=${url##*/}
[ "$name" != "${FAKE_DOWNLOAD_FAIL:-}" ] || exit 22
cp "$FAKE_ASSETS/$name" "$destination"
`

const fakeUname = `#!/bin/sh
case "$1" in
  -s) echo "${FAKE_OS:-Darwin}" ;;
  -m) echo "${FAKE_ARCH:-arm64}" ;;
  *) exit 64 ;;
esac
`

type installFixture struct {
	root, home, bin, assets, installer string
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func newInstallFixture(t *testing.T) installFixture {
	t.Helper()
	root := t.TempDir()
	f := installFixture{
		root: root, home: filepath.Join(root, "user's home"),
		bin: filepath.Join(root, "bin"), assets: filepath.Join(root, "assets"),
		installer: strings.ReplaceAll(readFile(t, "../../scripts/install.sh"), "@WX_RELEASE_VERSION@", "v1.2.3"),
	}
	writeFile(t, filepath.Join(f.assets, "wx-darwin-arm64"), fakeBinary, 0o755)
	f.checksum(t)
	for name, body := range map[string]string{
		"curl": fakeCurl, "uname": fakeUname,
		"git":       "#!/bin/sh\nexit 0\n",
		"launchctl": "#!/bin/sh\n[ \"$1\" = print ] || exit 64\nexit \"${FAKE_LAUNCHCTL_STATUS:-1}\"\n",
		"plutil":    "#!/bin/sh\ncat \"$HOME/Library/LaunchAgents/com.user.wx.plist\"\n",
		"wx":        "#!/bin/sh\necho 'wrong wx on PATH' >&2\nexit 99\n",
	} {
		writeFile(t, filepath.Join(f.bin, name), body, 0o755)
	}
	return f
}

func (f installFixture) checksum(t *testing.T) {
	t.Helper()
	digest := sha256.Sum256([]byte(readFile(t, filepath.Join(f.assets, "wx-darwin-arm64"))))
	writeFile(t, filepath.Join(f.assets, "checksums.txt"), fmt.Sprintf("%x  wx-darwin-arm64\n", digest), 0o644)
}

func (f installFixture) run(t *testing.T, environment ...string) (string, error) {
	t.Helper()
	command := exec.Command("/bin/bash")
	command.Stdin = strings.NewReader(f.installer)
	command.Env = append([]string{
		"PATH=" + f.bin + ":/usr/bin:/bin", "HOME=" + f.home,
		"TMPDIR=" + f.root, "FAKE_ASSETS=" + f.assets,
		"FAKE_WX_LOG=" + filepath.Join(f.root, "wx.log"),
		"FAKE_CURL_LOG=" + filepath.Join(f.root, "curl.log"),
	}, environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}

func (f installFixture) existing(t *testing.T, registered string) {
	t.Helper()
	writeFile(t, f.destination(), "old binary", 0o755)
	writeFile(t, filepath.Join(f.home, "Library", "LaunchAgents", "com.user.wx.plist"), registered, 0o644)
}

func (f installFixture) destination() string {
	return filepath.Join(f.home, ".local", "bin", "wx")
}
