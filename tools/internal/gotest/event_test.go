package gotest

import (
	"strings"
	"testing"
)

func parse(t *testing.T, data string) Result {
	t.Helper()
	var result Result
	if err := ParseJSONL([]byte(data), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestParseJSONLRecordsShuffleSeedPerPackage(t *testing.T) {
	result := parse(t, `{"Action":"output","Package":"example/a","Output":"-test.shuffle 123\n"}
{"Action":"output","Package":"example/b","Output":"-test.shuffle 456\n"}
`)
	if result.ShuffleByPackage["example/a"] != "123" || result.ShuffleByPackage["example/b"] != "456" {
		t.Fatalf("shuffle seeds=%v", result.ShuffleByPackage)
	}
	if result.Shuffle != "123" {
		t.Fatalf("shuffle=%q", result.Shuffle)
	}
}

// job timeoutで末尾が切れたJSONLでも、そこまでのイベントは集計できる必要がある。
func TestParseJSONLKeepsEventsFromATruncatedStream(t *testing.T) {
	result := parse(t, `{"Action":"run","Package":"example","Test":"TestA"}
{"Action":"pass","Package":"example","Test":"TestA"}
{"Action":"run","Package":"exam`)
	if !result.Malformed {
		t.Fatal("truncated line was not reported as malformed")
	}
	if got := Counts(result)[TestID{Package: "example", Test: "TestA"}]; got.Pass != 1 {
		t.Fatalf("counts=%+v", got)
	}
}

func TestTextWriterEmitsOutputAndPassesThroughNonJSON(t *testing.T) {
	var buffer strings.Builder
	writer := NewTextWriter(&buffer)
	if _, err := writer.Write([]byte("# example\n{\"Action\":\"output\",\"Output\":\"ok\\n\"}\n{\"Action\":\"run\"}\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("trailing")); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	if got := buffer.String(); got != "# example\nok\ntrailing" {
		t.Fatalf("output=%q", got)
	}
}
