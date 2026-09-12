package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ValueKind は設定エディターが値ごとの入力方法を選ぶための分類である。
type ValueKind string

const (
	KindString   ValueKind = "string"
	KindInteger  ValueKind = "integer"
	KindBoolean  ValueKind = "boolean"
	KindDuration ValueKind = "duration"
	KindList     ValueKind = "list"
)

// Metadata は CLI と TUI が共有する設定項目の説明と編集制約である。
// Key と Kind は設定構造体から導出し、手書きの説明側には重複させない。
type Metadata struct {
	Key         string
	Kind        ValueKind
	Scopes      []string
	DisplayName string
	Group       string
	Description string
	Impact      string
	Choices     []string
}

type catalogText struct {
	name, description, impact string
	choices                   []string
}

var catalogTexts = map[string]catalogText{
	"worktree.undefined":                  {"未設定時の方針", "方針が未登録の workspace で worktree を使う方法です。", "次回の起動方法が変わります。", []string{"ask", "hot", "cold", "off"}},
	"worktree.reuse_standby":              {"待機枠の再利用", "古い READY worktree を要求 OID へ更新して再利用します。", "無効にすると完全一致しない貸出は cold start になります。", nil},
	"worktree.submodules":                 {"submodule の準備", "worktree 内の submodule をローカル clone で準備します。", "変更後は以前の待機枠を再利用しません。", nil},
	"storage.worktree_root":               {"worktree 保存先", "新しい root 世代を作る worktree の保存先です。", "既存 slot は登録済み root で寿命を全うします。", nil},
	"storage.copy_mode":                   {"コピー方式", "checkout 済みファイルを APFS CoW で共有する方針です。", "変更後は対象 repository の待機枠を作り直します。", []string{"auto", "cow", "copy"}},
	"storage.cow_min_size_kib":            {"CoW の最小サイズ", "CoW 共有の対象にするファイルサイズの下限です。", "値を下げるほど共有対象と判定コストが増えます。", nil},
	"storage.repo_dir_source":             {"repository 名の由来", "slot 内の repository directory 名を決める情報源です。", "新しく準備する workspace の配置名に影響します。", []string{"remote", "directory"}},
	"storage.backup_generations":          {"DB バックアップ世代数", "state database のバックアップを残す世代数です。", "保持する管理データ量に影響します。", nil},
	"storage.backup_retention":            {"DB バックアップ保持期間", "state database のバックアップを保持する期間です。", "短くすると古いバックアップが早く回収されます。", nil},
	"pool.warm_per_workspace":             {"待機枠数", "hot workspace ごとに維持する READY slot 数です。", "0 にすると自動補充を無効にします。", nil},
	"pool.preparation_concurrency":        {"準備の同時実行数", "利用者向けの準備・復元・保存を同時に実行する上限です。", "増やすと CPU とディスク負荷が上がります。", nil},
	"retention.hot_standby":               {"待機枠の保持期間", "利用されていない READY slot を hot として保つ期間です。", "0 にすると自動補充を無効にします。", nil},
	"retention.ended_worktree":            {"終了 worktree の保持期間", "終了した session の worktree を再開用に残す期間です。", "短くすると復元前に実体が回収されやすくなります。", nil},
	"retention.quarantined":               {"隔離 slot の保持期間", "隔離された slot を診断用に残す期間です。", "短くすると障害調査に使える実体が早く消えます。", nil},
	"retention.recovery_snapshot":         {"復旧 snapshot の保持期間", "session の復旧 snapshot を保持する期間です。", "短くすると古い session を元の作業状態へ戻せなくなります。", nil},
	"retention.expired_session_tombstone": {"終了 session 記録の保持期間", "期限切れ session の識別情報を残す期間です。", "重複や古い参照の診断期間に影響します。", nil},
	"retention.failed_job":                {"失敗 job の保持期間", "失敗した daemon job の記録を残す期間です。", "障害履歴を確認できる期間に影響します。", nil},
	"retention.event_log":                 {"イベントログ保持期間", "daemon のイベント記録を残す期間です。", "診断に利用できる履歴の長さに影響します。", nil},
	"discovery.max_depth":                 {"探索の深さ", "workspace 内で repository を探索する深さの上限です。", "大きくすると探索範囲と所要時間が増えます。", nil},
	"discovery.max_entries":               {"探索件数の上限", "workspace 探索で確認する entry 数の上限です。", "大きくすると大規模 directory を発見しやすくなります。", nil},
	"discovery.timeout":                   {"探索 timeout", "repository と workspace の探索を待つ上限時間です。", "短すぎると大規模 workspace の解決が失敗します。", nil},
	"discovery.reconcile_interval":        {"再照合間隔", "daemon が workspace と slot を再照合する間隔です。", "短くすると変化の検出と保守処理が増えます。", nil},
	"discovery.exclude":                   {"探索除外名", "repository 探索から除外する directory 名です。", "除外を減らすと探索範囲が増えます。", nil},
	"readiness.mode":                      {"準備完了モード", "agent を起動できる準備段階を選びます。", "full は全準備を待ち、early は hook で残りを保護します。", []string{"early", "full"}},
	"readiness.early_paths":               {"先行配置 path", "early 起動までに配置する追加 path です。", "静的な起動設定を早期に利用できます。", nil},
	"readiness.timeout":                   {"準備 timeout", "worktree の準備完了を待つ上限時間です。", "短すぎると正常な準備も中断されます。", nil},
	"readiness.progress":                  {"準備進捗の表示", "端末で準備待ちの進捗を stderr に表示します。", "表示だけが変わり、準備処理には影響しません。", nil},
	"resume.auto_fresh":                   {"自動 fresh 再開", "元の worktree を復元できないとき新しい worktree で再開します。", "有効にすると確認を省いて会話の再開を優先します。", nil},
	"lease.ttl":                           {"path 貸出の期限", "wx new などの path 貸出を保存へ進めるまでの期間です。", "期限後の編集は snapshot に含まれません。", nil},
	"lease.shell":                         {"起動 shell", "wx shell が起動する shell の path です。", "空なら環境の既定 shell を使います。", nil},
	"includes.default_agent_rules":        {"標準 agent 資産", "標準の agent 指示ファイルを worktree へ含めます。", "無効にすると明示した include だけを配置します。", nil},
	"agent.add_dir":                       {"agent の追加 directory", "複数 repository を agent の追加作業 directory として渡す方針です。", "agent が読み込む repository と資産に影響します。", []string{"always", "worktree", "off"}},
	"logging.level":                       {"ログレベル", "daemon log に記録する詳細度です。", "詳細な値ほどログ量が増えます。", []string{"debug", "info", "warn", "error"}},
	"sessions.paths.claude.sessions":      {"Claude 履歴 path", "Claude の会話履歴を検索する directory です。", "再開候補として見つかる会話が変わります。", nil},
	"sessions.paths.codex.sessions":       {"Codex 履歴 path", "Codex の会話履歴を検索する directory です。", "再開候補として見つかる会話が変わります。", nil},
	"worktree":                            {"workspace の worktree 方針", "対象 workspace で worktree を使う方法です。", "次回の起動方法が変わります。", []string{"hot", "cold", "off"}},
	"copy":                                {"workspace のコピー path", "非 Git workspace root から slot へコピーする path です。", "準備内容と fingerprint が変わります。", nil},
	"link":                                {"workspace のリンク path", "非 Git workspace root へ向けて symlink を作る path です。", "slot から source の実体を直接参照します。", nil},
	"reuse_standby":                       {"workspace の待機枠再利用", "対象 workspace の READY slot 更新方針です。", "global の待機枠再利用設定を上書きします。", nil},
	"submodules":                          {"workspace の submodule 準備", "対象 workspace の submodule 準備方針です。", "global の submodule 設定を上書きします。", nil},
	"warm_count":                          {"workspace の待機枠数", "対象 workspace に維持する READY slot 数です。", "0 にすると対象の自動補充を無効にします。", nil},
	"default_branch":                      {"既定 branch", "対象 repository の detached base に使う branch です。", "新しい worktree の起点に影響します。", nil},
	"dir_name":                            {"配置 directory 名", "slot 内で対象 repository に使う directory 名です。", "新しく準備する配置 path に影響します。", nil},
	"dir_source":                          {"配置名の由来", "対象 repository の directory 名を決める情報源です。", "global の命名方針を上書きします。", []string{"remote", "directory"}},
	"cow_min_size_kib":                    {"repository の CoW 最小サイズ", "対象 repository で CoW 共有するサイズ下限です。", "対象 repository の待機枠を作り直します。", nil},
	"prepare.command":                     {"準備 command", "checkout 後に対象 repository で実行する command です。", "失敗すると slot の準備も失敗します。", nil},
	"prepare.timeout":                     {"準備 command timeout", "準備 command の実行を待つ上限時間です。", "短すぎると準備が失敗します。", nil},
	"prepare.version":                     {"準備手順 version", "準備内容を意図的に無効化するための識別値です。", "変更すると以前の待機枠を再利用しません。", nil},
}

