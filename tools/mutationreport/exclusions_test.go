package main

import (
	"strings"
	"testing"
)

func TestValidExclusionKey(t *testing.T) {
	valid := exclusionKeyVersion + ":" + strings.Repeat("a", 64)
	if !validExclusionKey(valid) {
		t.Fatalf("valid exclusion key was rejected: %s", valid)
	}
	for _, value := range []string{strings.Repeat("a", 64), exclusionKeyVersion + ":" + strings.Repeat("A", 64)} {
		if validExclusionKey(value) {
			t.Fatalf("invalid exclusion key was accepted: %s", value)
		}
	}
}
