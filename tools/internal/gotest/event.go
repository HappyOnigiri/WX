// gotest は go test -json の出力解析と、失敗したテストの宣言解決を提供する。
// citest と huntreport が同じ判定でイベントを読むための共有実装であり、tools 配下からのみ import できる。
package gotest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"time"
)

// maxLine は test2json の1行に許す最大バイト数。
// 生成した diff をそのまま出力するテストがあるため、証拠を切り詰めない大きさにしてある。
const maxLine = 16 * 1024 * 1024

// Event は test2json が出力する1行に対応する。
// ImportPath は Go 1.24 以降の build-fail / build-output イベントだけが持つ。
type Event struct {
	Time        string  `json:"Time,omitempty"`
	Action      string  `json:"Action,omitempty"`
	Package     string  `json:"Package,omitempty"`
	Test        string  `json:"Test,omitempty"`
	Elapsed     float64 `json:"Elapsed,omitempty"`
	Output      string  `json:"Output,omitempty"`
	FailedBuild string  `json:"FailedBuild,omitempty"`
	ImportPath  string  `json:"ImportPath,omitempty"`
}

// TestID は1つのテスト（サブテストを含む）を指す。
// Result.Tests のキーに使い、パッケージ名とテスト名を区切り文字で連結しないで済むようにする。
type TestID struct {
	Package string
	Test    string
}

// Result は1回の go test 実行の観測結果をまとめる。
// Anomaly が空でないときは、テスト単位の pass/fail をそのまま信じてはいけない。
type Result struct {
	Package          string
	Events           []Event
	Tests            map[TestID][]Event
	StartedAt        time.Time
	FinishedAt       time.Time
	Status           string
	Exit             int
	Signal           string
	Malformed        bool
	Anomaly          string
	Shuffle          string
	ShuffleByPackage map[string]string
	LogExcerpt       string
}

// ParseJSONL は test2json の JSONL を Result へ取り込む。
// JSON として読めない行は Malformed を立てて読み飛ばすため、job timeout で末尾が切れた
// 出力からも、そこまでに届いたイベントを集計できる。
func ParseJSONL(data []byte, result *Result) error {
	if result.Tests == nil {
		result.Tests = make(map[TestID][]Event)
	}
	if result.ShuffleByPackage == nil {
		result.ShuffleByPackage = make(map[string]string)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			result.Malformed = true
			continue
		}
		result.Events = append(result.Events, event)
		if seed := FindShuffle(event.Output); seed != "" {
			if result.Shuffle == "" {
				result.Shuffle = seed
			}
			if event.Package != "" {
				result.ShuffleByPackage[event.Package] = seed
			}
		}
		if event.Package == "" {
			continue
		}
		if result.Package == "" {
			result.Package = event.Package
		}
		if event.Test != "" {
			id := TestID{Package: event.Package, Test: event.Test}
			result.Tests[id] = append(result.Tests[id], event)
		}
	}
	return scanner.Err()
}

// TextWriter は test2json の行から Output だけを取り出して流す。
// JSON として読めない行はそのまま流し、枠外に出たビルドエラーなどを失わない。
type TextWriter struct {
	out  io.Writer
	line []byte
}

func NewTextWriter(out io.Writer) *TextWriter {
	return &TextWriter{out: out}
}

func (w *TextWriter) Write(data []byte) (int, error) {
	w.line = append(w.line, data...)
	for {
		index := bytes.IndexByte(w.line, '\n')
		if index < 0 {
			break
		}
		w.emit(w.line[:index+1])
		w.line = append(w.line[:0], w.line[index+1:]...)
	}
	// 改行の来ない長大な行でメモリを持ち続けないよう、ParseJSONL と同じ上限で吐き出す。
	if len(w.line) > maxLine {
		w.Flush()
	}
	return len(data), nil
}

// Flush は改行で終わっていない残りを出力する。実行の終了後に必ず呼ぶ。
func (w *TextWriter) Flush() {
	if len(w.line) == 0 {
		return
	}
	w.emit(w.line)
	w.line = w.line[:0]
}

func (w *TextWriter) emit(line []byte) {
	var event Event
	if err := json.Unmarshal(line, &event); err != nil {
		_, _ = w.out.Write(line)
		return
	}
	if event.Output == "" {
		return
	}
	_, _ = io.WriteString(w.out, event.Output)
}
