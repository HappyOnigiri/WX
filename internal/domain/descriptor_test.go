package domain

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unsafe"
)

// descriptorFileInfo keeps FileIdentity tests independent of the platform's
// syscall.Stat_t layout while still exercising every supported field shape.
type descriptorFileInfo struct {
	sys any
}

func (i descriptorFileInfo) Name() string       { return "test" }
func (i descriptorFileInfo) Size() int64        { return 0 }
func (i descriptorFileInfo) Mode() os.FileMode  { return 0 }
func (i descriptorFileInfo) ModTime() time.Time { return time.Time{} }
func (i descriptorFileInfo) IsDir() bool        { return false }
func (i descriptorFileInfo) Sys() any           { return i.sys }

type descriptorUnsignedIdentity struct {
	Dev uint64
	Ino uint32
}

type descriptorSignedIdentity struct {
	Dev int64
	Ino int32
}

type descriptorNegativeIdentity struct {
	Dev int64
	Ino int64
}

type descriptorInvalidIdentity struct {
	Dev string
	Ino bool
}

func TestDescriptorOperationsPinAndValidateDirectories(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := OpenDirectoryAt(nil, "."); err == nil {
		t.Fatal("nil owner was accepted")
	}
	if _, err := OpenRootAt(nil, "."); err == nil {
		t.Fatal("nil owner was accepted by OpenRootAt")
	}
	if _, _, err := OpenOwnedDirectory(root, child); err != nil {
		t.Fatalf("OpenOwnedDirectory: %v", err)
	}

	owner, relative, err := OpenOwnedRoot(root, child)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Close() }()
	if relative != "child" {
		t.Fatalf("relative=%q", relative)
	}
	directory, identity, err := OpenDirectoryAt(owner, relative)
	if err != nil {
		t.Fatalf("OpenDirectoryAt: %v", err)
	}
	if identity == "" {
		t.Fatal("directory identity is empty")
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	childRoot, err := OpenRootAt(owner, relative)
	if err != nil {
		t.Fatalf("OpenRootAt: %v", err)
	}
	if err := childRoot.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "missing", path: "missing"},
		{name: "regular", path: "../regular"},
		{name: "empty", path: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := OpenDirectoryAt(owner, test.path); err == nil {
				t.Fatal("non-directory path was accepted")
			}
			if _, err := OpenRootAt(owner, test.path); err == nil {
				t.Fatal("non-directory path was accepted by OpenRootAt")
			}
		})
	}
	if _, _, err := OpenDirectoryAt(owner, "child"); err != nil {
		t.Fatalf("reopening child: %v", err)
	}
}

func TestFileIdentityRejectsUnavailableAndUnsupportedMetadata(t *testing.T) {
	for _, info := range []os.FileInfo{
		nil,
		descriptorFileInfo{},
		descriptorFileInfo{sys: (*descriptorUnsignedIdentity)(nil)},
		descriptorFileInfo{sys: 42},
		descriptorFileInfo{sys: descriptorInvalidIdentity{Dev: "device", Ino: true}},
		descriptorFileInfo{sys: descriptorNegativeIdentity{Dev: -1, Ino: 1}},
		descriptorFileInfo{sys: descriptorNegativeIdentity{Dev: 1, Ino: -1}},
	} {
		if _, err := FileIdentity(info); err == nil {
			t.Fatalf("unsupported metadata accepted: %#v", info)
		}
	}

	for _, test := range []struct {
		name string
		sys  any
		want string
	}{
		{name: "unsigned", sys: descriptorUnsignedIdentity{Dev: 7, Ino: 11}, want: "7:11"},
		{name: "signed", sys: descriptorSignedIdentity{Dev: 13, Ino: 17}, want: "13:17"},
	} {
		t.Run(test.name, func(t *testing.T) {
			identity, err := FileIdentity(descriptorFileInfo{sys: test.sys})
			if err != nil {
				t.Fatal(err)
			}
			if identity != test.want {
				t.Fatalf("identity=%q want %q", identity, test.want)
			}
		})
	}
}

func TestUnsignedStatFieldCoversAllReflectionKinds(t *testing.T) {
	valid := []struct {
		name  string
		value reflect.Value
		want  uint64
	}{
		{name: "uint", value: reflect.ValueOf(uint(7)), want: 7},
		{name: "uint8", value: reflect.ValueOf(uint8(8)), want: 8},
		{name: "uint16", value: reflect.ValueOf(uint16(9)), want: 9},
		{name: "uint32", value: reflect.ValueOf(uint32(10)), want: 10},
		{name: "uint64", value: reflect.ValueOf(uint64(11)), want: 11},
		{name: "uintptr", value: reflect.ValueOf(uintptr(12)), want: 12},
		{name: "int", value: reflect.ValueOf(int(13)), want: 13},
		{name: "int8", value: reflect.ValueOf(int8(14)), want: 14},
		{name: "int16", value: reflect.ValueOf(int16(15)), want: 15},
		{name: "int32", value: reflect.ValueOf(int32(16)), want: 16},
		{name: "int64", value: reflect.ValueOf(int64(17)), want: 17},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			got, ok := unsignedStatField(test.value)
			if !ok || got != test.want {
				t.Fatalf("unsignedStatField=%d,%v want %d,true", got, ok, test.want)
			}
		})
	}
	var pointed int
	invalid := []reflect.Value{
		{},
		reflect.ValueOf(false), reflect.ValueOf(float32(1)), reflect.ValueOf(float64(1)),
		reflect.ValueOf(complex64(1)), reflect.ValueOf(complex128(1)), reflect.ValueOf([1]byte{}),
		reflect.ValueOf(make(chan int)), reflect.ValueOf(func() {}),
		reflect.ValueOf(map[string]int{}), reflect.ValueOf((*int)(nil)), reflect.ValueOf([]int{}),
		reflect.ValueOf("text"), reflect.ValueOf(struct{}{}), reflect.ValueOf(unsafe.Pointer(&pointed)),
	}
	var interfaceValue any = 1
	invalid = append(invalid, reflect.ValueOf(&interfaceValue).Elem())
	for _, value := range invalid {
		if got, ok := unsignedStatField(value); ok || got != 0 {
			t.Fatalf("unsupported kind %v returned %d,%v", value.Kind(), got, ok)
		}
	}
}
