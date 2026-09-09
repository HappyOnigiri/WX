package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

func printPlan(out io.Writer, plan selection) {
	_, _ = fmt.Fprintln(out, "hook plan:")
	if plan.note != "" {
		_, _ = fmt.Fprintln(out, plan.note)
	}
	if plan.empty() {
		for _, reason := range plan.skipped {
			_, _ = fmt.Fprintf(out, "- no checks: %s\n", reason)
		}
		return
	}
	for _, check := range plan.sortedChecks() {
		_, _ = fmt.Fprintf(out, "- %s\n", check.name)
		printReasons(out, check.reasons)
		_, _ = fmt.Fprintf(out, "  command: %s\n", formatCommand([]string{"make", check.name}))
	}
	if plan.compile {
		_, _ = fmt.Fprintln(out, "- compile-all")
		printReasons(out, plan.compileBy)
		_, _ = fmt.Fprintf(out, "  command: %s\n", formatCommand([]string{"go", "test", "-run", "^$", "./..."}))
	}
	for _, test := range plan.sortedTests() {
		args := []string{"go", "test", "-short"}
		if test.countOne {
			args = append(args, "-count=1")
		}
		args = append(args, test.packages...)
		_, _ = fmt.Fprintf(out, "- package-test %s\n", strings.Join(test.packages, ", "))
		printReasons(out, test.reasons)
		_, _ = fmt.Fprintf(out, "  command: %s\n", formatCommand(args))
	}
	for _, reason := range plan.skipped {
		_, _ = fmt.Fprintf(out, "- no checks: %s\n", reason)
	}
}

func printReasons(out io.Writer, reasons []string) {
	for _, reason := range reasons {
		_, _ = fmt.Fprintf(out, "  reason: %s\n", reason)
	}
}

func formatCommand(args []string) string {
	formatted := make([]string, len(args))
	for index, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\n'\"") {
			formatted[index] = strconv.Quote(arg)
		} else {
			formatted[index] = arg
		}
	}
	return strings.Join(formatted, " ")
}
