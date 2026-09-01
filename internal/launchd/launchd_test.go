package launchd

import (
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
	if err := Install("/usr/local/bin/wx", logPath); err != nil {
		t.Fatal(err)
	}
	path, err := PlistPath()
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plist info=%v err=%v", info, err)
	}
	if err := Kickstart(); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist remains after uninstall: %v", err)
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
	if err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plist remains after missing service uninstall: %v", err)
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
	if err := Install("/wx", filepath.Join(home, "daemon.log")); err == nil {
		t.Fatal("failed bootstrap succeeded")
	}
	if err := Uninstall(); err == nil {
		t.Fatal("failed bootout succeeded")
	}
	if err := Kickstart(); err == nil {
		t.Fatal("failed kickstart succeeded")
	}
}
