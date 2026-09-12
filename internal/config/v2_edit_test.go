package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestV2EditTracksLeafPresenceForSourceDisplay(t *testing.T) {
	raw := Config{}
	if err := SetV2Field(&raw, V2ScopeRepositoryDefaults, "", "", "default_branch", "trunk"); err != nil {
		t.Fatal(err)
	}
	if !raw.present["repository_defaults.default_branch"] {
		t.Fatalf("presence=%v", raw.present)
	}
	effective := Merge(Defaults(), raw)
	fields := V2Fields(effective, raw, V2ScopeRepositoryDefaults, "", "")
	for _, field := range fields {
		if field.Key == "default_branch" && field.Source != "global" {
			t.Fatalf("default_branch source=%q, want global", field.Source)
		}
	}
	if err := ResetV2Field(&raw, V2ScopeRepositoryDefaults, "", "", "default_branch"); err != nil {
		t.Fatal(err)
	}
	if raw.present["repository_defaults.default_branch"] {
		t.Fatalf("reset presence=%v", raw.present)
	}
}

func TestV2ResetLastFieldKeepsAnExplicitSchemaSection(t *testing.T) {
	var raw Config
	if err := SetV2Field(&raw, V2ScopeSystem, "", "", "pool.preparation_concurrency", "3"); err != nil {
		t.Fatal(err)
	}
	if err := ResetV2Field(&raw, V2ScopeSystem, "", "", "pool.preparation_concurrency"); err != nil {
		t.Fatal(err)
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "version: 2") || !strings.Contains(string(data), "system: {}") {
		t.Fatalf("reset left an invalid sparse document: %s", data)
	}
}
