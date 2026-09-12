package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// scopeSamples は各 scope キーの妥当な値である。
// 表に無いキーがあるとテストが落ちるので、キーを足したら往復と検証の確認も必ず足すことになる。
var scopeSamples = map[Scope]map[string]string{
	ScopeWorkspace: {
		"worktree":                 "hot",
		"copy":                     ".env",
		"link":                     "cache",
		"reuse_standby":            "false",
		"submodules":               "false",
		"warm_count":               "2",
		"agent.add_dir":            "off",
		"retention.hot_standby":    "30m0s",
		"retention.ended_worktree": "2h0m0s",
		"discovery.max_depth":      "3",
		"discovery.exclude":        "build",
	},
	ScopeRepository: {
		"default_branch":               "develop",
		"dir_name":                     "app",
		"dir_source":                   "directory",
		"cow_min_size_kib":             "64",
		"prepare.command":              "make",
		"prepare.timeout":              "5m0s",
		"prepare.version":              "v1",
		"includes.default_agent_rules": "false",
		"readiness.mode":               "full",
		"readiness.early_paths":        "docs",
		"readiness.timeout":            "20m0s",
		"storage.copy_mode":            "copy",
	},
}

// scopeInvalid は Validate が拒否しなければならない値である。
// 検証を持たないキーは scopeUnvalidated へ入れる。どちらにも無いキーはテストが落ちる。
var scopeInvalid = map[Scope]map[string]string{
	ScopeWorkspace: {
		"worktree":                 "bogus",
		"warm_count":               "-1",
		"agent.add_dir":            "bogus",
		"retention.hot_standby":    "-1s",
		"retention.ended_worktree": "-1s",
		"discovery.max_depth":      "0",
	},
	ScopeRepository: {
		"dir_source":        "bogus",
		"cow_min_size_kib":  "-1",
		"prepare.timeout":   "-1s",
		"readiness.mode":    "bogus",
		"readiness.timeout": "0s",
		"storage.copy_mode": "bogus",
	},
}

// scopeUnvalidated は値域を持たず、Validate が通してよいキーである。
var scopeUnvalidated = map[Scope][]string{
	ScopeWorkspace:  {"copy", "link", "reuse_standby", "submodules", "discovery.exclude"},
	ScopeRepository: {"default_branch", "dir_name", "prepare.command", "prepare.version", "includes.default_agent_rules", "readiness.early_paths"},
}

// scopeOnlyKeys は継承元の global キーを持たない、scope 固有のキーである。
var scopeOnlyKeys = map[string]bool{
	"copy": true, "link": true, "dir_name": true, "default_branch": true,
	"prepare.command": true, "prepare.timeout": true, "prepare.version": true,
}

func scopeTestHome(t *testing.T) string {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	return home
}

func scopeTestTarget(t *testing.T, home string) string {
	t.Helper()
	target := filepath.Join(home, "repo")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	return target
}

// setScopeSample はキーの種別に応じて scalar と list の設定経路を振り分ける。
func setScopeSample(t *testing.T, raw *Config, s Scope, target, key, value string) {
	t.Helper()
	var err error
	if IsScopeListKey(s, key) {
		err = AppendScopeList(raw, s, target, key, value)
	} else {
		err = SetScopeField(raw, s, target, key, value)
	}
	if err != nil {
		t.Fatalf("set %s %s: %v", s, key, err)
	}
}

func scopeEntry(c Config, s Scope, target string) reflect.Value {
	entry := s.newEntry()
	overrides := scopeMap(&c, s)
	if !overrides.IsNil() {
		if existing := overrides.MapIndex(reflect.ValueOf(target)); existing.IsValid() {
			entry.Set(existing)
		}
	}
	return entry
}

