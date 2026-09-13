package i18n

import (
	"strings"
	"testing"
)

// help 本文は catalog へ合流し、ID の重複は起動前に気付ける。
func TestHelpCatalogJoinsTheCatalog(t *testing.T) {
	catalog := Catalog()
	for id, entry := range helpCatalog {
		joined, known := catalog[id]
		if !known {
			t.Fatalf("help message %q is missing from the catalog", id)
		}
		if joined != entry {
			t.Fatalf("help message %q differs from the catalog entry", id)
		}
		if !strings.HasPrefix(id, "help.") {
			t.Fatalf("help message %q does not use the help. prefix", id)
		}
	}
}

// 説明の折り返しは訳文が持つ改行だけで表し、桁合わせの空白を訳文へ埋め込まない。
func TestHelpCatalogHasNoAlignmentPadding(t *testing.T) {
	for id, entry := range helpCatalog {
		for _, text := range []string{entry.EN, entry.JA} {
			for line := range strings.SplitSeq(text, "\n") {
				if strings.HasPrefix(line, " ") || strings.Contains(line, "  ") {
					t.Fatalf("help message %q carries alignment padding: %q", id, line)
				}
			}
		}
	}
}
