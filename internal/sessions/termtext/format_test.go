package termtext

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		value int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1 KiB"},
		{1536, "1.5 KiB"},
		{254464, "248.5 KiB"},
		{1024 * 1024, "1 MiB"},
		{1468006, "1.4 MiB"},
		{1024 * 1024 * 1024 * 1024 * 1024, "1 PiB"},
		{-2048, "-2 KiB"},
	}
	for _, tc := range tests {
		if got := HumanBytes(tc.value); got != tc.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 2, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		at   time.Time
		want string
	}{
		{"future", now.Add(time.Hour), "たった今"},
		{"just now", now.Add(-30 * time.Second), "たった今"},
		{"minutes", now.Add(-9 * time.Minute), "9分前"},
		{"minute boundary", now.Add(-59*time.Minute - 59*time.Second), "59分前"},
		{"hours", now.Add(-7 * time.Hour), "7時間前"},
		{"hour boundary", now.Add(-24*time.Hour + time.Second), "23時間前"},
		{"days", now.Add(-25 * time.Hour), "1日前"},
		{"many days", now.Add(-40 * 24 * time.Hour), "40日前"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelativeTime(tc.at, now); got != tc.want {
				t.Errorf("RelativeTime = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHomePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tests := []struct {
		name, path, want string
	}{
		{"empty", "", ""},
		{"home itself", home, "~"},
		{"under home", filepath.Join(home, "wx", "repo"), filepath.Join("~", "wx", "repo")},
		{"outside home", "/opt/other", "/opt/other"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := HomePath(tc.path); got != tc.want {
				t.Errorf("HomePath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