// すべての scope キーが設定ファイルへ往復し、1つの --reset が兄弟の個別指定を道連れにしない。
// 項目ごとの削除条件を手書きしていた頃の事故を、キー表の全数で機械的に検出する。
func TestScopeFieldsRoundTripAndResetKeepsSiblings(t *testing.T) {
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		t.Run(scope.String(), func(t *testing.T) {
			home := scopeTestHome(t)
			target := scopeTestTarget(t, home)
			keys := ScopeKeys(scope)
			samples := scopeSamples[scope]
			if len(keys) != len(samples) {
				t.Fatalf("keys=%v samples=%d, want a sample value for every key", keys, len(samples))
			}
			var raw Config
			for _, key := range keys {
				sample, ok := samples[key]
				if !ok {
					t.Fatalf("no sample value for %s key %s", scope, key)
				}
				setScopeSample(t, &raw, scope, target, key, sample)
			}
			if err := Save(raw); err != nil {
				t.Fatal(err)
			}
			loaded, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			entry := scopeEntry(loaded, scope, target)
			for _, key := range keys {
				if field := scopeFieldByKey(entry, key); field.IsZero() {
					t.Fatalf("%s %s was not persisted", scope, key)
				}
			}
			for _, key := range keys {
				raw, err := LoadRaw()
				if err != nil {
					t.Fatal(err)
				}
				if IsScopeListKey(scope, key) {
					err = ResetScopeList(&raw, scope, target, key)
				} else {
					err = ResetScopeField(&raw, scope, target, key)
				}
				if err != nil {
					t.Fatalf("reset %s: %v", key, err)
				}
				merged := Merge(Defaults(), raw)
				if err := NormalizePaths(&merged); err != nil {
					t.Fatal(err)
				}
				entry := scopeEntry(merged, scope, target)
				if field := scopeFieldByKey(entry, key); !field.IsZero() {
					t.Fatalf("%s %s survived its own reset", scope, key)
				}
				for _, sibling := range keys {
					if sibling == key {
						continue
					}
					if field := scopeFieldByKey(entry, sibling); field.IsZero() {
						t.Fatalf("resetting %s also dropped %s", key, sibling)
					}
				}
			}
		})
	}
}

// 最後の個別指定を解除すると map の項目ごと消え、疎な YAML に戻る。
func TestScopeResetDropsTheEntryOnceEmpty(t *testing.T) {
	home := scopeTestHome(t)
	target := scopeTestTarget(t, home)
	var raw Config
	if err := SetScopeField(&raw, ScopeRepository, target, "readiness.mode", "full"); err != nil {
		t.Fatal(err)
	}
	if err := ResetScopeField(&raw, ScopeRepository, target, "readiness.mode"); err != nil {
		t.Fatal(err)
	}
	if len(raw.Repositories) != 0 {
		t.Fatalf("repositories=%+v, want the entry dropped", raw.Repositories)
	}
	if err := Save(raw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(mustConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "repositories") {
		t.Fatalf("config=%q, want no repositories section", data)
	}
}

func mustConfigPath(t *testing.T) string {
	t.Helper()
	path, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// すべての scope キーは継承元の global キーを持つか、scope 固有として明示されている。
// 別名表への追記漏れは、CLI の表示と list の種取りを黙って狂わせる。
func TestScopeKeysResolveTheirGlobalCounterpart(t *testing.T) {
	t.Parallel()
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		for _, key := range ScopeKeys(scope) {
			_, ok := globalKeyForScopeKey(key)
			if ok == scopeOnlyKeys[key] {
				t.Fatalf("%s key %s: global counterpart=%v, scope-only=%v", scope, key, ok, scopeOnlyKeys[key])
			}
		}
	}
}

// 値域のあるキーはすべて Validate が拒否し、値域の無いキーは明示されている。
func TestScopeValidationRejectsOutOfRangeValues(t *testing.T) {
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		t.Run(scope.String(), func(t *testing.T) {
			home := scopeTestHome(t)
			target := scopeTestTarget(t, home)
			for _, key := range ScopeKeys(scope) {
				invalid, ok := scopeInvalid[scope][key]
				if !ok {
					if !slices.Contains(scopeUnvalidated[scope], key) {
						t.Fatalf("%s key %s has neither an invalid sample nor an explicit exemption", scope, key)
					}
					continue
				}
				var raw Config
				if err := SetScopeField(&raw, scope, target, key, invalid); err != nil {
					t.Fatalf("set %s: %v", key, err)
				}
				effective := Merge(Defaults(), raw)
				if err := NormalizePaths(&effective); err != nil {
					t.Fatal(err)
				}
				if err := Validate(&effective); err == nil {
					t.Fatalf("%s %s=%q was accepted", scope, key, invalid)
				}
			}
		})
	}
}

