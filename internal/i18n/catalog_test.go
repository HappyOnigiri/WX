package i18n

import "testing"

func TestCatalogHasBothLanguagesAndValidTemplates(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatal(err)
	}
	if len(Catalog()) == 0 {
		t.Fatal("catalog is empty")
	}
}

func TestLanguageParsingFallsBackOnlyForDisplayNormalization(t *testing.T) {
	for _, test := range []struct {
		value string
		want  Language
	}{
		{"", English}, {"en", English}, {"ja", Japanese}, {"JA", Japanese},
	} {
		got, err := Parse(test.value)
		if err != nil || got != test.want {
			t.Fatalf("Parse(%q)=(%q,%v), want %q", test.value, got, err, test.want)
		}
	}
	if _, err := Parse("fr"); err == nil {
		t.Fatal("unsupported language accepted")
	}
	if got := Normalize("fr"); got != English {
		t.Fatalf("Normalize unsupported=%q, want en", got)
	}
}
