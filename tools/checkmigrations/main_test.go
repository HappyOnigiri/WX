package main

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"
)

// repository は検査対象の最小構成を組み立てる。store は空文字なら配置しない。
func repository(store string, migrationNames ...string) fstest.MapFS {
	files := fstest.MapFS{}
	if store != "" {
		files[storePath] = &fstest.MapFile{Data: []byte(store)}
	}
	for _, name := range migrationNames {
		files[migrationsDir+"/"+name] = &fstest.MapFile{Data: []byte("CREATE TABLE t(id TEXT);\n")}
	}
	return files
}

// storeSource は SchemaVersion 宣言だけを持つ store.go を作る。値は宣言の右辺へそのまま置く。
func storeSource(value string) string {
	return "package state\n\nconst SchemaVersion = " + value + "\n\nconst JSONSchemaVersion = 13\n"
}

func TestRunAcceptsMatchingVersions(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	files := repository(storeSource("3"), "001_initial.sql", "002_clean.sql", "003_standby.sql")
	if err := run(files, &out); err != nil {
		t.Fatalf("run: %v (output %q)", err, out.String())
	}
	if !strings.Contains(out.String(), "3 migration(s) numbered 001..003 match SchemaVersion = 3") {
		t.Fatalf("output=%q", out.String())
	}
}

func TestRunReportsAStaleSchemaVersion(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	files := repository(storeSource("1"), "001_initial.sql", "002_clean.sql")
	if err := run(files, &out); err == nil {
		t.Fatal("a SchemaVersion below the migration count was accepted")
	}
	for _, want := range []string{"SchemaVersion = 1", "holds 2 migration(s)", "set the constant to 2"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q does not mention %q", out.String(), want)
		}
	}
}

func TestRunReportsBrokenNumbering(t *testing.T) {
	t.Parallel()
	for name, expectation := range map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"gap": {
			files: repository(storeSource("2"), "001_initial.sql", "003_clean.sql"),
			want:  "003 appears where 002 is expected",
		},
		"duplicate": {
			files: repository(storeSource("2"), "001_initial.sql", "001_other.sql"),
			want:  "already used by",
		},
		"does not start at 001": {
			files: repository(storeSource("1"), "002_initial.sql"),
			want:  "002 appears where 001 is expected",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := run(expectation.files, &out); err == nil {
				t.Fatalf("%s accepted", name)
			}
			if !strings.Contains(out.String(), expectation.want) {
				t.Fatalf("output %q does not mention %q", out.String(), expectation.want)
			}
		})
	}
}

func TestRunReportsUnusableMigrationNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"1_initial.sql", "0001_initial.sql", "001-initial.sql", "001_Initial.sql", "initial.sql"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			if err := run(repository(storeSource("1"), name), &out); err == nil {
				t.Fatalf("%s accepted", name)
			}
			if !strings.Contains(out.String(), "NNN_lower_snake_case.sql") {
				t.Fatalf("output %q does not say what the name must look like", out.String())
			}
		})
	}
}

func TestRunRejectsUnreadableInputs(t *testing.T) {
	t.Parallel()
	for name, expectation := range map[string]struct {
		files fstest.MapFS
		want  string
	}{
		"no migration":         {files: repository(storeSource("0")), want: "no *.sql migration found"},
		"missing store":        {files: repository("", "001_initial.sql"), want: storePath},
		"unparsable store":     {files: repository("package state\n\nconst SchemaVersion =\n", "001_initial.sql"), want: "parse " + storePath},
		"constant absent":      {files: repository("package state\n\nconst JSONSchemaVersion = 13\n", "001_initial.sql"), want: "no const SchemaVersion declaration found"},
		"constant expression":  {files: repository(storeSource("len(names)"), "001_initial.sql"), want: "cannot interpret an expression"},
		"constant not integer": {files: repository(storeSource(`"5"`), "001_initial.sql"), want: "cannot interpret an expression"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := run(expectation.files, &out)
			if err == nil {
				t.Fatalf("%s accepted", name)
			}
			if !strings.Contains(err.Error(), expectation.want) {
				t.Fatalf("error %q does not mention %q", err, expectation.want)
			}
		})
	}
}

// 検査は宣言をまとめた const group からも読めなければならない。store.go の形が変わっても契約は同じである。
func TestSchemaVersionReadsAGroupedDeclaration(t *testing.T) {
	t.Parallel()
	files := repository("package state\n\nconst (\n\tother = 1\n\tSchemaVersion = 7\n)\n", "001_initial.sql")
	version, err := schemaVersion(files)
	if err != nil {
		t.Fatalf("schemaVersion: %v", err)
	}
	if version != 7 {
		t.Fatalf("version=%d", version)
	}
}