// list の個別指定は global の置き換えで、最初の --add で global の現在値が焼き付く。
func TestScopeListReplacesTheGlobalValue(t *testing.T) {
	home := scopeTestHome(t)
	target := scopeTestTarget(t, home)
	var raw Config
	if err := AppendList(&raw, "discovery.exclude", "global-only"); err != nil {
		t.Fatal(err)
	}
	if err := AppendScopeList(&raw, ScopeWorkspace, target, "discovery.exclude", "build"); err != nil {
		t.Fatal(err)
	}
	effective := Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	values, overridden := effective.DiscoveryExcludeForWorkspace(target)
	if !overridden || !slices.Contains(values, "global-only") || !slices.Contains(values, "build") {
		t.Fatalf("exclude=%v overridden=%v, want the global list seeded and extended", values, overridden)
	}
	// 焼き付いた後の global 側の追加はこの workspace へ伝わらない。
	if err := AppendList(&raw, "discovery.exclude", "later"); err != nil {
		t.Fatal(err)
	}
	effective = Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	if values, _ := effective.DiscoveryExcludeForWorkspace(target); slices.Contains(values, "later") {
		t.Fatalf("exclude=%v, want the later global addition kept out", values)
	}
	if err := ResetScopeList(&raw, ScopeWorkspace, target, "discovery.exclude"); err != nil {
		t.Fatal(err)
	}
	effective = Merge(Defaults(), raw)
	if err := NormalizePaths(&effective); err != nil {
		t.Fatal(err)
	}
	values, overridden = effective.DiscoveryExcludeForWorkspace(target)
	if overridden || !slices.Contains(values, "later") {
		t.Fatalf("exclude=%v overridden=%v, want the global list restored", values, overridden)
	}
}

// 未設定の list からの --remove は、global の実効値を黙って削らないよう拒否する。
func TestScopeListRemoveRequiresAnExplicitList(t *testing.T) {
	home := scopeTestHome(t)
	target := scopeTestTarget(t, home)
	var raw Config
	err := RemoveScopeList(&raw, ScopeWorkspace, target, "discovery.exclude", "vendor")
	if err == nil || !strings.Contains(err.Error(), "--add") {
		t.Fatalf("err=%v, want guidance to use --add first", err)
	}
}

// repository の early_paths も global と同じ安全条件で検査し、clean と重複除去を済ませて保存する。
func TestRepositoryEarlyPathsAreValidatedAndNormalized(t *testing.T) {
	home := scopeTestHome(t)
	target := scopeTestTarget(t, home)
	for _, unsafe := range []string{"/abs", "../escape", ".git/config", ".."} {
		cfg := Defaults()
		cfg.Repositories[target] = Repository{Readiness: RepositoryReadiness{EarlyPaths: []string{unsafe}}}
		if err := Validate(&cfg); err == nil {
			t.Fatalf("early path %q was accepted", unsafe)
		}
	}
	cfg := Defaults()
	cfg.Repositories[target] = Repository{Readiness: RepositoryReadiness{EarlyPaths: []string{"docs/", "docs", "./src"}}}
	if err := Validate(&cfg); err != nil {
		t.Fatal(err)
	}
	got := cfg.ReadinessForRepository(target).EarlyPaths
	if len(got) != 2 || got[0] != "docs" || got[1] != "src" {
		t.Fatalf("early paths=%v, want cleaned and deduplicated values", got)
	}
}

