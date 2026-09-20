package cli

import (
	"reflect"
	"testing"
)

func TestMessageMutationBoundariesKeepOddAndMixedPairs(t *testing.T) {
	for _, tt := range []struct {
		name  string
		pairs []any
		want  map[string]any
	}{
		{name: "no pairs", want: nil},
		{name: "odd tail", pairs: []any{"JobID", "job-1", "Dropped"}, want: map[string]any{"JobID": "job-1"}},
		{name: "non string key", pairs: []any{1, "ignored", "JobID", "job-2"}, want: map[string]any{"JobID": "job-2"}},
		{name: "two fields", pairs: []any{"JobID", "job-1", "State", "DONE"}, want: map[string]any{"JobID": "job-1", "State": "DONE"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := message("diag.detail.submodules_checked", tt.pairs...)
			if !reflect.DeepEqual(got.Data, tt.want) {
				t.Fatalf("message data=%v, want %v", got.Data, tt.want)
			}
		})
	}
}
