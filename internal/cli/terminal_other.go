//go:build !darwin && !linux

package cli

func isTerminal(int) bool { return false }
