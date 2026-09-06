package domain

import (
	"errors"
	"os"
)

// OpenOwnedDirectory は root を起点とする descriptor 経由で既存ディレクトリを開く。
// 返す FD はディレクトリ inode 自体を参照するため、子プロセス起動中も保持すれば
// パス名の rename や置換で子の作業ディレクトリが変わることを防げる。
func OpenOwnedDirectory(root, path string) (*os.File, string, error) {
	ownedRoot, relative, err := OpenOwnedRoot(root, path)
	if err != nil {
		return nil, "", err
	}
	file, identity, openErr := OpenDirectoryAt(ownedRoot, relative)
	closeErr := ownedRoot.Close()
	if openErr != nil {
		return nil, "", openErr
	}
	if closeErr != nil {
		_ = file.Close()
		return nil, "", closeErr
	}
	return file, identity, nil
}

// OpenDirectoryAt は、既に pin された os.Root を基準にディレクトリを開く。
// owner は呼び出し元が所有するため、この関数では意図的に close しない。
func OpenDirectoryAt(owner *os.Root, relative string) (*os.File, string, error) {
	if owner == nil {
		return nil, "", errors.New("owned root is nil")
	}
	expected, err := PhysicalPathInfo(owner, relative)
	if err != nil {
		return nil, "", err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, "", errors.New("owned path is not a physical directory")
	}
	file, err := owner.Open(relative)
	if err != nil {
		return nil, "", err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, info) {
		_ = file.Close()
		return nil, "", errors.New("owned path changed while opening")
	}
	current, err := PhysicalPathInfo(owner, relative)
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		_ = file.Close()
		return nil, "", errors.New("owned path changed while opening")
	}
	identity, err := FileIdentity(file)
	if err != nil {
		_ = file.Close()
		return nil, "", err
	}
	return file, identity, nil
}

// OpenRootAt は OpenRoot の前後でディレクトリ inode を検査し、既存ディレクトリを
// 子 Root として開き直す。pin された worktree 配下で複数の descriptor 相対操作を
// 行う呼び出し元向けの、Root 版 OpenDirectoryAt である。
func OpenRootAt(owner *os.Root, relative string) (*os.Root, error) {
	if owner == nil {
		return nil, errors.New("owned root is nil")
	}
	expected, err := PhysicalPathInfo(owner, relative)
	if err != nil {
		return nil, err
	}
	if !expected.IsDir() || expected.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("owned path is not a physical directory")
	}
	child, err := owner.OpenRoot(relative)
	if err != nil {
		return nil, err
	}
	opened, err := child.Lstat(".")
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if opened.Mode()&os.ModeSymlink != 0 || !opened.IsDir() || !os.SameFile(expected, opened) {
		_ = child.Close()
		return nil, errors.New("owned path changed while opening")
	}
	current, err := PhysicalPathInfo(owner, relative)
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	if current.Mode()&os.ModeSymlink != 0 || !os.SameFile(expected, current) {
		_ = child.Close()
		return nil, errors.New("owned path changed while opening")
	}
	return child, nil
}
