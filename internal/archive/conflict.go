package archive

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

const emptyObjectOID = "0000000000000000000000000000000000000000"

// conflictIndexEntry は index に残る stage 付き entry 1 件である。
// path は Git の slash 区切りの repository 相対 path として扱う。
type conflictIndexEntry struct {
	mode  string
	oid   string
	stage int
	path  string
}

func parseIndexEntries(output string) ([]conflictIndexEntry, error) {
	var entries []conflictIndexEntry
	for _, record := range strings.Split(output, "\x00") {
		if record == "" {
			continue
		}
		metadata, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 || name == "" {
			return nil, fmt.Errorf("invalid Git index entry %q", record)
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 0 || stage > 3 {
			return nil, fmt.Errorf("invalid Git index stage in %q", record)
		}
		if !validConflictPath(name) {
			return nil, fmt.Errorf("invalid Git index path %q", name)
		}
		entries = append(entries, conflictIndexEntry{mode: fields[0], oid: fields[1], stage: stage, path: name})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].path != entries[j].path {
			return entries[i].path < entries[j].path
		}
		if entries[i].stage != entries[j].stage {
			return entries[i].stage < entries[j].stage
		}
		if entries[i].mode != entries[j].mode {
			return entries[i].mode < entries[j].mode
		}
		return entries[i].oid < entries[j].oid
	})
	return entries, nil
}

func validConflictPath(name string) bool {
	clean := path.Clean(name)
	return name != "" && name != "." && clean == name && !path.IsAbs(name) && name != ".." && !strings.HasPrefix(name, "../")
}

func unmergedEntries(entries []conflictIndexEntry) []conflictIndexEntry {
	conflicts := make([]conflictIndexEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.stage != 0 {
			conflicts = append(conflicts, entry)
		}
	}
	return conflicts
}

// captureConflictState は stage 情報を artifact へ退避し、未解消 path を除いた index tree を返す。
// live index は読み取り専用で、加工は一時 index に閉じる。
func captureConflictState(value gitValueFunc, run gitRunner, head string) (string, string, error) {
	listing, err := value(nil, "ls-files", "--stage", "-z")
	if err != nil {
		return "", "", fmt.Errorf("inspect unmerged index: %w", err)
	}
	entries, err := parseIndexEntries(listing)
	if err != nil {
		return "", "", err
	}
	conflicts := unmergedEntries(entries)
	if len(conflicts) == 0 {
		return "", "", nil
	}
	root, err := openGitStateRoot(value)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = root.Close() }()
	conflictTree, err := writeConflictTree(root, run, conflicts)
	if err != nil {
		return "", "", err
	}
	commit, err := run(recoveryCommitEnv(nil), []byte("wx conflict state snapshot\n"), "commit-tree", conflictTree, "-p", head)
	if err != nil {
		return "", "", fmt.Errorf("commit conflict state: %w", err)
	}
	indexTree, err := writeConflictFreeIndex(root, run, conflicts)
	if err != nil {
		return "", "", err
	}
	return indexTree, strings.TrimSpace(commit.Stdout), nil
}

func writeConflictTree(root *os.Root, run gitRunner, conflicts []conflictIndexEntry) (string, error) {
	tmp, cleanup, err := temporaryIndex("conflict state", ".wx-conflict-state-index-*")
	if err != nil {
		return "", err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmp}
	if _, err := run(env, nil, "read-tree", "--empty"); err != nil {
		return "", fmt.Errorf("initialize conflict state index: %w", err)
	}
	for _, entry := range conflicts {
		if entry.mode == "0" || entry.oid == emptyObjectOID {
			return "", fmt.Errorf("cannot preserve deleted conflict stage for %s", entry.path)
		}
		storedPath := path.Join("stages", strconv.Itoa(entry.stage), entry.path)
		if _, err := run(env, nil, "update-index", "--add", "--cacheinfo", entry.mode+","+entry.oid+","+storedPath); err != nil {
			return "", fmt.Errorf("stage conflict entry %s: %w", entry.path, err)
		}
	}
	if autoMergeTree := autoMergeTree(root, run); autoMergeTree != "" {
		if _, err := run(env, nil, "read-tree", "--prefix=auto-merge/", autoMergeTree); err != nil {
			return "", fmt.Errorf("stage auto-merge tree: %w", err)
		}
	}
	tree, err := run(env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write conflict state tree: %w", err)
	}
	return strings.TrimSpace(tree.Stdout), nil
}

