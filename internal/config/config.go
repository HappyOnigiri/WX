package config

import (
	"errors"
	"fmt"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
	sessionsconfig "github.com/HappyOnigiri/WorktreeX/internal/sessions/config"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string")
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", n.Value, err)
	}
	d.Duration = v
	return nil
}
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

type Config struct {
	// Version 2 は machine-wide 設定を system と2つの global profile に分ける。
	// 旧フィールドは既存 caller の実効値参照用にメモリへ残し、保存時は v2 形状だけを出力する。
	System             SystemConfig       `yaml:"system,omitempty"`
	WorkspaceDefaults  WorkspaceDefaults  `yaml:"workspace_defaults,omitempty"`
	RepositoryDefaults RepositoryDefaults `yaml:"repository_defaults,omitempty"`
	// Language は CLI・TUI・daemon の利用者向け表示言語で、global 設定だけが持つ。
	// 未記載は英語を意味し、workspace/repository の個別指定へは伝播しない。
	// v2 の正本は system.language で、この field はその flatten view である。
	Language     string                `yaml:"language,omitempty"`
	Worktree     WorktreePolicy        `yaml:"worktree,omitempty"`
	Version      int                   `yaml:"version,omitempty"`
	Storage      Storage               `yaml:"storage,omitempty"`
	Pool         Pool                  `yaml:"pool,omitempty"`
	Retention    Retention             `yaml:"retention,omitempty"`
	Discovery    Discovery             `yaml:"discovery,omitempty"`
	Readiness    Readiness             `yaml:"readiness,omitempty"`
	Resume       Resume                `yaml:"resume,omitempty"`
	Lease        Lease                 `yaml:"lease,omitempty"`
	Includes     Includes              `yaml:"includes,omitempty"`
	Agent        Agent                 `yaml:"agent,omitempty"`
	Sessions     sessionsconfig.Config `yaml:"sessions,omitempty"`
	Workspaces   map[string]Workspace  `yaml:"workspaces,omitempty"`
	Repositories map[string]Repository `yaml:"repositories,omitempty"`
	Logging      Logging               `yaml:"logging,omitempty"`
	Update       Update                `yaml:"update,omitempty"`
	Daemon       Daemon                `yaml:"daemon,omitempty"`
	present      map[string]bool
	// unknown は設定ファイルにあった wx が解釈しないキーで、doctor の報告と保存時の差し戻しに使う。
	unknown []unknownEntry
	// prepareOverride は貸出1回だけの準備設定の上書きで、設定ファイルにも workspaces/repositories にも現れない。
	// 解決ヘルパーはこれを最上位に置き、repository 個別指定より優先する。
	prepareOverride PrepareOverride
	// v2Explicit は旧 adapter の互換情報としてだけ残る。schema の判定には使わない。
	v2Explicit bool
}

const (
	LanguageEnglish  = "en"
	LanguageJapanese = "ja"
)

// DisplayLanguage は設定を直接組み立てた呼び出し側も安全に表示できるよう、
// 未設定・未対応値を英語へ戻して返す。
func (c Config) DisplayLanguage() string {
	language := c.System.Language
	if language == "" {
		language = c.Language
	}
	if language == LanguageJapanese {
		return LanguageJapanese
	}
	return LanguageEnglish
}

// WorktreeRoot は新しい system.storage と、v2 移行前に Config を直接組み立てていた
// caller の view を同じ値へ解決する。ファイルから得た実効 Config は flatten により
// 両方が一致するため、差分がある場合だけ直接 caller の変更を採用する。
func (c Config) WorktreeRoot() string {
	c = withLegacyAdapter(c)
	return c.System.Storage.WorktreeRoot
}

// LanguageForRPC は要求へ載せる表示言語を返す。未記載の英語は旧クライアントと
// 同じくフィールドを省略し、明示した英語と日本語だけを daemon へ伝える。
func (c Config) LanguageForRPC() string {
	language := c.System.Language
	if language == "" {
		language = c.Language
	}
	if language == LanguageJapanese {
		return LanguageJapanese
	}
	if _, explicit := c.rawLanguage(); explicit && language == LanguageEnglish {
		return LanguageEnglish
	}
	return ""
}

// rawLanguage は v1 の top-level と v2 の system.language のどちらで書かれていても、
// 設定ファイルで明示された表示言語を取り出す。値の妥当性は検証しない。
// 設定ファイル由来でない Config は未記載として扱い、既定値を明示指定と誤認しない。
func (c Config) rawLanguage() (string, bool) {
	if c.present == nil {
		return "", false
	}
	if c.has("system.language", false) {
		return c.System.Language, true
	}
	return "", false
}

