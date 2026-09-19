package diag

import "testing"

func TestSetupMeasurementCheckNameIsStable(t *testing.T) {
	if CheckSetupMeasurement != "setup_measurement" {
		t.Fatalf("check=%q", CheckSetupMeasurement)
	}
}