// 個別指定の無い repository は global の readiness をそのまま受け取る。
func TestReadinessForRepositoryInheritsGlobalValues(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Readiness.EarlyPaths = []string{"boot"}
	timeout := Duration{}
	if err := parseInto(reflect.ValueOf(&timeout).Elem(), "1m"); err != nil {
		t.Fatal(err)
	}
	cfg.Repositories["/slow"] = Repository{Readiness: RepositoryReadiness{Mode: "full", Timeout: &timeout}}
	if got := cfg.ReadinessForRepository("/other"); got.Mode != cfg.Readiness.Mode || got.Timeout != cfg.Readiness.Timeout || len(got.EarlyPaths) != 1 {
		t.Fatalf("readiness=%+v, want the global values", got)
	}
	slow := cfg.ReadinessForRepository("/slow")
	if slow.Mode != "full" || slow.Timeout.Duration != timeout.Duration || len(slow.EarlyPaths) != 1 {
		t.Fatalf("readiness=%+v, want the repository overrides with the global early paths", slow)
	}
	if cfg.MaxReadinessTimeout() != cfg.Readiness.Timeout.Duration {
		t.Fatalf("max timeout=%s, want the longer global value", cfg.MaxReadinessTimeout())
	}
}

// ScopeFields は個別指定と継承元を区別して出す。
func TestScopeFieldsReportTheSource(t *testing.T) {
	t.Parallel()
	cfg := Defaults()
	cfg.Workspaces["/repo"] = Workspace{Agent: WorkspaceAgent{AddDir: AgentAddDirOff}}
	sources := map[string]ScopeField{}
	for _, field := range ScopeFields(cfg, ScopeWorkspace, "/repo") {
		sources[field.Key] = field
	}
	if got := sources["agent.add_dir"]; got.Value != AgentAddDirOff || got.Source != "workspace" {
		t.Fatalf("agent.add_dir=%+v, want the workspace override", got)
	}
	if got := sources["warm_count"]; got.Value != "1" || got.Source != "global" {
		t.Fatalf("warm_count=%+v, want the inherited global value", got)
	}
	if got := sources["copy"]; got.Source != "unset" {
		t.Fatalf("copy=%+v, want no inherited value", got)
	}
	if got := sources["discovery.exclude"]; got.Source != "global" || !strings.Contains(got.Value, "node_modules") {
		t.Fatalf("discovery.exclude=%+v, want the inherited global list", got)
	}
}

func TestResolvedFieldsDistinguishExplicitGlobalAndDefault(t *testing.T) {
	t.Parallel()
	raw := Config{}
	if err := SetField(&raw, "pool.warm_per_workspace", "1"); err != nil {
		t.Fatal(err)
	}
	effective := Merge(Defaults(), raw)
	global := map[string]ScopeField{}
	for _, field := range GlobalFields(effective, raw) {
		global[field.Key] = field
	}
	if got := global["pool.warm_per_workspace"]; got.Value != "1" || got.Source != "explicit" {
		t.Fatalf("explicit global=%+v", got)
	}
	if got := global["storage.cow_min_size_kib"]; got.Value != "16" || got.Source != "default" {
		t.Fatalf("default global=%+v", got)
	}

	effective.Workspaces["/repo"] = Workspace{}
	resolved := map[string]ScopeField{}
	for _, field := range ResolvedScopeFields(effective, raw, ScopeWorkspace, "/repo") {
		resolved[field.Key] = field
	}
	if got := resolved["warm_count"]; got.Value != "1" || got.Source != "global" {
		t.Fatalf("global inheritance=%+v", got)
	}
	if got := resolved["reuse_standby"]; got.Value != "true" || got.Source != "default" {
		t.Fatalf("default inheritance=%+v", got)
	}
}
