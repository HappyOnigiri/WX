package cli

import (
	"testing"

	"github.com/HappyOnigiri/WX/internal/daemon"
)

func TestMeasurementUnavailableForSetupMutationBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name        string
		measurement string
		want        bool
	}{
		{name: "missing", measurement: "", want: true},
		{name: "pending", measurement: daemon.MeasurementPending, want: true},
		{name: "unsupported", measurement: daemon.MeasurementUnsupported, want: true},
		{name: "measured", measurement: "log2phys_first_last"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := measurementUnavailableForSetup(tt.measurement); got != tt.want {
				t.Fatalf("measurementUnavailableForSetup(%q)=%t, want %t", tt.measurement, got, tt.want)
			}
		})
	}
}
