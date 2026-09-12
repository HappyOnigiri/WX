// testshard は go test -list の結果を安定した名前単位のbucketへ分ける。
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"go/token"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const listCommandName = "go test -list"

func main() {
	os.Exit(commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func commandMain(ctx context.Context, args []string, output, errorOutput io.Writer) int {
	flags := flag.NewFlagSet("testshard", flag.ContinueOnError)
	flags.SetOutput(errorOutput)
	goCommand := flags.String("go", "go", "Go executable used to list tests")
	packageName := flags.String("package", "", "package passed to go test")
	bucketCount := flags.Int("count", 0, "number of buckets")
	bucketIndex := flags.Int("index", -1, "zero-based bucket index")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(errorOutput, "testshard: unexpected positional arguments")
		return 2
	}
	if err := validateConfig(*goCommand, *packageName, *bucketCount, *bucketIndex); err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 2
	}
	names, err := listNames(ctx, *goCommand, *packageName)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 1
	}
	selected, err := selectBucket(names, *bucketCount, *bucketIndex)
	if err != nil {
		_, _ = fmt.Fprintf(errorOutput, "testshard: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(output, runPattern(selected))
	return 0
}

func validateConfig(goCommand, packageName string, count, index int) error {
	if strings.TrimSpace(goCommand) == "" || strings.IndexFunc(goCommand, unicode.IsControl) >= 0 {
		return errors.New("Go executable is required and must not contain control characters")
	}
	if strings.TrimSpace(packageName) == "" || strings.IndexFunc(packageName, unicode.IsControl) >= 0 {
		return errors.New("package is required and must not contain control characters")
	}
	return validateBuckets(count, index)
}

func validateBuckets(count, index int) error {
	if count <= 0 {
		return fmt.Errorf("bucket count must be positive, got %d", count)
	}
	if index < 0 || index >= count {
		return fmt.Errorf("bucket index %d is outside 0..%d", index, count-1)
	}
	return nil
}

func listNames(ctx context.Context, goCommand, packageName string) ([]string, error) {
	var stdout, stderr bytes.Buffer
	command := exec.CommandContext(ctx, goCommand, "test", "-list", ".", packageName)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s failed: %w: %s", listCommandName, err, detail)
		}
		return nil, fmt.Errorf("%s failed: %w", listCommandName, err)
	}
	return parseListOutput(stdout.String())
}

func parseListOutput(output string) ([]string, error) {
	seen := make(map[string]bool)
	var names []string
	for lineNumber, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if packageResultLine(line) {
			continue
		}
		if benchmarkName(line) {
			continue
		}
		if !testName(line) {
			return nil, fmt.Errorf("go test -list output line %d is not a top-level Test, Fuzz, or Example name: %q", lineNumber+1, line)
		}
		if seen[line] {
			return nil, fmt.Errorf("go test -list output contains duplicate test name %q", line)
		}
		seen[line] = true
		names = append(names, line)
	}
	if len(names) == 0 {
		return nil, errors.New("go test -list returned no top-level Test, Fuzz, or Example names")
	}
	sort.Strings(names)
	return names, nil
}

func benchmarkName(name string) bool {
	return testPrefixName(name, "Benchmark")
}

func packageResultLine(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return false
	}
	switch fields[0] {
	case "ok", "?", "FAIL":
		return true
	default:
		return false
	}
}

func testName(name string) bool {
	return testPrefixName(name, "Test") || testPrefixName(name, "Fuzz") || testPrefixName(name, "Example")
}

func testPrefixName(name, prefix string) bool {
	if !token.IsIdentifier(name) || !strings.HasPrefix(name, prefix) {
		return false
	}
	suffix := name[len(prefix):]
	if suffix == "" {
		return true
	}
	runeValue, _ := utf8.DecodeRuneInString(suffix)
	return !unicode.IsLower(runeValue)
}

func selectBucket(names []string, count, index int) ([]string, error) {
	if err := validateBuckets(count, index); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("test name list is empty")
	}
	seen := make(map[string]bool, len(names))
	selected := make([]string, 0, (len(names)+count-1)/count)
	for _, name := range names {
		if !testName(name) {
			return nil, fmt.Errorf("invalid top-level test name %q", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate test name %q", name)
		}
		seen[name] = true
		if bucketForName(name, count) == index {
			selected = append(selected, name)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("bucket %d of %d has no tests", index, count)
	}
	sort.Strings(selected)
	return selected, nil
}

// bucketForName はSHA-256の先頭8byteを名前のUTF-8列へ適用し、countで剰余を取る。
// ハッシュの入力と読み出し順を固定し、CPUアーキテクチャによる所属差を避ける。
func bucketForName(name string, count int) int {
	if count <= 0 {
		return -1
	}
	digest := sha256.Sum256([]byte(name))
	value := binary.BigEndian.Uint64(digest[:8])
	return int(value % uint64(count))
}

func runPattern(names []string) string {
	parts := make([]string, len(names))
	for index, name := range names {
		parts[index] = regexp.QuoteMeta(name)
	}
	return "^(" + strings.Join(parts, "|") + ")$"
}
