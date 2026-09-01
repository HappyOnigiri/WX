package coverageconfig

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Exclusion struct {
	Path   string
	Reason string
}

func Load(fileName string) ([]Exclusion, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var exclusions []Exclusion
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		if len(fields) != 2 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("invalid coverage exclusion %q: expected path and reason separated by a tab", line)
		}
		exclusions = append(exclusions, Exclusion{Path: filepath.ToSlash(filepath.Clean(fields[0])), Reason: strings.TrimSpace(fields[1])})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return exclusions, nil
}

func Matches(fileName string, exclusions []Exclusion) bool {
	fileName = filepath.ToSlash(filepath.Clean(fileName))
	for _, exclusion := range exclusions {
		if fileName == exclusion.Path || strings.HasSuffix(fileName, "/"+exclusion.Path) {
			return true
		}
	}
	return false
}
