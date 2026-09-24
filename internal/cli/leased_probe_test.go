package cli

import (
	"testing"

	"github.com/HappyOnigiri/WorktreeX/internal/diag"
)

func TestSetupMeasurementFindingIsUnchecked(t *testing.T) {
	finding := setupMeasurementFinding("/slot", "missing")
	if finding.Check != diag.CheckSetupMeasurement || finding.Severity != diag.SeverityUnchecked || finding.Target != "/slot" {
		t.Fatalf("finding=%+v", finding)
	}
}
