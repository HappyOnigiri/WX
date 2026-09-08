package config

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReadinessDefaultsAndValidation(t *testing.T) {
	for _, mode := range []string{"early", "full", "invalid", ""} {
		t.Run("mode="+mode, func(t *testing.T) {
			cfg := Defaults()
			cfg.Readiness.Mode = mode
			err := Validate(&cfg)
			if (err == nil) != (mode == "early" || mode == "full") {
				t.Fatalf("mode %q: %v", mode, err)
			}
		})
	}
	var raw Config
	if err := yaml.Unmarshal([]byte("readiness:\n  early_paths: []\n"), &raw); err != nil {
		t.Fatal(err)
	}
	cfg := Merge(Defaults(), raw)
	if cfg.Readiness.Mode != "early" {
		t.Fatalf("default mode=%q", cfg.Readiness.Mode)
	}
	cfg.Readiness.EarlyPaths = []string{"./config/rules.md", "config/rules.md", "tools/../config"}
	if err := Validate(&cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Readiness.EarlyPaths, []string{"config/rules.md", "config"}) {
		t.Fatalf("paths=%v", cfg.Readiness.EarlyPaths)
	}
	for _, path := range []string{"", ".", "tools/..", "../outside", "/absolute", ".git", ".git/config", "nested/.GIT", "bad\x00path"} {
		cfg.Readiness.EarlyPaths = []string{path}
		if err := Validate(&cfg); err == nil {
			t.Errorf("accepted unsafe path %q", path)
		}
	}
}
