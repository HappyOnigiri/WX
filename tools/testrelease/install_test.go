package testrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRegistersDaemonAndPinsRelease(t *testing.T) {
	t.Parallel()
	f := newInstallFixture(t)
	output, err := f.run(t, "RELEASE_VERSION=v9.9.9")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	if got := readFile(t, f.destination()); got != fakeBinary {
		t.Fatalf("installed binary=%q", got)
	}
	if got := readFile(t, filepath.Join(f.root, "wx.log")); got != "stop\ninstall\nstart\n" {
		t.Fatalf("daemon calls=%q", got)
	}
	for _, url := range strings.Fields(readFile(t, filepath.Join(f.root, "curl.log"))) {
		if !strings.HasPrefix(url, "https://github.com/HappyOnigiri/WX/releases/download/v1.2.3/") {
			t.Fatalf("download is not pinned: %q", url)
		}
	}
	if !strings.Contains(output, `export PATH="$HOME/.local/bin:$PATH"`) {
		t.Fatalf("missing PATH instructions: %s", output)
	}
	if _, err := os.Stat(filepath.Join(f.home, ".zshrc")); !os.IsNotExist(err) {
		t.Fatalf("shell configuration was changed: %v", err)
	}
	info, err := os.Stat(f.destination())
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("installed permissions: %v, %v", info, err)
	}
	assertNoInstallTemps(t, f)
}

func TestInstallUpdatesAndMigratesDaemon(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, registered, loaded, want string
	}{
		{name: "same path", registered: "current", loaded: "0", want: "restart\n"},
		{name: "old path", registered: "/old/wx", loaded: "0", want: "stop\ninstall\nstart\n"},
		{name: "unloaded", registered: "current", loaded: "1", want: "stop\ninstall\nstart\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInstallFixture(t)
			registered := tc.registered
			if registered == "current" {
				registered = f.destination()
			}
			f.existing(t, registered)
			if output, err := f.run(t, "FAKE_LAUNCHCTL_STATUS="+tc.loaded); err != nil {
				t.Fatalf("update: %v\n%s", err, output)
			}
			if got := readFile(t, filepath.Join(f.root, "wx.log")); got != tc.want {
				t.Fatalf("daemon calls=%q, want %q", got, tc.want)
			}
			if readFile(t, f.destination()) != fakeBinary {
				t.Fatal("binary was not updated")
			}
		})
	}
}

func TestInstallFailurePreservesExistingBinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, environment string
		corrupt           bool
	}{
		{name: "unsupported OS", environment: "FAKE_OS=Linux"},
		{name: "unsupported CPU", environment: "FAKE_ARCH=x86_64"},
		{name: "binary download", environment: "FAKE_DOWNLOAD_FAIL=wx-darwin-arm64"},
		{name: "checksum download", environment: "FAKE_DOWNLOAD_FAIL=checksums.txt"},
		{name: "wrong version", environment: "FAKE_BINARY_VERSION=v1.2.2"},
		{name: "stop failed", environment: "FAKE_FAIL=stop"},
		{name: "checksum mismatch", corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newInstallFixture(t)
			f.existing(t, "/old/wx")
			if tc.corrupt {
				writeFile(t, filepath.Join(f.assets, "wx-darwin-arm64"), "corrupt", 0o755)
			}
			if output, err := f.run(t, tc.environment); err == nil {
				t.Fatalf("install unexpectedly succeeded: %s", output)
			}
			if got := readFile(t, f.destination()); got != "old binary" {
				t.Fatalf("old binary was changed to %q", got)
			}
			assertNoInstallTemps(t, f)
		})
	}
}

func TestInstallReportsDaemonFailureAfterBinaryPlacement(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"install", "start", "restart"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			f := newInstallFixture(t)
			if action == "restart" {
				f.existing(t, f.destination())
			}
			output, err := f.run(t, "FAKE_FAIL="+action, "FAKE_LAUNCHCTL_STATUS=0")
			if err == nil || !strings.Contains(output, "binary installed, but") || !strings.Contains(output, "wx daemon "+action) {
				t.Fatalf("daemon failure was not reported: %v\n%s", err, output)
			}
			if readFile(t, f.destination()) != fakeBinary {
				t.Fatal("verified binary was not retained")
			}
			assertNoInstallTemps(t, f)
		})
	}
}

func TestInstallRejectsTemplateAndInvalidChecksum(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"template", "checksum name", "checksum format", "truncated script"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newInstallFixture(t)
			f.existing(t, "/old/wx")
			switch name {
			case "template":
				f.installer = readFile(t, "../../scripts/install.sh")
			case "checksum name":
				writeFile(t, filepath.Join(f.assets, "checksums.txt"), strings.Repeat("a", 64)+"  ../other\n", 0o644)
			case "checksum format":
				writeFile(t, filepath.Join(f.assets, "checksums.txt"), "bad checksum\n", 0o644)
			case "truncated script":
				prefix, _, found := strings.Cut(f.installer, "  local install_dir")
				if !found {
					t.Fatal("cannot locate truncation point")
				}
				f.installer = prefix
			}
			if output, err := f.run(t); err == nil {
				t.Fatalf("install unexpectedly succeeded: %s", output)
			}
			if readFile(t, f.destination()) != "old binary" {
				t.Fatal("old binary was replaced")
			}
		})
	}
}

// 一時配置先も同じファイルシステムに作るため、終了時にはダウンロード先と両方が消える。
func assertNoInstallTemps(t *testing.T, f installFixture) {
	t.Helper()
	for _, pattern := range []string{filepath.Join(f.root, "wx-install.*"), filepath.Join(f.home, ".local", "bin", ".wx-install.*")} {
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) != 0 {
			t.Fatalf("temporary files remain: %v, %v", matches, err)
		}
	}
}

func TestInstallReplacesLinkWithoutChangingOriginalBinary(t *testing.T) {
	t.Parallel()
	f := newInstallFixture(t)
	f.existing(t, f.destination())
	original := filepath.Join(f.root, "original-wx")
	if err := os.Rename(f.destination(), original); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(original, f.destination()); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(t, "FAKE_LAUNCHCTL_STATUS=0"); err != nil {
		t.Fatalf("install: %v\n%s", err, output)
	}
	if readFile(t, original) != "old binary" || readFile(t, f.destination()) != fakeBinary {
		t.Fatal("replacement changed the original binary or did not install the new binary")
	}
}
