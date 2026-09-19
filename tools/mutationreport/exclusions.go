package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go/ast"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type exclusion struct {
	Path   string
	Key    string
	Reason string
}

type resolvedExclusion struct {
	ID      string
	Mutator string
}

func resolveExclusions(root string, exclusions []exclusion) (map[string]resolvedExclusion, error) {
	resolved := make(map[string]resolvedExclusion, len(exclusions))
	seenIDs := make(map[string]string, len(exclusions))
	cache := make(map[string]*sourceFile)
	var problems []string
	for _, item := range exclusions {
		source, err := sourceForPath(cache, root, filepath.Join(root, filepath.FromSlash(item.Path)))
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", item.Path, err))
			continue
		}
		matches, err := source.resolveExclusionKey(item.Key)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s %s: %v", item.Path, item.Key, err))
			continue
		}
		if len(matches) != 1 {
			problems = append(problems, fmt.Sprintf("%s %s: matched %d source mutations", item.Path, item.Key, len(matches)))
			continue
		}
		if previous, exists := seenIDs[matches[0].ID]; exists {
			problems = append(problems, fmt.Sprintf("%s %s: duplicates resolved mutation already selected by %s", item.Path, item.Key, previous))
			continue
		}
		seenIDs[matches[0].ID] = item.Key
		resolved[item.Key] = resolvedExclusion{ID: matches[0].ID, Mutator: matches[0].Mutator}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("exclusion resolution failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return resolved, nil
}

func mutatorSetIncludes(set, mutator string) bool {
	switch set {
	case "", "default":
		return true
	case "boundary":
		return mutator == "CONDITIONALS_BOUNDARY" || mutator == "INCREMENT_DECREMENT"
	default:
		return false
	}
}

func (source *sourceFile) resolveExclusionKey(key string) ([]mutationRecord, error) {
	var records []mutationRecord
	var candidateErr error
	ast.Inspect(source.file, func(node ast.Node) bool {
		if node == nil || candidateErr != nil {
			return candidateErr == nil
		}
		position, original, ok := mutationToken(node)
		if !ok {
			return true
		}
		for mutator, replacements := range tokenMutations {
			mutated := replacements[original]
			if mutated == "" {
				continue
			}
			location := source.fileSet.Position(position)
			candidateKey, err := source.exclusionKey(location.Line, location.Column, mutator, original, mutated)
			if err != nil {
				candidateErr = err
				return false
			}
			if candidateKey == key {
				declaration, _ := source.declarationAtLine(location.Line)
				records = append(records, mutationRecord{
					ID:           mutationID(source.repository, declaration.Function, mutator, location.Line, location.Column, original, mutated),
					ExclusionKey: key, Declaration: declaration, Mutator: mutator,
					Line: location.Line, Column: location.Column, Original: original, Mutated: mutated,
				})
			}
		}
		return true
	})
	return records, candidateErr
}

func mutationToken(node ast.Node) (token.Pos, string, bool) {
	switch typed := node.(type) {
	case *ast.AssignStmt:
		return typed.TokPos, typed.Tok.String(), true
	case *ast.BinaryExpr:
		return typed.OpPos, typed.Op.String(), true
	case *ast.BranchStmt:
		return typed.TokPos, typed.Tok.String(), true
	case *ast.IncDecStmt:
		return typed.TokPos, typed.Tok.String(), true
	case *ast.UnaryExpr:
		return typed.OpPos, typed.Op.String(), true
	default:
		return token.NoPos, "", false
	}
}

func (source *sourceFile) exclusionKey(line, column int, mutator, original, mutated string) (string, error) {
	declarationRange, ok := source.declarationRangeAtLine(line)
	if !ok {
		return "", errors.New("no enclosing declaration")
	}
	file := source.fileSet.File(source.file.Pos())
	if file == nil || line < 1 || line > file.LineCount() || column < 1 {
		return "", errors.New("invalid mutation position")
	}
	targetOffset := file.Offset(file.LineStart(line)) + column - 1
	startOffset := file.Offset(declarationRange.start)
	endOffset := file.Offset(declarationRange.end)
	if startOffset < 0 || endOffset > len(source.data) || targetOffset < startOffset || targetOffset >= endOffset {
		return "", errors.New("mutation is outside its declaration")
	}
	segment := source.data[startOffset:endOffset]
	fileSet := token.NewFileSet()
	tokenFile := fileSet.AddFile("declaration.go", -1, len(segment))
	var lexer scanner.Scanner
	lexer.Init(tokenFile, segment, nil, 0)
	var normalized []string
	targetIndex := -1
	for {
		position, kind, literal := lexer.Scan()
		if kind == token.EOF {
			break
		}
		if startOffset+tokenFile.Offset(position) == targetOffset {
			targetIndex = len(normalized)
		}
		value := kind.String()
		if literal != "" {
			value += "\x00" + literal
		}
		normalized = append(normalized, value)
	}
	if targetIndex < 0 {
		return "", errors.New("mutation token was not found in normalized declaration")
	}
	canonical := strings.Join([]string{
		exclusionKeyVersion,
		declarationRange.declaration.Function,
		strings.Join(normalized, "\x1f"),
		strconv.Itoa(targetIndex),
		mutator,
		original,
		mutated,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	return exclusionKeyVersion + ":" + hex.EncodeToString(digest[:]), nil
}

func loadExclusions(path string) ([]exclusion, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var result []exclusion
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(strings.TrimSuffix(scanner.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" || strings.TrimSpace(fields[2]) == "" {
			return nil, fmt.Errorf("%s:%d: every entry needs path, exclusion key, and reason separated by tabs", path, lineNumber)
		}
		relative, err := repositoryPath(fields[0])
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
		key := strings.TrimSpace(fields[1])
		if !validExclusionKey(key) {
			return nil, fmt.Errorf("%s:%d: exclusion key must use ast-v1 followed by 64 lowercase hexadecimal characters", path, lineNumber)
		}
		reason := strings.TrimSpace(fields[2])
		if strings.ContainsAny(reason, "\r\n") || len(reason) > 2000 {
			return nil, fmt.Errorf("%s:%d: reason is invalid", path, lineNumber)
		}
		if seen[key] {
			return nil, fmt.Errorf("%s:%d: duplicate exclusion %s", path, lineNumber, key)
		}
		seen[key] = true
		result = append(result, exclusion{Path: relative, Key: key, Reason: reason})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func validExclusionKey(value string) bool {
	prefix := exclusionKeyVersion + ":"
	return strings.HasPrefix(value, prefix) && validMutationID(strings.TrimPrefix(value, prefix))
}

func exclusionApplies(path, root, packageDir string) bool {
	relativePackage, err := repositoryRelative(root, packageDir)
	if err != nil || relativePackage == "." {
		return err == nil
	}
	return path == relativePackage || strings.HasPrefix(path, relativePackage+"/")
}