func autoMergeTree(root *os.Root, run gitRunner) string {
	content, err := root.ReadFile("AUTO_MERGE")
	if err != nil {
		return ""
	}
	oid := strings.TrimSpace(string(content))
	if oid == "" {
		return ""
	}
	if _, err := run(nil, nil, "cat-file", "-e", oid+"^{tree}"); err != nil {
		return ""
	}
	return oid
}

func writeConflictFreeIndex(root *os.Root, run gitRunner, conflicts []conflictIndexEntry) (string, error) {
	contents, err := root.ReadFile("index")
	if err != nil {
		return "", fmt.Errorf("read live index for conflict snapshot: %w", err)
	}
	tmp, cleanup, err := temporaryIndex("conflict index", ".wx-conflict-index-*")
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := os.WriteFile(tmp, contents, 0o600); err != nil {
		return "", fmt.Errorf("copy live index for conflict snapshot: %w", err)
	}
	env := []string{"GIT_INDEX_FILE=" + tmp}
	seen := map[string]bool{}
	var input strings.Builder
	for _, entry := range conflicts {
		if seen[entry.path] {
			continue
		}
		seen[entry.path] = true
		fmt.Fprintf(&input, "0 %s\t%s\n", emptyObjectOID, entry.path)
	}
	if _, err := run(env, []byte(input.String()), "update-index", "--index-info"); err != nil {
		return "", fmt.Errorf("remove unmerged entries from snapshot index: %w", err)
	}
	tree, err := run(env, nil, "write-tree")
	if err != nil {
		return "", fmt.Errorf("write conflict-free index tree: %w", err)
	}
	return strings.TrimSpace(tree.Stdout), nil
}

// restoreConflictIndex は保存した stage を検証済みの target index へ最後に積み直す。
// この間だけ prepare は未解消 path が無い index を観測するが、検証順序を崩さないため許容する。
func restoreConflictIndex(value gitValueFunc, run gitRunner, tree string) error {
	if tree == "" {
		return nil
	}
	listing, err := run(nil, nil, "ls-tree", "-r", "-z", tree)
	if err != nil {
		return fmt.Errorf("list conflict state tree: %w", err)
	}
	entries, err := parseConflictTreeEntries(listing.Stdout)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return errors.New("conflict state tree has no stage entries")
	}
	var input strings.Builder
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.path] {
			continue
		}
		seen[entry.path] = true
		fmt.Fprintf(&input, "0 %s\t%s\n", emptyObjectOID, entry.path)
	}
	for _, entry := range entries {
		fmt.Fprintf(&input, "%s %s %d\t%s\n", entry.mode, entry.oid, entry.stage, entry.path)
	}
	if _, err := run(nil, []byte(input.String()), "update-index", "--index-info"); err != nil {
		return fmt.Errorf("restore unmerged entries: %w", err)
	}
	actualListing, err := value(nil, "ls-files", "--stage", "-z")
	if err != nil {
		return fmt.Errorf("verify restored unmerged index: %w", err)
	}
	actual, err := parseIndexEntries(actualListing)
	if err != nil {
		return err
	}
	actualConflicts := unmergedEntries(actual)
	if !sameConflictEntries(entries, actualConflicts) {
		return errors.New("restored unmerged index does not match snapshot")
	}
	return nil
}

func parseConflictTreeEntries(output string) ([]conflictIndexEntry, error) {
	listing := strings.Split(output, "\x00")
	var entries []conflictIndexEntry
	for _, record := range listing {
		if record == "" {
			continue
		}
		metadata, storedPath, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("invalid conflict state tree record %q", record)
		}
		prefix, original, ok := strings.Cut(storedPath, "/")
		if !ok || prefix != "stages" {
			if storedPath == "auto-merge" || strings.HasPrefix(storedPath, "auto-merge/") {
				continue
			}
			return nil, fmt.Errorf("unexpected conflict state tree path %q", storedPath)
		}
		stageText, original, ok := strings.Cut(original, "/")
		stage, stageErr := strconv.Atoi(stageText)
		if !ok || stageErr != nil || stage < 1 || stage > 3 || !validConflictPath(original) {
			return nil, fmt.Errorf("invalid conflict state path %q", storedPath)
		}
		entry := conflictIndexEntry{mode: fields[0], oid: fields[2], stage: stage, path: original}
		for _, existing := range entries {
			if existing.path == entry.path && existing.stage == entry.stage {
				return nil, fmt.Errorf("duplicate conflict state entry %s stage %d", entry.path, entry.stage)
			}
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].path != entries[j].path {
			return entries[i].path < entries[j].path
		}
		return entries[i].stage < entries[j].stage
	})
	return entries, nil
}

func sameConflictEntries(want, got []conflictIndexEntry) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}
