package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/HappyOnigiri/WX/internal/gitx"
)

type changedFile struct {
	status string
	path   string
}

func stagedFiles(ctx context.Context, root, gitDir, index string) ([]changedFile, error) {
	runner := &gitx.Runner{}
	env := []string{
		"GIT_DIR=" + gitDir,
		"GIT_INDEX_FILE=" + index,
		"GIT_WORK_TREE=" + root,
	}
	result, err := runner.RunEnv(ctx, root, env, "diff", "--cached", "--name-status", "--no-renames", "-z")
	if err != nil {
		var gitErr *gitx.Error
		if errors.As(err, &gitErr) {
			return nil, fmt.Errorf("read staged diff: %w: %s", err, strings.TrimSpace(gitErr.Result.Stderr))
		}
		return nil, fmt.Errorf("read staged diff: %w", err)
	}
	return parseDiff(result.Stdout)
}

func parseDiff(output string) ([]changedFile, error) {
	fields := strings.Split(output, "\x00")
	files := make([]changedFile, 0, len(fields)/2)
	for index := 0; index < len(fields); {
		if fields[index] == "" {
			index++
			continue
		}
		if index+1 >= len(fields) || fields[index+1] == "" {
			return nil, fmt.Errorf("parse staged diff: incomplete NUL record at field %d", index)
		}
		status := fields[index]
		if len(status) != 1 {
			return nil, fmt.Errorf("parse staged diff: unexpected status %q", status)
		}
		files = append(files, changedFile{status: status, path: fields[index+1]})
		index += 2
	}
	return files, nil
}