// SystemConfig は daemon とマシン全体で共有する設定を保持する config v2 の
// system 節である。Repository や Workspace に属する値をここへ置かないため、
// global defaults を変更しても所属単位以外へ波及しないことを型で示す。
type SystemConfig struct {
	// Language は v2 における表示言語の正本で、top-level の Language はこの flatten view である。
	Language  string                `yaml:"language,omitempty"`
	Storage   SystemStorage         `yaml:"storage,omitempty"`
	Pool      SystemPool            `yaml:"pool,omitempty"`
	Retention SystemRetention       `yaml:"retention,omitempty"`
	Discovery SystemDiscovery       `yaml:"discovery,omitempty"`
	Resume    Resume                `yaml:"resume,omitempty"`
	Lease     Lease                 `yaml:"lease,omitempty"`
	Sessions  sessionsconfig.Config `yaml:"sessions,omitempty"`
	Logging   Logging               `yaml:"logging,omitempty"`
	Update    SystemUpdate          `yaml:"update,omitempty"`
	Daemon    SystemDaemon          `yaml:"daemon,omitempty"`
}

// SystemStorage は worktree root と state backup の設定である。
type SystemStorage struct {
	WorktreeRoot      string   `yaml:"worktree_root,omitempty"`
	BackupGenerations int      `yaml:"backup_generations,omitempty"`
	BackupRetention   Duration `yaml:"backup_retention,omitempty"`
}

// SystemPool は daemon 全体の worker 数である。
type SystemPool struct {
	PreparationConcurrency int `yaml:"preparation_concurrency,omitempty"`
}

// SystemRetention は workspace・repository に属さない保持期間である。
type SystemRetention struct {
	Quarantined             Duration `yaml:"quarantined,omitempty"`
	RecoverySnapshot        Duration `yaml:"recovery_snapshot,omitempty"`
	ExpiredSessionTombstone Duration `yaml:"expired_session_tombstone,omitempty"`
	FailedJob               Duration `yaml:"failed_job,omitempty"`
	EventLog                Duration `yaml:"event_log,omitempty"`
}

