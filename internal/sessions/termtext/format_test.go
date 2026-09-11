package termtext

import (
	"testing"
	"time"
)

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"future", now.Add(time.Hour), "just now"},
		{"just now", now.Add(-30 * time.Second), "just now"},
		{"minutes", now.Add(-9 * time.Minute), "9m ago"},
		{"minute boundary", now.Add(-59*time.Minute - 59*time.Second), "59m ago"},
		{"hours", now.Add(-7 * time.Hour), "7h ago"},
		{"hour boundary", now.Add(-24*time.Hour + time.Second), "23h ago"},
		{"days", now.Add(-25 * time.Hour), "1d ago"},
		{"many days", now.Add(-40 * 24 * time.Hour), "40d ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelativeTime(tc.at, now); got != tc.want {
				t.Errorf("RelativeTime = %q, want %q", got, tc.want)
			}
		})
	}
}
