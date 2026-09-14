package main

import "testing"

// manifestの版と種別はreporting側の検証と対になるので、変更を目に見えるようにする。
func TestManifestContractConstants(t *testing.T) {
	if huntSchemaVersion != 1 {
		t.Fatalf("schema version=%d; update report-flaky-tests.cjs before changing it", huntSchemaVersion)
	}
	if huntKind != "flake-hunt" {
		t.Fatalf("kind=%q; update report-flaky-tests.cjs before changing it", huntKind)
	}
}
