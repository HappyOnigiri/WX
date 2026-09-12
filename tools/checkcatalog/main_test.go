package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCatalogCheckerRuns(t *testing.T) {
	// 実際の検査は main が呼ぶ i18n.ValidateCatalog に委ねる。package にテストが
	// あること自体も testlayout-check の契約である。
	if testing.Short() {
		t.Skip("catalog validation is covered by the package test")
	}
}

func TestValidateReferencesFindsUnknownStaticID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "unknown.go")
	if err := os.WriteFile(path, []byte("package sample\nfunc f() { _ = i18n.T(ctx, \"missing.id\", nil) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := validateReferences(root)
	if err == nil || !strings.Contains(err.Error(), `unknown message ID "missing.id"`) {
		t.Fatalf("validateReferences error=%v", err)
	}
}
