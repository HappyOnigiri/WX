package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/HappyOnigiri/WorktreeX/internal/gitx"
)

// SubmoduleSharingStatus は local module を clone するときの object 状態である。
// 欠落 object は通常の local module でも起こるが、共有不可の診断対象は promisor の場合だけに限る。
type SubmoduleSharingStatus string

const (
	SubmoduleSharingAvailable       SubmoduleSharingStatus = "available"
	SubmoduleSharingShallow         SubmoduleSharingStatus = "shallow"
	SubmoduleSharingPromisor        SubmoduleSharingStatus = "promisor"
	SubmoduleSharingPromisorMissing SubmoduleSharingStatus = "promisor_missing"
	SubmoduleSharingObjectMissing   SubmoduleSharingStatus = "object_missing"
)

// SubmoduleInspection は source module を clone する前に読める状態をまとめる。
// Origin は準備後に戻す upstream で、空なら source module の origin が未設定である。
type SubmoduleInspection struct {
	OriginURL       string
	Shallow         bool
	Promisor        bool
	ObjectAvailable bool
}

// Status は準備と doctor が共有する submodule の判定を返す。
func (i SubmoduleInspection) Status() SubmoduleSharingStatus {
	if i.Promisor && !i.ObjectAvailable {
		return SubmoduleSharingPromisorMissing
	}
	if !i.ObjectAvailable {
		return SubmoduleSharingObjectMissing
	}
	if i.Shallow {
		return SubmoduleSharingShallow
	}
	if i.Promisor {
		return SubmoduleSharingPromisor
	}
	return SubmoduleSharingAvailable
}

// InspectSubmodule は module の shallow/promisor 状態と要求 OID の完全性を読む。
// promisor の object 検査には --missing=print を使い、lazy fetch を発生させない。
// shallow の有無はファイルだけで判定し、通常 module の object 検査は cat-file 1 回に留める。
func InspectSubmodule(ctx context.Context, git *gitx.Runner, source, oid string) (SubmoduleInspection, error) {
	var inspection SubmoduleInspection
	info, err := os.Stat(source)
	if err != nil {
		return inspection, fmt.Errorf("stat submodule module %s: %w", source, err)
	}
	if !info.IsDir() {
		return inspection, fmt.Errorf("submodule module %s is not a directory", source)
	}
	shallow, err := os.Stat(filepath.Join(source, "shallow"))
	switch {
	case err == nil && !shallow.IsDir():
		inspection.Shallow = true
	case err == nil:
		return inspection, fmt.Errorf("submodule shallow marker %s is a directory", filepath.Join(source, "shallow"))
	case !errors.Is(err, os.ErrNotExist):
		return inspection, fmt.Errorf("stat submodule shallow marker: %w", err)
	}

	configResult, configErr := git.Run(ctx, source, "--git-dir=.", "config", "--get-regexp", `^(remote\.origin\.url|remote\..*\.promisor|extensions\.partialclone)$`)
	if configErr != nil && !isEmptySubmoduleConfig(ctx, configErr) {
		return inspection, configErr
	}
	inspection.OriginURL, inspection.Promisor = parseSubmoduleSourceConfig(configResult.Stdout)

	if inspection.Promisor {
		result, runErr := git.Run(ctx, source, "--git-dir=.", "rev-list", "--objects", "--no-object-names", "--no-walk", "--missing=print", oid)
		if runErr != nil {
			if err := submoduleInspectionExecutionError(ctx, runErr); err != nil {
				return inspection, err
			}
			inspection.ObjectAvailable = false
		} else {
			inspection.ObjectAvailable = !submoduleObjectsMissing(result.Stdout)
		}
		return inspection, nil
	}

	_, objectErr := git.Run(ctx, source, "--git-dir=.", "cat-file", "-e", oid+"^{commit}")
	if objectErr != nil {
		if err := submoduleInspectionExecutionError(ctx, objectErr); err != nil {
			return inspection, err
		}
		inspection.ObjectAvailable = false
		return inspection, nil
	}
	inspection.ObjectAvailable = true
	return inspection, nil
}

func parseSubmoduleSourceConfig(output string) (string, bool) {
	var origin string
	promisor := false
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		key, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case key == "remote.origin.url":
			origin = value
		case strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".promisor"):
			promisor = promisor || strings.EqualFold(value, "true")
		case key == "extensions.partialclone":
			promisor = promisor || value != ""
		}
	}
	return origin, promisor
}

func submoduleObjectsMissing(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "?") {
			return true
		}
	}
	return false
}

func submoduleInspectionExecutionError(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var gitErr *gitx.Error
	if !errors.As(err, &gitErr) || gitErr.Result.ExitCode < 0 {
		return err
	}
	return nil
}
