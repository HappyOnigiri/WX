// hookcheck はステージ済み差分からコミット時に必要な軽量検査を選ぶ。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
)

func main() {
	if err := runCLI(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(parent context.Context, args []string, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("hookcheck", flag.ContinueOnError)
	flags.SetOutput(errOut)
	root := flags.String("root", "", "repository root")
	gitDir := flags.String("git-dir", "", "absolute Git directory")
	index := flags.String("index", "", "absolute index path")
	plan := flags.Bool("plan", false, "print the selected checks without executing them")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("hookcheck: unexpected argument %q", flags.Arg(0))
	}
	if *root == "" || *gitDir == "" || *index == "" {
		return errors.New("hookcheck: --root, --git-dir, and --index are required")
	}
	rootPath, err := filepath.Abs(*root)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	gitDirPath, err := absolutePath(rootPath, *gitDir)
	if err != nil {
		return fmt.Errorf("resolve git directory: %w", err)
	}
	indexPath, err := absolutePath(rootPath, *index)
	if err != nil {
		return fmt.Errorf("resolve index: %w", err)
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt)
	defer stop()
	changed, err := stagedFiles(ctx, rootPath, gitDirPath, indexPath)
	if err != nil {
		return err
	}
	selection, err := selectChecks(rootPath, changed)
	if err != nil {
		return err
	}
	selection.note = fmt.Sprintf("selection: staged index %s; commands inspect the working tree (partial staging is not recreated)", indexPath)
	if *plan {
		printPlan(out, selection)
		return nil
	}
	printPlan(out, selection)
	return execute(ctx, rootPath, selection, out)
}

func absolutePath(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	return filepath.Abs(path)
}
