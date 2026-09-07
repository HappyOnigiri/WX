// checkmigrations はmigrationファイルの連番とstate.SchemaVersionの一致を検査する。
// 適用版はSQL名の辞書順の位置で決まり、定数は手動で揃えるため、
// 番号の欠番・重複や定数の据え置きは実行時まで表面化しない。この検査でmake ciから落とす。
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
)

const (
	migrationsDir     = "migrations"
	storePath         = "internal/state/store.go"
	schemaVersionName = "SchemaVersion"
)

// migrationName は`internal/state`の適用順が辞書順であるため、番号を固定幅にし語を小文字に限る。
var migrationName = regexp.MustCompile(`^([0-9]{3})_[a-z0-9_]+\.sql$`)

func main() {
	if err := run(os.DirFS("."), os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(root fs.FS, out io.Writer) error {
	names, err := migrationFiles(root)
	if err != nil {
		return err
	}
	problems, highest := checkSequence(names)
	version, err := schemaVersion(root)
	if err != nil {
		return err
	}
	if len(problems) == 0 && version != highest {
		problems = append(problems, fmt.Sprintf(
			"%s declares %s = %d, but %s/ holds %d migration(s); set the constant to %d so PRAGMA user_version matches the applied files",
			storePath, schemaVersionName, version, migrationsDir, highest, highest))
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(out, problem)
		}
		return fmt.Errorf("checkmigrations: %d problem(s); migration files and %s disagree", len(problems), schemaVersionName)
	}
	_, _ = fmt.Fprintf(out, "checkmigrations: %d migration(s) numbered 001..%03d match %s = %d\n", len(names), highest, schemaVersionName, version)
	return nil
}

// migrationFiles はmigrationディレクトリ直下のSQL名を辞書順で返す。
// 1つも無い状態はSchemaVersion=0を正当化しかねないため失敗させる。
func migrationFiles(root fs.FS) ([]string, error) {
	names, err := fs.Glob(root, migrationsDir+"/*.sql")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s/: no *.sql migration found; the schema cannot be created without one", migrationsDir)
	}
	sort.Strings(names)
	return names, nil
}

// checkSequence は名前の形式と001からの連番を検査し、期待される最終版を返す。
// 形式不正がある場合の最終版はファイル数とし、番号の解釈に依存させない。
func checkSequence(names []string) (problems []string, highest int) {
	seen := map[int]string{}
	var numbers []int
	for _, name := range names {
		match := migrationName.FindStringSubmatch(path.Base(name))
		if match == nil {
			problems = append(problems, fmt.Sprintf(
				"%s: name must be NNN_lower_snake_case.sql (three digits, then lowercase letters, digits, and underscores); the applied version is the position in this sorted list",
				name))
			continue
		}
		number, err := strconv.Atoi(match[1])
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: cannot read the version number: %v", name, err))
			continue
		}
		if earlier, duplicated := seen[number]; duplicated {
			problems = append(problems, fmt.Sprintf(
				"%s: version %03d is already used by %s; two files with one number shift every later version by one",
				name, number, earlier))
			continue
		}
		seen[number] = name
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	for position, number := range numbers {
		if expected := position + 1; number != expected {
			problems = append(problems, fmt.Sprintf(
				"%s: version %03d appears where %03d is expected; migrations must run from 001 with no gap because the applied version is the position in the sorted list",
				seen[number], number, expected))
		}
	}
	if len(problems) > 0 {
		return problems, len(names)
	}
	return nil, len(numbers)
}

// schemaVersion は宣言された整数リテラルだけを契約として読む。
// 式へ変えるとこの検査が解釈できないため、その旨を示して失敗させる。
func schemaVersion(root fs.FS) (int, error) {
	source, err := fs.ReadFile(root, storePath)
	if err != nil {
		return 0, err
	}
	file, err := parser.ParseFile(token.NewFileSet(), storePath, source, 0)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", storePath, err)
	}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, specification := range group.Specs {
			value, found := constantValue(specification)
			if !found {
				continue
			}
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.INT {
				return 0, fmt.Errorf("%s: %s must stay an integer literal; checkmigrations cannot interpret an expression", storePath, schemaVersionName)
			}
			number, err := strconv.Atoi(literal.Value)
			if err != nil {
				return 0, fmt.Errorf("%s: %s = %s is not a plain decimal integer: %w", storePath, schemaVersionName, literal.Value, err)
			}
			return number, nil
		}
	}
	return 0, fmt.Errorf("%s: no const %s declaration found", storePath, schemaVersionName)
}

// constantValue はSchemaVersionの宣言だけを取り出す。名前と値が1対1でない宣言は解釈しない。
func constantValue(specification ast.Spec) (ast.Expr, bool) {
	spec, ok := specification.(*ast.ValueSpec)
	if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 || spec.Names[0].Name != schemaVersionName {
		return nil, false
	}
	return spec.Values[0], true
}
