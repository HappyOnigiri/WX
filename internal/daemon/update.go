package daemon

import (
	"context"
	"time"

	"github.com/HappyOnigiri/WX/internal/update"
)

// updateCheckInterval は最新リリースを問い合わせる間隔である。
// 保守の一巡（Discovery.ReconcileInterval）に相乗りして間引くので、実際の刻みはその周期以上になる。
const updateCheckInterval = 6 * time.Hour

// updateCheckDeadline は1回の問い合わせに与える総時間である。保守の一巡を待たせないよう短く取る。
const updateCheckDeadline = 15 * time.Second

// UpdateStatus は daemon が知っている更新の有無で、CLI と TUI はこれを読むだけにする。
// 手元のバイナリの版は呼び出し側が自分で持つため、ここには含めない。
type UpdateStatus struct {
	// LatestVersion は最後に成功した確認で見えた公開済みの最新タグである。未確認なら空になる。
	LatestVersion string `json:"latest_version"`
	// ReleaseURL は LatestVersion の人間向け page である。
	ReleaseURL string `json:"release_url"`
	// Available は daemon 自身の版より LatestVersion が新しいかどうかである。
	Available bool `json:"available"`
	// Announce は、要求した呼び出しがこの版の案内権を取れたかどうかである。版ごとに1回だけ true になる。
	Announce bool `json:"announce"`
}

// updateProbe は更新確認が触る外部の値をまとめた差し替え点である。
// production では Manager が nil を持ち、resolveUpdateProbe が実装を埋める。
type updateProbe struct {
	// releaseBuild は配布用ビルドかどうかを返す。
	releaseBuild func() bool
	// current は手元のバイナリの表示版を返す。
	current func() string
	// latest は公開済みの最新リリースを問い合わせる。
	latest func(context.Context) (update.Release, error)
}

// resolveUpdateProbe は差し替えのない項目を production の実装で埋めて返す。
func (m *Manager) resolveUpdateProbe() updateProbe {
	m.mu.RLock()
	injected := m.updateProbe
	m.mu.RUnlock()
	probe := updateProbe{releaseBuild: update.ReleaseBuild, current: update.CurrentVersion, latest: update.Checker{}.Latest}
	if injected == nil {
		return probe
	}
	if injected.releaseBuild != nil {
		probe.releaseBuild = injected.releaseBuild
	}
	if injected.current != nil {
		probe.current = injected.current
	}
	if injected.latest != nil {
		probe.latest = injected.latest
	}
	return probe
}

// updateCheckEnabled は自動確認の前提が揃っているかを返す。
// 開発ビルドでは確認も更新も行わない。埋め込み版が vX.Y.Z ではないため比較できず、
// install.sh が置き換える先も開発用の配置とは限らないためである。
func (m *Manager) updateCheckEnabled(probe updateProbe) bool {
	return probe.releaseBuild() && m.Config().Update.AutoCheck
}

// maybeCheckUpdate は保守の一巡に相乗りして更新確認を行い、間引きと記録だけを担う。
// 失敗も「確認した」とみなして最終確認時刻を進める。進めないと、オフラインが続く間は
// 一巡ごとに GitHub を叩き、未認証のレート制限へ触れるためである。
func (m *Manager) maybeCheckUpdate(ctx context.Context) {
	probe := m.resolveUpdateProbe()
	if !m.updateCheckEnabled(probe) {
		return
	}
	record, err := m.store.UpdateCheck(ctx)
	if err != nil {
		m.log.Error("update check state is unreadable", "error", err)
		return
	}
	if updateCheckIsFresh(record.CheckedAt, time.Now()) {
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckDeadline)
	defer cancel()
	release, checkErr := probe.latest(checkCtx)
	if checkErr != nil {
		// 失敗は記録するだけでは誰も読まない。オフラインやレート制限で確認が止まり続けても
		// 案内が黙って出なくなるだけなので、調査の手掛かりを log に残す。
		m.log.Warn("update check failed", "error", checkErr)
		if recordErr := m.store.RecordUpdateCheck(ctx, "", "", checkErr.Error()); recordErr != nil {
			m.log.Error("update check result could not be recorded", "error", recordErr)
		}
		return
	}
	if recordErr := m.store.RecordUpdateCheck(ctx, release.Tag, release.URL, ""); recordErr != nil {
		m.log.Error("update check result could not be recorded", "error", recordErr)
	}
}

// updateCheckIsFresh はちょうど期限へ達した確認を古いものとして扱う。
func updateCheckIsFresh(checkedAt, now time.Time) bool {
	return !checkedAt.IsZero() && now.Sub(checkedAt) < updateCheckInterval
}

// UpdateState は記録済みの確認結果を返す。claim が true の呼び出しだけが案内権を要求し、
// 取れたときに Announce を true にする。TUI と wx update は常時表示・明示実行なので claim しない。
func (m *Manager) UpdateState(ctx context.Context, claim bool) (UpdateStatus, error) {
	probe := m.resolveUpdateProbe()
	enabled := m.updateCheckEnabled(probe)
	var status UpdateStatus
	record, err := m.store.UpdateCheck(ctx)
	if err != nil {
		return UpdateStatus{}, err
	}
	status.LatestVersion, status.ReleaseURL = record.LatestVersion, record.ReleaseURL
	status.Available = update.Newer(probe.current(), record.LatestVersion)
	if !claim || !status.Available || !enabled {
		return status, nil
	}
	claimed, err := m.store.ClaimUpdateAnnouncement(ctx, record.LatestVersion)
	if err != nil {
		return UpdateStatus{}, err
	}
	status.Announce = claimed
	return status, nil
}
