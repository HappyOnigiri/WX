package config

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/i18n"
)

func TestCatalogCoversEveryConfigKey(t *testing.T) {
	catalog := Catalog()
	if len(catalog) == 0 {
		t.Fatal("Catalog returned no entries")
	}
	seen := map[string]Metadata{}
	for _, meta := range catalog {
		if meta.Key == "" || meta.Kind == "" || meta.DisplayName == "" || meta.Group == "" || meta.Description == "" || meta.Impact == "" || len(meta.Scopes) == 0 {
			t.Errorf("incomplete metadata: %+v", meta)
		}
		if _, duplicate := seen[meta.Key]; duplicate {
			t.Errorf("duplicate metadata for %q", meta.Key)
		}
		seen[meta.Key] = meta
	}
	for _, field := range append(Fields(Defaults()), Lists(Defaults())...) {
		if _, ok := seen[field.Key]; !ok {
			t.Errorf("global key %q has no metadata", field.Key)
		}
	}
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		for _, key := range ScopeKeys(scope) {
			meta, ok := seen[key]
			if !ok || !contains(meta.Scopes, scope.String()) {
				t.Errorf("%s key %q has no compatible metadata", scope, key)
			}
		}
	}
}

func TestCatalogDerivesKindsAndChoices(t *testing.T) {
	mode, err := Describe("readiness.mode", "repository")
	if err != nil {
		t.Fatal(err)
	}
	if mode.Kind != KindString || len(mode.Choices) != 2 {
		t.Fatalf("readiness.mode metadata=%+v", mode)
	}
	paths, err := Describe("readiness.early_paths", "global")
	if err != nil {
		t.Fatal(err)
	}
	if paths.Kind != KindList {
		t.Fatalf("readiness.early_paths kind=%q", paths.Kind)
	}
}

// TestCatalogTextsAreLocalized は、設定項目の表示名・説明・影響が両言語のカタログにあることを守る。
// 表示側は ID をキーから組み立てて引き、無ければ英語の原文へ落とすため、
// この検査が無いと新しいキーだけ日本語設定でも英語のまま残る。
func TestCatalogTextsAreLocalized(t *testing.T) {
	messages := i18n.Catalog()
	for _, meta := range Catalog() {
		for _, kind := range []string{"name", "description", "impact"} {
			id := "config." + meta.Key + "." + kind
			if _, known := messages[id]; !known {
				t.Errorf("message %q is missing; the %s of %q would stay English", id, kind, meta.Key)
			}
		}
	}
}
