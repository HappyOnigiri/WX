package main

import "testing"

func classifyJSONL(t *testing.T, data string) testResult {
	t.Helper()
	var result testResult
	if err := parseJSONL([]byte(data), &result); err != nil {
		t.Fatal(err)
	}
	classifyResult(&result)
	return result
}

func TestNamedTestOutputMentioningPanicIsNotAnAnomaly(t *testing.T) {
	result := classifyJSONL(t, `{"Action":"start","Package":"example"}
{"Action":"run","Package":"example","Test":"TestRecovers"}
{"Action":"output","Package":"example","Test":"TestRecovers","Output":"    flaky_test.go:10: recovered: panic: boom\n"}
{"Action":"pass","Package":"example","Test":"TestRecovers"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "" || result.Status != "passed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}

func TestPanicWithoutATestNameIsAnAnomaly(t *testing.T) {
	result := classifyJSONL(t, `{"Action":"start","Package":"example"}
{"Action":"output","Package":"example","Output":"panic: boom\n"}
{"Action":"pass","Package":"example"}
`)
	if result.Anomaly != "panic outside a named test" || result.Status != "failed" {
		t.Fatalf("anomaly=%q status=%q", result.Anomaly, result.Status)
	}
}
