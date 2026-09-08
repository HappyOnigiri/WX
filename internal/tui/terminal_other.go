//go:build !darwin && !linux

package tui

func IsTerminal(int) bool { return false }
