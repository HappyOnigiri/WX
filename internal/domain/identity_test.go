package domain

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// TestFileIdentityIsStableAcrossDeviceNumbers は identity が inode と volume だけで決まることを検証する。
// device 番号は macOS では再起動で変わるため、identity に含まれていれば記録済み row が一斉に一致しなくなる。
func TestFileIdentityIsStableAcrossDeviceNumbers(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	identity, err := FileIdentity(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("unsupported file identity type %T", info.Sys())
	}
	inode, volume, ok := IdentityFields(identity)
	if !ok || volume == "" {
		t.Fatalf("IdentityFields(%q) inode=%q volume=%q ok=%v", identity, inode, volume, ok)
	}
	if inode != strconv.FormatUint(stat.Ino, 10) {
		t.Fatalf("identity inode=%q, want %d", inode, stat.Ino)
	}
	if identity != FormatIdentity(inode, volume) {
		t.Fatalf("identity=%q, want %q", identity, FormatIdentity(inode, volume))
	}
	if fmt.Sprint(stat.Dev) == volume {
		t.Fatalf("identity volume=%q still carries the device number", volume)
	}
}

func TestFileIdentityRejectsUnusableDescriptors(t *testing.T) {
	closed, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]*os.File{"nil": nil, "closed": closed} {
		if _, err := FileIdentity(file); err == nil {
			t.Errorf("FileIdentity(%s) succeeded", name)
		}
	}
}

// TestIdentityFieldsSeparatesLegacyRecords は、旧 dev:ino 形式を volume 空で返す分解規則を固定する。
// 記録済み identity の移行判定はこの区別に依存する。
func TestIdentityFieldsSeparatesLegacyRecords(t *testing.T) {
	for _, test := range []struct {
		identity, inode, volume string
		ok                      bool
	}{
		{identity: "vol:11:/System/Volumes/Data", inode: "11", volume: "/System/Volumes/Data", ok: true},
		{identity: "vol:11:/Volumes/a:b", inode: "11", volume: "/Volumes/a:b", ok: true},
		{identity: "7:11", inode: "11", ok: true},
		{identity: "vol:11:", inode: "11"},
		{identity: "vol::/System/Volumes/Data", volume: "/System/Volumes/Data"},
		{identity: "11"},
		{identity: ""},
	} {
		inode, volume, ok := IdentityFields(test.identity)
		if inode != test.inode || volume != test.volume || ok != test.ok {
			t.Errorf("IdentityFields(%q)=(%q,%q,%v), want (%q,%q,%v)", test.identity, inode, volume, ok, test.inode, test.volume, test.ok)
		}
	}
}

// TestFileIdentityDistinguishesDirectories は、同じ volume 上の別 directory が別 identity になることを確かめる。
func TestFileIdentityDistinguishesDirectories(t *testing.T) {
	root := t.TempDir()
	identities := map[string]string{}
	for _, name := range []string{"first", "second"} {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := FileIdentity(file)
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if err != nil {
			t.Fatal(err)
		}
		identities[name] = identity
	}
	if identities["first"] == identities["second"] {
		t.Fatalf("distinct directories share identity %q", identities["first"])
	}
}
