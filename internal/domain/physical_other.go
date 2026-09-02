//go:build !darwin && !linux

package domain

import "os"

func ensurePhysicalDirectoryRootPlatform(absolute string, perm os.FileMode) (*os.Root, error) {
	return ensurePhysicalDirectoryRootLegacy(absolute, perm)
}
