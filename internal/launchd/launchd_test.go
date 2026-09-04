package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/HappyOnigiri/WX/internal/config"
)

func TestRenderInstallUninstallAndKickstart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	logPath := filepath.Join(home, "Library", "Logs", "wx", "daemon.log")
	data, err := Render("/usr/local/bin/wx", home, logPath)
	if err != nil || !strings.Contains(string(data), Label) || !strings.Contains(string(data), "/usr/local/bin/wx") {
		t.Fatalf("rendered plist=%q err=%v", data, err)
	}
	// The plist is the only caller of the foreground mode, so a rename that
	// missed it would leave launchd running an unknown action in a KeepAlive
	// loop rather than failing anywhere a test would notice.
	for _, argument := range []string{"<string>daemon</string>", "<string>start</string>", "<string>--foreground</string>"} {
		if !strings.Contains(string(data), argument) {
			t.Fatalf("rendered plist does not pass %s: %s", argument, data)
		}
	}
	if err := Install(context.Background(), "/usr/local/bin/wx", logPath); err != nil {
		t.Fatal(err)
	}
	path, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plist info=%v err=%v", info, err)
	}
	if err := Kickstart(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist remains after uninstall: %v", err)
	}
}

func TestRenderEscapesAndPreservesSpecialCharacterPaths(t *testing.T) {
	binaryPath := `/tmp/wx & <binary> "quoted"`
	home := `/tmp/home & <user> "quoted"`
	logPath := `/tmp/logs & <wx> "daemon".log`
	data, err := Render(binaryPath, home, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "&amp;") || !strings.Contains(string(data), "&lt;") || !strings.Contains(string(data), "&gt;") {
		t.Fatalf("special characters were not XML escaped: %s", data)
	}
	values := make([]string, 0)
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, decodeErr := decoder.Token()
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			t.Fatalf("rendered plist is not well-formed XML: %v", decodeErr)
		}
		if text, ok := token.(xml.CharData); ok {
			values = append(values, string(text))
		}
	}
	joined := strings.Join(values, "\n")
	for _, expected := range []string{binaryPath, home, filepath.Join(home, ".local", "bin") + ":/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin", logPath} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("decoded plist does not preserve %q: %s", expected, joined)
		}
	}
}

func TestCurrentPlistStatusDistinguishesCurrentStaleAndUnknown(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(bin, "wx")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	logPath, err := config.LogPath()
	if err != nil {
		t.Fatal(err)
	}
	plist, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(plist), 0o700); err != nil {
		t.Fatal(err)
	}
	expected, err := Render(binary, home, logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, expected, 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := CurrentPlistStatus(); status != PlistCurrent || err != nil {
		t.Fatalf("current plist status=%v err=%v", status, err)
	}
	// This is the stale shape produced by the old daemon command contract.
	if err := os.WriteFile(plist, []byte("<string>daemon start --foreground</string>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := CurrentPlistStatus(); status != PlistStale || err != nil {
		t.Fatalf("stale plist status=%v err=%v", status, err)
	}
	if err := os.Remove(plist); err != nil {
		t.Fatal(err)
	}
	status, err := CurrentPlistStatus()
	if status != PlistUnknown || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing plist status=%v err=%v", status, err)
	}
}

func TestUninstallRemovesPlistWhenServiceIsAlreadyMissing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(bin, "launchctl")
	if err := os.WriteFile(launchctl, []byte("#!/bin/sh\necho 'Could not find service' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	path, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist remains after missing service uninstall: %v", err)
	}
}

func TestKickstartReportsAnUninstalledService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho 'Could not find service' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	err := Kickstart(context.Background())
	if !errors.Is(err, ErrServiceMissing) {
		t.Fatalf("kickstart error=%v, want ErrServiceMissing", err)
	}
	if !strings.Contains(err.Error(), "Could not find service") {
		t.Fatalf("kickstart error does not carry the launchctl output: %v", err)
	}
}

func TestKickstartReportsAFailureThatProducedNoOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// An empty PATH makes exec fail before launchctl can print anything, which
	// is the shape of every failure that leaves CombinedOutput empty (a
	// cancelled context and a reached deadline included). The wrapped error is
	// then all the daemon log and wx daemon restart have to report.
	t.Setenv("PATH", "")
	err := Kickstart(context.Background())
	if err == nil {
		t.Fatal("kickstart succeeded without launchctl on PATH")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("kickstart error=%v, want it to carry %v", err, exec.ErrNotFound)
	}
}

func TestLaunchctlFailuresAreReported(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho failed >&2\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := Install(context.Background(), "/wx", filepath.Join(home, "daemon.log")); err == nil {
		t.Fatal("failed bootstrap succeeded")
	}
	if err := Uninstall(context.Background()); err == nil {
		t.Fatal("failed bootout succeeded")
	}
	if err := Kickstart(context.Background()); err == nil {
		t.Fatal("failed kickstart succeeded")
	}
}

// TestStartAsksLaunchdWithoutKillingTheRunningService pins the one difference
// between Start and Kickstart. Passing -k here would make wx daemon start tear
// down a daemon that is serving other sessions, which is exactly what the
// separate entry point exists to avoid.
func TestStartAsksLaunchdWithoutKillingTheRunningService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(home, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + record + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), Label)
	if got := strings.Fields(string(argv)); len(got) != 2 || got[0] != "kickstart" || got[1] != target {
		t.Fatalf("launchctl argv=%q, want [kickstart %s]", got, target)
	}
	if err := os.Truncate(record, 0); err != nil {
		t.Fatal(err)
	}
	if err := Kickstart(context.Background()); err != nil {
		t.Fatal(err)
	}
	argv, err = os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(argv)); len(got) != 3 || got[1] != "-k" {
		t.Fatalf("launchctl argv=%q, want kickstart -k %s", got, target)
	}
}

func TestStartReportsAnUninstalledService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte("#!/bin/sh\necho 'Could not find service' >&2\nexit 3\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := Start(context.Background()); !errors.Is(err, ErrServiceMissing) {
		t.Fatalf("start error=%v, want ErrServiceMissing", err)
	}
}
