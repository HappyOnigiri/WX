package version

import "testing"

func TestStringUsesBuildMetadata(t *testing.T) {
	oldVersion, oldBuildMeta := Version, BuildMeta
	t.Cleanup(func() {
		Version, BuildMeta = oldVersion, oldBuildMeta
	})

	tests := []struct {
		name      string
		version   string
		buildMeta string
		want      string
	}{
		{name: "with metadata", version: "v1.2.3", buildMeta: "dev", want: "v1.2.3-dev"},
		{name: "without metadata", version: "v1.2.3", want: "v1.2.3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			Version, BuildMeta = test.version, test.buildMeta
			if got := String(); got != test.want {
				t.Fatalf("String()=%q, want %q", got, test.want)
			}
		})
	}
}

func TestEmbeddedStringRejectsUnsetVersion(t *testing.T) {
	oldVersion, oldBuildMeta := Version, BuildMeta
	t.Cleanup(func() {
		Version, BuildMeta = oldVersion, oldBuildMeta
	})

	for _, value := range []string{"", "undefined"} {
		Version, BuildMeta = value, "dev"
		if got, ok := EmbeddedString(); ok || got != "" {
			t.Fatalf("EmbeddedString()=(%q, %v) for version %q, want empty and false", got, ok, value)
		}
	}
	Version, BuildMeta = "v1.2.3", "dev"
	if got, ok := EmbeddedString(); !ok || got != "v1.2.3-dev" {
		t.Fatalf("EmbeddedString()=(%q, %v), want (v1.2.3-dev, true)", got, ok)
	}
}
