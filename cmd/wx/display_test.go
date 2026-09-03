package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintDisplayRendersNestedPayloadsWithoutGoFormatting pins the shapes a
// real `wx status` / `wx doctor` run produced. Before this renderer the human
// output carried Go's own map, slice and nil formatting, and whole numbers
// were printed in exponent form.
func TestPrintDisplayRendersNestedPayloadsWithoutGoFormatting(t *testing.T) {
	payload := map[string]any{
		"active_sessions":   float64(0),
		"job_details":       map[string]any{"failed": float64(0), "pending": float64(2), "running": float64(1)},
		"quarantine":        nil,
		"retention_seconds": map[string]any{"expired_session_tombstone": float64(31536000)},
		"checks": map[string]any{
			"config": "ok",
			"artifact_ownership": map[string]any{
				"errors":        []any{},
				"unknown_paths": []any{"/a", "/b"},
			},
			"worktree_registration": map[string]any{
				"invalid": []any{map[string]any{"path": "/x", "reason": "missing"}},
			},
		},
		"degraded": true,
	}

	var buf bytes.Buffer
	printDisplay(&buf, payload)
	got := buf.String()

	for _, forbidden := range []string{"map[", "<nil>", "e+07", "[]interface", "0x"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("output leaks Go formatting %q:\n%s", forbidden, got)
		}
	}

	for _, want := range []string{
		"active_sessions",
		"checks.artifact_ownership.errors",
		"checks.artifact_ownership.unknown_paths",
		"checks.config",
		"checks.worktree_registration.invalid[0].path",
		"checks.worktree_registration.invalid[0].reason",
		"degraded",
		"job_details.failed",
		"job_details.pending",
		"quarantine",
		"retention_seconds.expired_session_tombstone",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output is missing key %q:\n%s", want, got)
		}
	}

	values := map[string]string{
		"checks.artifact_ownership.errors":        "(none)",
		"checks.artifact_ownership.unknown_paths": "/a, /b",
		"quarantine": "(none)",
		"retention_seconds.expired_session_tombstone":    "31536000",
		"job_details.pending":                            "2",
		"degraded":                                       "true",
		"checks.worktree_registration.invalid[0].reason": "missing",
	}
	lines := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		key, value, found := strings.Cut(line, " ")
		if !found {
			t.Fatalf("line has no value separator: %q", line)
		}
		lines[key] = strings.TrimSpace(value)
	}
	for key, want := range values {
		if lines[key] != want {
			t.Fatalf("key %q rendered as %q, want %q\n%s", key, lines[key], want, got)
		}
	}

	// Keys must be sorted so repeated runs produce identical output.
	var keys []string
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		key, _, _ := strings.Cut(line, " ")
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] > keys[i] {
			t.Fatalf("keys are not sorted: %q before %q", keys[i-1], keys[i])
		}
	}
}

// TestPrintDisplayHandlesEmptyAndNonObjectPayloads covers the degenerate
// shapes the daemon can return when it is degraded or has nothing to report.
func TestPrintDisplayHandlesEmptyAndNonObjectPayloads(t *testing.T) {
	var empty bytes.Buffer
	printDisplay(&empty, map[string]any{})
	if got := empty.String(); !strings.Contains(got, "(none)") {
		t.Fatalf("empty payload rendered as %q", got)
	}

	pairs := appendDisplayPairs(nil, "", "bare")
	if len(pairs) != 1 || pairs[0].key != "value" || pairs[0].value != "bare" {
		t.Fatalf("non-object payload rendered as %+v", pairs)
	}
}