// Catalog は構造体から導出した全キーを、設定ファイルでの宣言順に返す。
func Catalog() []Metadata {
	byKey := map[string]*Metadata{}
	order := make([]string, 0)
	add := func(key string, field reflect.Value, scope string) {
		meta := byKey[key]
		if meta == nil {
			text := catalogTexts[key]
			group, _, _ := strings.Cut(key, ".")
			value := Metadata{Key: key, Kind: kindOf(field), DisplayName: text.name, Group: group, Description: text.description, Impact: text.impact, Choices: append([]string(nil), text.choices...)}
			meta = &value
			byKey[key] = meta
			order = append(order, key)
		}
		if !contains(meta.Scopes, scope) {
			meta.Scopes = append(meta.Scopes, scope)
		}
	}
	defaults := reflect.ValueOf(Defaults())
	walkConfigLeaves(defaults, "", func(key string, field reflect.Value) { add(key, field, "global") })
	walkConfigLists(defaults, "", func(key string, field reflect.Value) { add(key, field, "global") })
	for _, scope := range []Scope{ScopeWorkspace, ScopeRepository} {
		walkScopeFields(scope.newEntry(), "", func(key string, field reflect.Value) { add(key, field, scope.String()) })
	}
	out := make([]Metadata, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

func kindOf(field reflect.Value) ValueKind {
	for field.Kind() == reflect.Pointer {
		field = reflect.New(field.Type().Elem()).Elem()
	}
	switch {
	case field.Type() == durationType:
		return KindDuration
	case field.Kind() == reflect.Bool:
		return KindBoolean
	case field.Kind() == reflect.Int:
		return KindInteger
	case field.Kind() == reflect.Slice:
		return KindList
	default:
		return KindString
	}
}

// Describe は指定 scope で利用できる設定項目の説明を返す。
func Describe(key string, scope string) (Metadata, error) {
	for _, meta := range Catalog() {
		if meta.Key == key && (scope == "" || contains(meta.Scopes, scope)) {
			return meta, nil
		}
	}
	var keys []string
	for _, meta := range Catalog() {
		if scope == "" || contains(meta.Scopes, scope) {
			keys = append(keys, meta.Key)
		}
	}
	sort.Strings(keys)
	prefix := ""
	if scope != "" {
		prefix = scope + " "
	}
	return Metadata{}, fmt.Errorf("unknown %sconfig key %q; available keys: %s", prefix, key, strings.Join(keys, ", "))
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
