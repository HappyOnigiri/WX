package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/state"
	"github.com/HappyOnigiri/WX/internal/update"
)

// updateApplyLogName は自動適用の子の出力を残すファイル名である。
// daemon log は構造化出力なので、install.sh の生の出力は混ぜずに別ファイルへ分ける。
const updateApplyLogName = "update-apply.log"

// updateApplySystemPath は子の PATH の末尾へ足す system の path である。
// install.sh が curl・shasum・plutil・launchctl・git を要求するのに対し、
// launchd から起動した daemon の PATH はこれらを含むとは限らない。
const updateApplySystemPath = "/usr/bin:/bin:/usr/sbin:/sbin"

// updateApplyHooks は自動適用の子の起動と出力先をまとめた差し替え点である。
// production では Manager が nil を持ち、resolveUpdateApply が実装と既定の置き場を埋める。
type updateApplyHooks struct {
	// spawn は argv の子をセッションを切り離して起動し、終了を待たずに返す。
	spawn func(argv, env []string, logPath string) error
	// logDir は子の出力ファイルを置く directory である。
	logDir string
}

// resolveUpdateApply は差し替えのない項目を production の実装で埋めて返す。
func (m *Manager) resolveUpdateApply() (updateApplyHooks, error) {
	m.mu.RLock()
	injected := m.updateApply
	m.mu.RUnlock()
	hooks := updateApplyHooks{spawn: spawnDetached}
	if injected != nil {
		if injected.spawn != nil {
			hooks.spawn = injected.spawn
		}
		hooks.logDir = injected.logDir
	}
	if hooks.logDir != "" {
		return hooks, nil
	}
	logPath, err := config.LogPath()
	if err != nil {
		return updateApplyHooks{}, err
	}
	hooks.logDir = filepath.Dir(logPath)
	return hooks, nil
}

// updateApplyEnabled は自動適用の前提が揃っているかを返す。
// 確認が無効なら適用の判断材料そのものが更新されないため、確認の前提をそのまま引き継ぐ。
// auto_apply を読むのはここだけにする。UpdateState 側が読むと、無効にした利用者へ案内が出なくなる。
func (m *Manager) updateApplyEnabled(probe updateProbe) bool {
	cfg := m.Config()
	return m.updateCheckEnabled(probe) && cfg.System.Update.AutoApply != nil && *cfg.System.Update.AutoApply
}

// updateApplyIdle は自動適用を始めてよい静けさかを返す。専用の述語にするのは、
// 明示的な restart / stop のゲート（runPendingLifecycle）へ貸出条件を足すと、
// 長時間の貸出が 1 件あるだけで `wx daemon stop` が返らなくなるためである。
func (m *Manager) updateApplyIdle(ctx context.Context) bool {
	m.mu.RLock()
	busy := m.inflightRequests > 0 || m.restartPending || m.stopPending || m.lifecycleClaimed
	m.mu.RUnlock()
	if busy {
		return false
	}
	// launchd の管理下でなければ、置き換えても誰も新しい process へ入れ替えない。
	if !m.underLaunchd() {
		return false
	}
	status, err := m.store.Status(ctx)
	if err != nil {
		return false
	}
	// DRAINING と SNAPSHOTTING の返却途中は必ず job を伴うので、Jobs == 0 がその区間も覆う。
	return status.Jobs == 0 && status.Leased == 0
}

// maybeApplyUpdate は記録済みの確認結果に新版があれば、貸出 0 件のアイドルで適用を始める。
// 始めるのは `wx update --apply` の起動までで、ダウンロード・checksum 検証・atomic な置換は install.sh が、
// 置換後の daemon の入れ替えは detectExecutableReplacement が持つ。
// 確認の 6 時間間引きには乗せず保守の一巡ごとに判定する。確認した時点で貸出中だった新版を、
// 次の一巡で拾えるようにするためである。同じ版を繰り返し適用しないための歯止めは claim が持つ。
// commentlint:allow-long -- 起動までの責務の切れ目と、間引きに乗せない理由をひと続きで示す必要がある
func (m *Manager) maybeApplyUpdate(ctx context.Context) {
	probe := m.resolveUpdateProbe()
	if !m.updateApplyEnabled(probe) {
		return
	}
	record, err := m.store.UpdateCheck(ctx)
	if err != nil {
		m.log.Error("update check state is unreadable", "error", err)
		return
	}
	if !update.Newer(probe.current(), record.LatestVersion) {
		return
	}
	m.mu.RLock()
	executable := m.executablePath
	m.mu.RUnlock()
	// watchExecutable が基準を取れなかった daemon は置換を検知できず、入れ替わらないまま古い process が残る。
	// PATH 上の wx で代替もしない。daemon 自身の実体とは限らないためである。
	if executable == "" {
		return
	}
	if !m.updateApplyIdle(ctx) {
		return
	}
	hooks, err := m.resolveUpdateApply()
	if err != nil {
		m.log.Error("automatic update has nowhere to write its log", "error", err)
		return
	}
	logPath := filepath.Join(hooks.logDir, updateApplyLogName)
	attempt, err := m.store.ClaimUpdateApply(ctx, record.LatestVersion, time.Now())
	if err != nil {
		m.log.Error("automatic update could not be claimed", "error", err)
		return
	}
	if attempt == 0 {
		return
	}
	if attempt >= state.UpdateApplyMaxAttempts {
		// 親は子の成否を観測できないので、諦める前に 1 回だけ残す。案内は消えないため手で適用できる。
		m.log.Warn("automatic update is making its final attempt; apply it with wx update --apply if it fails again",
			"version", record.LatestVersion, "attempt", attempt, "log", logPath)
	}
	m.log.Info("applying the update in a detached child", "version", record.LatestVersion, "attempt", attempt, "log", logPath)
	argv := []string{executable, "update", "--apply"}
	if err := hooks.spawn(argv, updateChildEnv(os.Environ(), filepath.Dir(executable)), logPath); err != nil {
		m.log.Error("automatic update could not be started", "version", record.LatestVersion, "error", err)
	}
}

// updateChildEnv は自動適用の子へ渡す環境変数を組み立てる。XPC_SERVICE_NAME を落とすのは、
// 子とその孫を launchd job へ紐付けたままにせず、子側の underLaunchd の誤判定も避けるためである。
// PATH の先頭へ binDir を置くのは、install.sh と wx の ResolveBinary が PATH 上の wx を先に見るためである。
func updateChildEnv(environ []string, binDir string) []string {
	out := make([]string, 0, len(environ)+1)
	path := ""
	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			out = append(out, entry)
			continue
		}
		switch name {
		case "XPC_SERVICE_NAME":
			continue
		case "PATH":
			path = value
		default:
			out = append(out, entry)
		}
	}
	parts := []string{binDir}
	if path != "" {
		parts = append(parts, path)
	}
	parts = append(parts, updateApplySystemPath)
	return append(out, "PATH="+strings.Join(parts, ":"))
}