// SystemDiscovery は全 workspace の探索実行を制限する値である。
type SystemDiscovery struct {
	MaxEntries        int      `yaml:"max_entries,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	ReconcileInterval Duration `yaml:"reconcile_interval,omitempty"`
}

// WorkspaceDefaults は Workspace 設定の global 既定値である。list の nil
// は組み込み既定値の継承、空 list は明示的な空を表す。
type WorkspaceDefaults struct {
	Worktree     string   `yaml:"worktree,omitempty"`
	Copy         []string `yaml:"copy,omitempty"`
	Link         []string `yaml:"link,omitempty"`
	ReuseStandby *bool    `yaml:"reuse_standby,omitempty"`
	// FetchDefaultBranch は branch 未指定の貸出前に origin の既定 branch を fetch するかを決める。
	// nil は組み込み既定値（false）を継承し、明示 false と未指定を区別する。
	FetchDefaultBranch *bool              `yaml:"fetch_default_branch,omitempty"`
	WarmCount          *int               `yaml:"warm_count,omitempty"`
	Agent              WorkspaceAgent     `yaml:"agent,omitempty"`
	Retention          WorkspaceRetention `yaml:"retention,omitempty"`
	Discovery          WorkspaceDiscovery `yaml:"discovery,omitempty"`
}

// RepositoryDefaults は Repository 設定の global 既定値である。
// dir_name は membership ごとの配置名なのでここには持たせない。
type RepositoryDefaults struct {
	DefaultBranch string              `yaml:"default_branch,omitempty"`
	DirSource     string              `yaml:"dir_source,omitempty"`
	COWMinSizeKiB *int                `yaml:"cow_min_size_kib,omitempty"`
	Submodules    *bool               `yaml:"submodules,omitempty"`
	Prepare       Prepare             `yaml:"prepare,omitempty"`
	Includes      RepositoryIncludes  `yaml:"includes,omitempty"`
	Readiness     RepositoryReadiness `yaml:"readiness,omitempty"`
	Storage       RepositoryStorage   `yaml:"storage,omitempty"`
}

// repositoryDefaultsAsRepository は v2 の global repository profile を既存の
// repository 設定型へ写す。既存の準備コードはこの型を受け取るため、解決境界を
// config package に閉じたまま段階的に移行できる。
func repositoryDefaultsAsRepository(d RepositoryDefaults) Repository {
	prepare := d.Prepare
	prepare.Command = cloneStrings(prepare.Command)
	prepare.Inputs = cloneStrings(prepare.Inputs)
	readiness := d.Readiness
	readiness.EarlyPaths = cloneStrings(readiness.EarlyPaths)
	return Repository{
		DefaultBranch: d.DefaultBranch,
		DirSource:     d.DirSource,
		COWMinSizeKiB: d.COWMinSizeKiB,
		Submodules:    d.Submodules,
		Prepare:       prepare,
		Includes:      d.Includes,
		Readiness:     readiness,
		Storage:       d.Storage,
	}
}

type WorktreePolicy struct {
	Undefined    string `yaml:"undefined,omitempty"`
	ReuseStandby bool   `yaml:"reuse_standby,omitempty"`
	// FetchDefaultBranch は branch 未指定の貸出前に origin の既定 branch を fetch するかを決める。
	FetchDefaultBranch bool `yaml:"fetch_default_branch,omitempty"`
	// Submodules は準備時に submodule を worktree へ実体化するかを決める。
	// linked worktree の submodule gitdir は共有できないため、有効なときは main の `.git/modules/<name>` からローカル clone する。
	Submodules bool `yaml:"submodules,omitempty"`
}

type Storage struct {
	WorktreeRoot string `yaml:"worktree_root,omitempty"`
	CopyMode     string `yaml:"copy_mode,omitempty"`
	// COWMinSizeKiB は CoW 共有の対象にするファイルサイズの下限（KiB）で、下限未満は通常 checkout のまま残す。
	// 0 は下限なしで、共有できる通常ファイルをすべて対象にする。
	COWMinSizeKiB int `yaml:"cow_min_size_kib,omitempty"`
	// RepoDirSource は slot 内の repository directory 名の導出方法を選ぶ。
	// remote は origin URL の basename、directory は main worktree の名前を使い、Repositories の個別指定を優先する。
	RepoDirSource     string   `yaml:"repo_dir_source,omitempty"`
	BackupGenerations int      `yaml:"backup_generations,omitempty"`
	BackupRetention   Duration `yaml:"backup_retention,omitempty"`
}
type Pool struct {
	WarmPerWorkspace int `yaml:"warm_per_workspace,omitempty"`
	// PreparationConcurrency は利用者が完了を待つ準備・復元・保存の同時実行数である。
	// 待機枠の補充と自動削除はこの枠を使わず、internal/daemon の maintenanceJobSlots 本の保守枠で動く。
	PreparationConcurrency int `yaml:"preparation_concurrency,omitempty"`
}
type Retention struct {
	HotStandby    Duration `yaml:"hot_standby,omitempty"`
	EndedWorktree Duration `yaml:"ended_worktree,omitempty"`
	// Quarantined は隔離slotの実体をGCが削除するまでの保持期間。
	// LEASEDから隔離へ落ちたslotを調査前に消さないよう、ended_worktreeより長く取る。
	Quarantined             Duration `yaml:"quarantined,omitempty"`
	RecoverySnapshot        Duration `yaml:"recovery_snapshot,omitempty"`
	ExpiredSessionTombstone Duration `yaml:"expired_session_tombstone,omitempty"`
	FailedJob               Duration `yaml:"failed_job,omitempty"`
	EventLog                Duration `yaml:"event_log,omitempty"`
}
type Discovery struct {
	MaxDepth          int      `yaml:"max_depth,omitempty"`
	MaxEntries        int      `yaml:"max_entries,omitempty"`
	Timeout           Duration `yaml:"timeout,omitempty"`
	ReconcileInterval Duration `yaml:"reconcile_interval,omitempty"`
	Exclude           []string `yaml:"exclude,omitempty"`
}
type Readiness struct {
	Mode       string   `yaml:"mode,omitempty"`
	EarlyPaths []string `yaml:"early_paths,omitempty"`
	Timeout    Duration `yaml:"timeout,omitempty"`
	// Progress は準備待ちの進捗行を stderr へ出すかを決める。既定は有効で、無効にすると
	// 待機中は何も出さずに準備の完了を待つ。端末でない出力先へは、この設定に関わらず出さない。
	Progress bool `yaml:"progress,omitempty"`
}

// Resume は会話の再開時の既定の振る舞いを決める。
type Resume struct {
	// AutoFresh は、当時の worktree を復元できないときの確認を省き、新しい worktree での再開をそのまま選ぶ。
	AutoFresh bool `yaml:"auto_fresh,omitempty"`
}

// Lease は agent 起動以外への worktree 貸出（wx shell / wx run / wx new）の設定である。
type Lease struct {
	// TTL は貸出の期限である。期限が来ても保存されてから返却され、実体は retention.ended_worktree の間残る。
	// プロセスに随伴しない wx new の貸出を、返却し忘れたまま無期限に居座らせないための保険である。
	TTL Duration `yaml:"ttl,omitempty"`
	// Shell は wx shell が起動するシェルを固定する。空なら $SHELL、それも無ければ /bin/sh を使う。
	Shell string `yaml:"shell,omitempty"`
}

// Update は新しいリリースを wx 自身が確認するかどうかの方針で、legacy の flatten view である。
type Update struct {
	AutoCheck bool `yaml:"auto_check,omitempty"`
	AutoApply bool `yaml:"auto_apply,omitempty"`
}

// SystemUpdate は config v2 における Update の正本である。
// 既定が有効なため、未記載と明示した false を区別できるようポインタで持つ。
type SystemUpdate struct {
	AutoCheck *bool `yaml:"auto_check,omitempty"`
	// AutoApply は確認で見つかった新版を daemon が自動で適用するかを決める。
	// 適用は AutoCheck が有効なときにだけ起き、無効にしても新版のお知らせは出続ける。
	AutoApply *bool `yaml:"auto_apply,omitempty"`
}

// Daemon は daemon process 自体の起動方法で、legacy の flatten view である。
type Daemon struct {
	// LoginShell は LaunchAgent が daemon をログインシェル経由で起動するかを決める。
	// 有効なとき、利用者のログインシェルの起動ファイルが組み立てた PATH が daemon と
	// その子（準備の hook を含む）へそのまま渡る。
	LoginShell bool `yaml:"login_shell,omitempty"`
}

// SystemDaemon は config v2 における Daemon の正本である。
// 既定が有効なため、未記載と明示した false を区別できるようポインタで持つ。
type SystemDaemon struct {
	LoginShell *bool `yaml:"login_shell,omitempty"`
}

type Workspace struct {
	Worktree     string   `yaml:"worktree,omitempty"`
	Copy         []string `yaml:"copy,omitempty"`
	Link         []string `yaml:"link,omitempty"`
	ReuseStandby *bool    `yaml:"reuse_standby,omitempty"`
	// FetchDefaultBranch は workspace_defaults.fetch_default_branch をこの workspace だけ上書きする。
	FetchDefaultBranch *bool `yaml:"fetch_default_branch,omitempty"`
	// Submodules は workspace 個別の submodule 実体化方針で、nil のときは worktree.submodules を継承する。
	Submodules *bool `yaml:"submodules,omitempty"`
	// WarmCount は workspace 個別の待機枠数で、nil のときは pool.warm_per_workspace を継承する。
	// ポインタで明示的な 0 と未指定を区別する。
	WarmCount *int `yaml:"warm_count,omitempty"`
	// Agent・Retention・Discovery は global の同名キー路をそのまま写した個別指定である。
	// slot の本数・寿命・探索範囲は workspace の形（単一 repository か multi か）と容量事情で変わる。
	Agent     WorkspaceAgent     `yaml:"agent,omitempty"`
	Retention WorkspaceRetention `yaml:"retention,omitempty"`
	Discovery WorkspaceDiscovery `yaml:"discovery,omitempty"`
	// RepositoryDefaults はこの Workspace に所属する全 Repository の
	// 共通上書きである。dir_name のような membership 専用値は含めない。
	RepositoryDefaults RepositoryDefaults `yaml:"repository_defaults,omitempty"`
	// Repositories は Workspace root からの正規化済み相対 path ごとの
	// membership 設定である。旧 Config.Repositories とは異なり、同じ
	// Repository を複数 Workspace で独立して設定できる。
	Repositories map[string]Repository `yaml:"repositories,omitempty"`
	// Onboarding は workspace root 自身が repository の場合の初回検査記録である。
	// 配下 repository の記録は Repositories の各 membership が持つ。
	Onboarding RepositoryOnboarding `yaml:"onboarding,omitempty"`
	// Discovered は dashboard が daemon status と config を統合するときだけ使う表示用印で、保存対象ではない。
	Discovered bool `yaml:"-"`
}

// WorkspaceAgent は agent 節の workspace 個別指定である。
type WorkspaceAgent struct {
	AddDir        string `yaml:"add_dir,omitempty"`
	CodexNoDaemon *bool  `yaml:"codex_no_daemon,omitempty"`
}

// WorkspaceRetention は retention 節の workspace 個別指定である。
// 0 にも「即時回収」の意味があるため、未指定と区別できるようポインタで持つ。
type WorkspaceRetention struct {
	HotStandby    *Duration `yaml:"hot_standby,omitempty"`
	EndedWorktree *Duration `yaml:"ended_worktree,omitempty"`
}

// WorkspaceDiscovery は discovery 節の workspace 個別指定である。
// Exclude は global list への追加ではなく置き換えで、実効値が設定ファイルだけで読めるようにする。
type WorkspaceDiscovery struct {
	MaxDepth *int     `yaml:"max_depth,omitempty"`
	Exclude  []string `yaml:"exclude,omitempty"`
}
type Includes struct {
	DefaultAgentRules bool `yaml:"default_agent_rules,omitempty"`
}

// Agent は agent プロセスへ渡す引数の組み立て方を決める。
type Agent struct {
	// AddDir は CWD 直下の repository directory を agent の --add-dir へ渡す条件を決める。
	// 複数 repository の workspace では agent の CWD が repository の親になり、渡さないと配下の .claude/skills などが読まれない。
	AddDir        string `yaml:"add_dir,omitempty"`
	CodexNoDaemon bool   `yaml:"codex_no_daemon,omitempty"`
}
type Repository struct {
	DefaultBranch string `yaml:"default_branch,omitempty"`
	// DirName は slot 内の repository directory 名を固定する。
	// 空なら DirSource（remote または directory）で導出する。Repositories は map のため YAML で直接指定する。
	DirName   string `yaml:"dir_name,omitempty"`
	DirSource string `yaml:"dir_source,omitempty"`
	// COWMinSizeKiB は repository 個別の CoW 共有下限（KiB）で、nil のときは storage.cow_min_size_kib を継承する。
	// 最適な下限は repository のファイルサイズ分布で変わるため個別に指定できる。ポインタで明示的な 0（下限なし）と未指定を区別する。
	// Repositories は map のため YAML で直接指定する。
	COWMinSizeKiB *int `yaml:"cow_min_size_kib,omitempty"`
	// Submodules は repository 個別の submodule 実体化方針で、nil は
	// repository_defaults または組み込み既定値を継承する。
	Submodules *bool              `yaml:"submodules,omitempty"`
	Prepare    Prepare            `yaml:"prepare,omitempty"`
	Includes   RepositoryIncludes `yaml:"includes,omitempty"`
	// Readiness・Storage は global の同名キー路をそのまま写した個別指定である。
	// slot の中身を決める値は repository の prepare.command と checkout 規模で変わる。
	Readiness RepositoryReadiness `yaml:"readiness,omitempty"`
	Storage   RepositoryStorage   `yaml:"storage,omitempty"`
	// Onboarding は初回 worktree 検査の完了または今後の確認を辞退した記録である。
	Onboarding RepositoryOnboarding `yaml:"onboarding,omitempty"`
	// Discovered は dashboard が daemon の membership 一覧を統合するときだけ使う表示用印で、保存対象ではない。
	Discovered bool `yaml:"-"`
}

type RepositoryOnboarding struct {
	CheckedAt  string `yaml:"checked_at,omitempty"`
	DeclinedAt string `yaml:"declined_at,omitempty"`
}

// RepositoryReadiness は readiness 節の repository 個別指定である。
// EarlyPaths は global list の置き換えで、組み込みの既定 path は置き換えても常に残る。
type RepositoryReadiness struct {
	Mode       string    `yaml:"mode,omitempty"`
	EarlyPaths []string  `yaml:"early_paths,omitempty"`
	Timeout    *Duration `yaml:"timeout,omitempty"`
	Progress   *bool     `yaml:"progress,omitempty"`
}

// RepositoryStorage は storage 節の repository 個別指定である。空文字は未指定を表す。
type RepositoryStorage struct {
	CopyMode string `yaml:"copy_mode,omitempty"`
}
type RepositoryIncludes struct {
	DefaultAgentRules *bool `yaml:"default_agent_rules,omitempty"`
}
type Prepare struct {
	Command []string `yaml:"command,omitempty"`
	Inputs  []string `yaml:"inputs,omitempty"`
	// Timeout は nil が未指定で、上位 scope の値をそのまま継承する。
	// 明示した `0s` は「repository 固有の timeout を使わず readiness timeout へ
	// fallback する」指定なので、継承と区別して下位 scope へ残す。
	Timeout *Duration `yaml:"timeout,omitempty"`
	Version string    `yaml:"version,omitempty"`
}
type Logging struct {
	Level string `yaml:"level,omitempty"`
}

// RepoDirSourceRemote と RepoDirSourceDirectory は storage.repo_dir_source と repositories.<path>.dir_source の値。
const (
	RepoDirSourceRemote    = "remote"
	RepoDirSourceDirectory = "directory"
)

// AgentAddDir* は agent.add_dir の値である。
const (
	// AgentAddDirAlways は worktree を作るかどうかに関わらず、agent の CWD 直下の repository を渡す。
	AgentAddDirAlways = "always"
	// AgentAddDirWorktree は wx が用意した worktree で起動したときだけ渡し、worktree 無しの直起動では渡さない。
	AgentAddDirWorktree = "worktree"
	// AgentAddDirOff はどちらの起動でも渡さない。
	AgentAddDirOff = "off"
)

const (
	CopyModeAuto = "auto"
	CopyModeCOW  = "cow"
	CopyModeCopy = "copy"
)

const (
	// DefaultCOWMinSizeKiB は storage.cow_min_size_kib の既定値である。
	// 数KBのファイルはブロック共有で減る容量より clone・比較・metadata 検査の定数費用が勝つため、既定では共有しない。
	DefaultCOWMinSizeKiB = 16
	// MaxCOWMinSizeKiB は設定できる上限である。
	// bytes 換算での桁溢れを防ぐためだけの上限で、これ以上は共有対象が無いのと変わらない。
	MaxCOWMinSizeKiB = 1 << 20
)

// COWMinShareSize は CoW 共有の下限を bytes で返す。0 は下限なしを表す。
func (s Storage) COWMinShareSize() int64 { return int64(s.COWMinSizeKiB) << 10 }

// COWMinSizeKiB は repository の CoW 共有下限（KiB）を解決する。
// 貸出1回の上書き、repository 個別指定、global 設定の順に優先する。
// mainPath は NormalizePaths 済み canonical path であることを呼び出し側の契約とする。
func (c Config) COWMinSizeKiB(mainPath string) int {
	return c.COWMinSizeKiBForWorkspaceRepository("", ".", mainPath)
}

// CopyMode は repository のコピー方式を解決する。
// 貸出1回の上書き、repository 個別指定、global 設定の順に優先する。
// mainPath は NormalizePaths 済み canonical path であることを呼び出し側の契約とする。
func (c Config) CopyMode(mainPath string) string {
	return c.CopyModeForWorkspaceRepository("", ".", mainPath)
}

// COWMinShareSize は repository の CoW 共有下限を bytes で返す。0 は下限なしを表す。
func (c Config) COWMinShareSize(mainPath string) int64 {
	return int64(c.COWMinSizeKiB(mainPath)) << 10
}

// defaultsLegacy は既定値を旧 in-memory adapter の形へ組み立てる。
// 保存・読み込みの正本は Defaults が返す v2 Config である。
func defaultsLegacy() Config {
	return Config{
		Language: LanguageEnglish,
		Worktree: WorktreePolicy{Undefined: "ask", ReuseStandby: true, FetchDefaultBranch: false, Submodules: true},
		Storage: Storage{
			WorktreeRoot: "$HOME/wx", CopyMode: CopyModeAuto, COWMinSizeKiB: DefaultCOWMinSizeKiB,
			RepoDirSource: RepoDirSourceRemote, BackupGenerations: 3, BackupRetention: Duration{168 * time.Hour},
		},
		Pool:      Pool{WarmPerWorkspace: 2, PreparationConcurrency: 2},
		Retention: Retention{Duration{168 * time.Hour}, Duration{time.Hour}, Duration{24 * time.Hour}, Duration{720 * time.Hour}, Duration{8760 * time.Hour}, Duration{168 * time.Hour}, Duration{168 * time.Hour}},
		Discovery: Discovery{MaxDepth: 6, MaxEntries: 100000, Timeout: Duration{30 * time.Second}, ReconcileInterval: Duration{10 * time.Minute}, Exclude: []string{"node_modules", "vendor", ".venv", "venv", "tmp", "log"}},
		Readiness: Readiness{Mode: "early", Timeout: Duration{10 * time.Minute}, Progress: true}, Resume: Resume{AutoFresh: false},
		Lease:    Lease{TTL: Duration{72 * time.Hour}},
		Includes: Includes{DefaultAgentRules: true}, Agent: Agent{AddDir: AgentAddDirAlways, CodexNoDaemon: true}, Logging: Logging{Level: "info"},
		Sessions:   sessionsconfig.Defaults(),
		Update:     Update{AutoCheck: true, AutoApply: true},
		Daemon:     Daemon{LoginShell: true},
		Workspaces: map[string]Workspace{}, Repositories: map[string]Repository{},
	}
}

// Defaults は組み込み既定値を config v2 の正本として返す。
// 旧 field も adapter として同じ実効値を持つが、YAML へは出力されない。
func Defaults() Config {
	c := DefaultsV2()
	// 既存 caller の移行期間は read-only adapter も埋めるが、保存と解決の正本は
	// v2 section だけに保つ。
	flattenV2(&c)
	return c
}

// DefaultAgentRulesEnabled は repository へ既定の agent rule をコピーするか解決する。
// 個別指定が global 設定より優先される。
func (c Config) DefaultAgentRulesEnabled(mainPath string) bool {
	return c.DefaultAgentRulesForWorkspaceRepository("", ".", mainPath)
}

// EffectiveEqual は正規化・検証を終えた実効設定として2つのConfigが同じ値かを返す。
// どのキーがファイルに書かれていたかの記録は実効値に影響しないため比較から外し、
// 書式だけが変わった設定ファイルを設定変更として扱わない。
func (c Config) EffectiveEqual(other Config) bool {
	c.present = nil
	other.present = nil
	// 未知キーは実効値ではないため、その有無だけで設定変更とみなさない。
	c.unknown = nil
	other.unknown = nil
	// 貸出1回の上書きは設定ファイルの実効値ではないため、reload の差分判定からも外す。
	c.prepareOverride = PrepareOverride{}
	other.prepareOverride = PrepareOverride{}
	return reflect.DeepEqual(c, other)
}

func Merge(d, raw Config) Config {
	// 未知キーは実効設定にも引き継ぐ。daemon が持つのは Merge 後の Config であり、doctor はそこから報告する。
	if raw.Version == 0 {
		d.unknown = raw.unknown
		return d
	}
	if !raw.V2() {
		// LoadRaw は Merge 前に拒否する。直接 caller でも v1 を黙って merge せず、
		// version を残して同じ検証エラーへ到達させる。
		d.Version = raw.Version
		d.unknown = raw.unknown
		return d
	}
	v2 := effectiveV2Defaults(raw)
	v2.unknown = raw.unknown
	return v2
}

// validateLanguage は表示言語を正規化し、対応しない値を拒否する。
func validateLanguage(c *Config) error {
	if c.System.Language == "" && !c.has("system.language", false) {
		// 呼び出し側が zero Config を組み立てても未記載＝英語として扱う。
		// YAML で明示された空文字は present に残るため、下の不正値検査を通る。
		c.System.Language = LanguageEnglish
	}
	if c.System.Language != LanguageEnglish && c.System.Language != LanguageJapanese {
		return i18n.NewError("config.language.invalid", nil)
	}
	c.Language = c.System.Language
	return nil
}

func validateSchema(c *Config) error {
	if c.V2() {
		if err := ValidateV2Rules(c); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	return nil
}

func Validate(c *Config) error {
	if c == nil {
		return errors.New("config is nil")
	}
	*c = withLegacyAdapter(*c)
	if err := validateSchema(c); err != nil {
		return err
	}
	// LoadWithRaw を通らず sparse な v2 Config を直接組み立てる caller もいるため、
	// validator と consumer が同じ組み込み既定値を見るよう実効 view をここで埋める。
	if c.V2() && (c.present != nil || (c.SystemIsZero() && c.WorkspaceDefaultsIsZero() && c.RepositoryDefaultsIsZero())) {
		unknown, present := c.unknown, c.present
		normalized := effectiveV2Defaults(*c)
		normalized.unknown, normalized.present = unknown, present
		*c = normalized
	}
	if err := validateLanguage(c); err != nil {
		return err
	}
	if err := validateReadiness(&c.Readiness); err != nil {
		return err
	}
	if !validWorktreeMode(c.Worktree.Undefined, true) {
		return errors.New("worktree.undefined must be ask, hot, cold, or off")
	}
	if err := validateWorkspaces(c.Workspaces); err != nil {
		return err
	}
	if err := validateStorage(&c.Storage); err != nil {
		return err
	}
	if err := validateRepositories(c.Repositories); err != nil {
		return err
	}
	if c.Agent.AddDir != AgentAddDirAlways && c.Agent.AddDir != AgentAddDirWorktree && c.Agent.AddDir != AgentAddDirOff {
		return fmt.Errorf("agent.add_dir must be %s, %s, or %s", AgentAddDirAlways, AgentAddDirWorktree, AgentAddDirOff)
	}
	if c.Pool.WarmPerWorkspace < 0 || c.Pool.PreparationConcurrency < 1 {
		return errors.New("pool counts must be non-negative and concurrency must be at least 1")
	}
	for k, v := range map[string]time.Duration{"retention.hot_standby": c.Retention.HotStandby.Duration, "retention.ended_worktree": c.Retention.EndedWorktree.Duration, "retention.quarantined": c.Retention.Quarantined.Duration, "retention.recovery_snapshot": c.Retention.RecoverySnapshot.Duration, "retention.expired_session_tombstone": c.Retention.ExpiredSessionTombstone.Duration, "retention.failed_job": c.Retention.FailedJob.Duration, "retention.event_log": c.Retention.EventLog.Duration, "discovery.timeout": c.Discovery.Timeout.Duration, "discovery.reconcile_interval": c.Discovery.ReconcileInterval.Duration, "readiness.timeout": c.Readiness.Timeout.Duration, "lease.ttl": c.Lease.TTL.Duration} {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", k)
		}
	}
	for k, v := range map[string]time.Duration{"discovery.timeout": c.Discovery.Timeout.Duration, "readiness.timeout": c.Readiness.Timeout.Duration} {
		if v <= 0 {
			return fmt.Errorf("%s must be positive", k)
		}
	}
	if c.Discovery.MaxDepth < 1 || c.Discovery.MaxEntries < 1 {
		return errors.New("discovery limits must be positive")
	}
	if c.Logging.Level != "debug" && c.Logging.Level != "info" && c.Logging.Level != "warn" && c.Logging.Level != "error" {
		return errors.New("logging.level must be debug, info, warn, or error")
	}
	return c.Sessions.Validate()
}

func validateWorkspaces(workspaces map[string]Workspace) error {
	for path, workspace := range workspaces {
		if workspace.Worktree != "" && !validWorktreeMode(workspace.Worktree, false) {
			return fmt.Errorf("workspaces.%s.worktree must be hot, cold, or off", path)
		}
		if workspace.WarmCount != nil && *workspace.WarmCount < 0 {
			return fmt.Errorf("workspaces.%s.warm_count must not be negative", path)
		}
		if err := validateWorkspaceOverride(path, workspace); err != nil {
			return err
		}
	}
	return nil
}

func validateRepositories(repositories map[string]Repository) error {
	for path, override := range repositories {
		if override.DirSource != "" && override.DirSource != RepoDirSourceRemote && override.DirSource != RepoDirSourceDirectory {
			return fmt.Errorf("repositories.%s.dir_source must be %s or %s", path, RepoDirSourceRemote, RepoDirSourceDirectory)
		}
		if override.Prepare.Timeout != nil && override.Prepare.Timeout.Duration < 0 {
			return fmt.Errorf("repositories.%s.prepare.timeout must not be negative", path)
		}
		if override.COWMinSizeKiB != nil && (*override.COWMinSizeKiB < 0 || *override.COWMinSizeKiB > MaxCOWMinSizeKiB) {
			return fmt.Errorf("repositories.%s.cow_min_size_kib must be between 0 and %d", path, MaxCOWMinSizeKiB)
		}
		normalized, err := validateRepositoryOverride(path, override)
		if err != nil {
			return err
		}
		// early_paths の正規化結果を書き戻し、Validate 後の値をそのまま消費側の実効値にする。
		repositories[path] = normalized
	}
	return nil
}

// validateWorkspaceOverride は workspace 個別指定のうち、global 側と同じ条件を課す項目を検査する。
// discovery.exclude は global 側にも検査が無いため、ここでも検査しない。
func validateWorkspaceOverride(path string, workspace Workspace) error {
	if mode := workspace.Agent.AddDir; mode != "" && mode != AgentAddDirAlways && mode != AgentAddDirWorktree && mode != AgentAddDirOff {
		return fmt.Errorf("workspaces.%s.agent.add_dir must be %s, %s, or %s", path, AgentAddDirAlways, AgentAddDirWorktree, AgentAddDirOff)
	}
	for key, d := range map[string]*Duration{"retention.hot_standby": workspace.Retention.HotStandby, "retention.ended_worktree": workspace.Retention.EndedWorktree} {
		if d != nil && d.Duration < 0 {
			return fmt.Errorf("workspaces.%s.%s must not be negative", path, key)
		}
	}
	if workspace.Discovery.MaxDepth != nil && *workspace.Discovery.MaxDepth < 1 {
		return fmt.Errorf("workspaces.%s.discovery.max_depth must be positive", path)
	}
	return nil
}

// validateRepositoryOverride は repository 個別指定を検査し、early_paths を正規化した個別指定を返す。
func validateRepositoryOverride(path string, override Repository) (Repository, error) {
	if override.Prepare.Timeout != nil && override.Prepare.Timeout.Duration < 0 {
		return Repository{}, fmt.Errorf("repositories.%s.prepare.timeout must not be negative", path)
	}
	if override.Prepare.Inputs != nil {
		inputs, err := validatePrepareInputs(override.Prepare.Inputs, fmt.Sprintf("repositories.%s.prepare.inputs", path))
		if err != nil {
			return Repository{}, err
		}
		override.Prepare.Inputs = inputs
	}
	if mode := override.Readiness.Mode; mode != "" && mode != "early" && mode != "full" {
		return Repository{}, fmt.Errorf("repositories.%s.readiness.mode must be early or full", path)
	}
	if override.Readiness.Timeout != nil && override.Readiness.Timeout.Duration <= 0 {
		return Repository{}, fmt.Errorf("repositories.%s.readiness.timeout must be positive", path)
	}
	if mode := override.Storage.CopyMode; mode != "" && mode != CopyModeAuto && mode != CopyModeCOW && mode != CopyModeCopy {
		return Repository{}, fmt.Errorf("repositories.%s.storage.copy_mode must be %s, %s, or %s", path, CopyModeAuto, CopyModeCOW, CopyModeCopy)
	}
	if override.Readiness.EarlyPaths != nil {
		paths, err := validateEarlyPaths(override.Readiness.EarlyPaths, fmt.Sprintf("repositories.%s.readiness.early_paths", path))
		if err != nil {
			return Repository{}, err
		}
		override.Readiness.EarlyPaths = paths
	}
	return override, nil
}

func validateStorage(s *Storage) error {
	if _, err := ExpandHome(s.WorktreeRoot); err != nil {
		return fmt.Errorf("storage.worktree_root: %w", err)
	}
	if s.CopyMode != CopyModeAuto && s.CopyMode != CopyModeCOW && s.CopyMode != CopyModeCopy {
		return errors.New("storage.copy_mode must be auto, cow, or copy")
	}
	if s.COWMinSizeKiB < 0 || s.COWMinSizeKiB > MaxCOWMinSizeKiB {
		return fmt.Errorf("storage.cow_min_size_kib must be between 0 and %d", MaxCOWMinSizeKiB)
	}
	if s.BackupGenerations < 1 || s.BackupRetention.Duration < 0 {
		return errors.New("storage backup_generations must be positive and backup_retention must not be negative")
	}
	if s.RepoDirSource != RepoDirSourceRemote && s.RepoDirSource != RepoDirSourceDirectory {
		return fmt.Errorf("storage.repo_dir_source must be %s or %s", RepoDirSourceRemote, RepoDirSourceDirectory)
	}
	return nil
}

func validWorktreeMode(mode string, allowAsk bool) bool {
	return mode == "hot" || mode == "cold" || mode == "off" || (allowAsk && mode == "ask")
}
