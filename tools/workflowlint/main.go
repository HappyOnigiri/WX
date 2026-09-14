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
	if err := validateTriggerNames(filepath.Dir(*workflow), *workflow); err != nil {
		return err
	}
	return runActionlint(ctx, *actionlint, out)
}

func validateQueue(path string) error {
	root, err := documentRoot(path)
	if err != nil {
		return err
	}
	concurrency := mappingValue(root, "concurrency")
	if concurrency == nil || concurrency.Kind != yaml.MappingNode {
		return errors.New("workflowlint: concurrency mapping is required")
	}
	queue := mappingValue(concurrency, "queue")
	if queue == nil || queue.Kind != yaml.ScalarNode || queue.Value != "max" {
		return errors.New("workflowlint: concurrency.queue must be max")
	}
	return nil
}

// validateTriggerNames は workflow_run の workflows: に並ぶ名前が、
// 同じディレクトリのworkflowのname:として実在することを確かめる。
// GitHubは名前が一致しないtriggerを黙って無視するので、起票経路が静かに止まる。
func validateTriggerNames(dir, path string) error {
	names, err := workflowNames(dir)
	if err != nil {
		return err
	}
	triggers, err := triggerWorkflows(path)
	if err != nil {
		return err
	}
	for _, name := range triggers {
		if !names[name] {
			return fmt.Errorf("workflowlint: %s triggers on workflow %q, which no workflow declares as its name", path, name)
		}
	}
	return nil
}

func workflowNames(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if extension := filepath.Ext(entry.Name()); extension != ".yml" && extension != ".yaml" {
			continue
		}
		root, err := documentRoot(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if name := mappingValue(root, "name"); name != nil && name.Kind == yaml.ScalarNode {
			names[name.Value] = true
		}
	}
	return names, nil
}

func triggerWorkflows(path string) ([]string, error) {
	root, err := documentRoot(path)
	if err != nil {
		return nil, err
	}
	// YAML 1.1の "on" は真偽値として読まれるため、true でも引けるようにする。
	trigger := mappingValue(root, "on")
	if trigger == nil {
		trigger = mappingValue(root, "true")
	}
	if trigger == nil || trigger.Kind != yaml.MappingNode {
		return nil, nil
	}
	workflowRun := mappingValue(trigger, "workflow_run")
	if workflowRun == nil || workflowRun.Kind != yaml.MappingNode {
		return nil, nil
	}
	list := mappingValue(workflowRun, "workflows")
	if list == nil || list.Kind != yaml.SequenceNode {
		return nil, nil
	}
	var names []string
	for _, item := range list.Content {
		if item.Kind == yaml.ScalarNode {
			names = append(names, item.Value)
		}
	}
	return names, nil
}

func documentRoot(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("workflowlint: %s root is not a mapping", path)
	}
	return document.Content[0], nil
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
