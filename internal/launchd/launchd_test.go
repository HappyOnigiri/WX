package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
