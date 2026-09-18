package config

import "testing"

// legacy の section は対応する Config field が必ず存在し、非ゼロ値だけを
// present へ記録する。tag の追加や対応漏れで v2 の判定を誤らせない。
func TestMarkLegacyPresentFromValueRecognizesEveryLegacySection(t *testing.T) {
	raw := Defaults()
	raw.Resume.AutoFresh = true
	present := map[string]bool{}

	markLegacyPresentFromValue(raw, present)

	for _, tag := range []string{
		"worktree", "storage", "pool", "retention", "discovery", "readiness", "resume",
		"lease", "includes", "agent", "sessions", "logging", "update", "daemon", "repositories",
	} {
		if !present[tag] {
			t.Errorf("legacy section %q was not recorded: %v", tag, present)
		}
	}
}

func TestNormalizeRepositoryRelativeRejectsNULAtEitherBoundary(t *testing.T) {
	for _, test := range []struct {
		name, value string
	}{
		{name: "prefix", value: "\x00repository"},
		{name: "suffix", value: "repository\x00"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizeRepositoryRelative(test.value); err == nil {
				t.Fatalf("NUL-containing path %q was accepted", test.value)
			}
		})
	}
}

func TestValidateRepositoryDefaultsAcceptsInclusiveCOWMinSizeBounds(t *testing.T) {
	for _, test := range []struct {
		name     string
		boundary int
	}{
		{name: "zero", boundary: 0},
		{name: "maximum", boundary: MaxCOWMinSizeKiB},
	} {
		t.Run(test.name, func(t *testing.T) {
			boundary := test.boundary
			if _, err := validateRepositoryDefaults("repository_defaults", RepositoryDefaults{
				COWMinSizeKiB: &boundary,
			}); err != nil {
				t.Fatalf("COW minimum %d was rejected: %v", boundary, err)
			}
		})
	}
}

func TestValidateRepositoryDefaultsRejectsZeroReadinessTimeout(t *testing.T) {
	timeout := Duration{}
	_, err := validateRepositoryDefaults("repository_defaults", RepositoryDefaults{
		Readiness: RepositoryReadiness{Timeout: &timeout},
	})
	if err == nil {
		t.Fatal("zero readiness timeout was accepted")
	}
}
