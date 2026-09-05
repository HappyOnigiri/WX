package termtext

import "testing"

func TestStripANSI(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string
	}{
		{"no escape", "plain text", "plain text"},
		{"SGR", "\x1b[2mCompacted\x1b[22m", "Compacted"},
		{"erase line keeps no leftover", "\x1b[2K\rtransforming...", "\rtransforming..."},
		{"OSC", "\x1b]0;title\x07body", "body"},
		{"markers are kept", "\x04a\x01hit\x02", "\x04a\x01hit\x02"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripANSI(tc.s); got != tc.want {
				t.Errorf("StripANSI(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		s    string
		keep string
		want string
	}{
		{"body keeps newline tab and role marker", "\x04u\nA\tB\r\nC", "\n\t\x04", "\x04u\nA\tB\nC"},
		{"snippet keeps highlight markers", "a\x01b\x02c\x03d\re", "\n\t\x01\x02\x03\x04", "a\x01b\x02c\x03de"},
		{"line drops newline", "title\nrest\r", KeepLine, "titlerest"},
		{"escape sequence removed with its parameters", "vite \x1b[2K\rbuild", KeepLine, "vite build"},
		{"DEL removed", "a\x7fb", "\n\t\x04", "ab"},
		{"multibyte is preserved", "日本語\x1b[0m", KeepLine, "日本語"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Sanitize(tc.s, tc.keep); got != tc.want {
				t.Errorf("Sanitize(%q) = %q, want %q", tc.s, got, tc.want)
			}
		})
	}
}
