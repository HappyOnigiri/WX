package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// displayKeyWidth is the minimum column width for the key of a rendered line.
// Nested keys are longer than the flat ones the first version of this output
// had, so the real width is computed per payload and this only sets a floor.
const displayKeyWidth = 20

// printDisplay renders an RPC display payload (wx status, wx doctor) for a
// human reader. The values arrive as JSON, so nested maps, arrays, nulls and
// float64 numbers are all possible; printing them with %v leaks Go's own
// formatting into user-facing output. A real daemon run produced lines like
//
//	job_details          map[failed:0 pending:0 running:0]
//	quarantine           <nil>
//	retention_seconds    map[... expired_session_tombstone:3.1536e+07 ...]
//
// and wx doctor collapsed every check into a single unreadable line. Nested
// values are flattened onto dotted keys instead, so each fact gets its own
// line. The --json output is the stability contract for scripts and is
// rendered separately from this function.
func printDisplay(w io.Writer, payload map[string]any) {
	pairs := appendDisplayPairs(nil, "", payload)
	width := displayKeyWidth
	for _, pair := range pairs {
		if len(pair.key) > width {
			width = len(pair.key)
		}
	}
	for _, pair := range pairs {
		// Display output goes to stdout; a write failure there is not
		// actionable and must not change the command's exit status.
		_, _ = fmt.Fprintf(w, "%-*s %s\n", width, pair.key, pair.value)
	}
}

type displayPair struct{ key, value string }

// appendDisplayPairs walks value depth-first with map keys sorted, so the
// output order is stable across runs regardless of Go's map iteration order.
func appendDisplayPairs(pairs []displayPair, prefix string, value any) []displayPair {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			return append(pairs, displayPair{displayKey(prefix), "(none)"})
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := key
			if prefix != "" {
				child = prefix + "." + key
			}
			pairs = appendDisplayPairs(pairs, child, typed[key])
		}
		return pairs
	case []any:
		if len(typed) == 0 {
			return append(pairs, displayPair{displayKey(prefix), "(none)"})
		}
		if scalars, ok := scalarElements(typed); ok {
			return append(pairs, displayPair{displayKey(prefix), strings.Join(scalars, ", ")})
		}
		for i, element := range typed {
			pairs = appendDisplayPairs(pairs, fmt.Sprintf("%s[%d]", prefix, i), element)
		}
		return pairs
	case nil:
		return append(pairs, displayPair{displayKey(prefix), "(none)"})
	default:
		return append(pairs, displayPair{displayKey(prefix), displayScalar(value)})
	}
}

// scalarElements reports whether every element is a scalar, so a list of
// strings or numbers stays on one line instead of being split across indexed
// keys. A list holding a map or a nested list is rendered element by element.
func scalarElements(elements []any) ([]string, bool) {
	out := make([]string, 0, len(elements))
	for _, element := range elements {
		switch element.(type) {
		case map[string]any, []any, nil:
			return nil, false
		}
		out = append(out, displayScalar(element))
	}
	return out, true
}

// displayScalar formats one JSON scalar. Numbers arrive as float64, so whole
// numbers must be printed without an exponent: retention seconds showed up as
// 3.1536e+07 before this.
func displayScalar(value any) string {
	if number, ok := value.(float64); ok {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

// displayKey guards against an empty key, which happens only if a payload
// itself is not an object. Callers pass a top-level map, so this is a
// fallback rather than an expected shape.
func displayKey(prefix string) string {
	if prefix == "" {
		return "value"
	}
	return prefix
}
