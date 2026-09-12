package config

import "testing"

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
