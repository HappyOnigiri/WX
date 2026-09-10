// workflowlint は固定版actionlintと、queue: maxの互換診断を組み合わせる。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const queueWorkflow = ".github/workflows/report-flaky-tests.yml"

var (
	queueDiagnostic  = regexp.MustCompile(`^(?:\./)?` + regexp.QuoteMeta(queueWorkflow) + `:(\d+):\d+: .*unexpected key ["']queue["']`)
	diagnosticHeader = regexp.MustCompile(`^[^\s|].*:\d+:\d+: `)
)

func main() {
	if err := commandMain(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func commandMain(ctx context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("workflowlint", flag.ContinueOnError)
	flags.SetOutput(errOut)
	actionlint := flags.String("actionlint", "actionlint", "actionlint executable")
	workflow := flags.String("workflow", queueWorkflow, "workflow containing the compatibility key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("workflowlint: unexpected argument %q", flags.Arg(0))
	}
	if err := validateQueue(*workflow); err != nil {
		return err
	}
	return runActionlint(ctx, *actionlint, out)
}

func validateQueue(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	root := document.Content
	if len(root) == 0 || root[0].Kind != yaml.MappingNode {
		return errors.New("workflowlint: workflow root is not a mapping")
	}
	concurrency := mappingValue(root[0], "concurrency")
	if concurrency == nil || concurrency.Kind != yaml.MappingNode {
		return errors.New("workflowlint: concurrency mapping is required")
	}
	queue := mappingValue(concurrency, "queue")
	if queue == nil || queue.Kind != yaml.ScalarNode || queue.Value != "max" {
		return errors.New("workflowlint: concurrency.queue must be max")
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func runActionlint(ctx context.Context, actionlint string, output io.Writer) error {
	command := exec.CommandContext(ctx, actionlint)
	data, err := command.CombinedOutput()
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	allowed := 0
	var unexpected []string
	skipContext := false
	for _, line := range lines {
		if line == "" {
			skipContext = false
			continue
		}
		if allowedQueueDiagnostic(line) {
			allowed++
			skipContext = true
			continue
		}
		if skipContext && !diagnosticHeader.MatchString(line) {
			continue
		}
		skipContext = false
		unexpected = append(unexpected, line)
	}
	if len(unexpected) > 0 {
		for _, line := range unexpected {
			_, _ = fmt.Fprintln(output, line)
		}
		if err != nil {
			return fmt.Errorf("actionlint failed: %w", err)
		}
		return errors.New("actionlint reported unexpected diagnostics")
	}
	if err != nil && allowed == 0 {
		return fmt.Errorf("actionlint failed: %w", err)
	}
	return nil
}

func allowedQueueDiagnostic(line string) bool {
	match := queueDiagnostic.FindStringSubmatch(line)
	if len(match) != 2 {
		return false
	}
	lineNumber, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	return lineNumber == queueLine(queueWorkflow)
}

func queueLine(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		root, rootErr := os.Getwd()
		if rootErr != nil {
			return -1
		}
		for {
			candidate := filepath.Join(root, path)
			data, err = os.ReadFile(candidate)
			if err == nil {
				break
			}
			parent := filepath.Dir(root)
			if parent == root {
				return -1
			}
			root = parent
		}
	}
	for lineNumber, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "queue: max" {
			return lineNumber + 1
		}
	}
	return -1
}
