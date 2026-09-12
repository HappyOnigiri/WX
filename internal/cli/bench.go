package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/daemon"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// benchIdleTimeout は前の run が残した保存・削除・補充の job が引くのを待つ上限である。
// 上限に達しても測定は続ける。並走した job は結果を遅くするだけで、無効にはしないためである。
const benchIdleTimeout = 5 * time.Minute

// benchIdlePoll は job の掃けるのを待つ間隔である。
const benchIdlePoll = 500 * time.Millisecond

// benchUsageTimeout は準備した slot の使用量が載るのを待つ上限である。
// daemon は準備直後にその slot だけを background で測るため、返却前に少しだけ待つ必要がある。
// 上限内に載らなかった回は時間だけの行として出す。測れなかったのは観測側の遅れで、測った時間は有効である。
const benchUsageTimeout = 90 * time.Second

// benchUsagePoll は使用量の測定が載るのを待つ間隔である。
const benchUsagePoll = 500 * time.Millisecond

// BenchOptions は `wx bench` の測定条件である。
// Configs は比較する準備設定で、空なら現在の実効設定だけで測る（従来の1通り）。
type BenchOptions struct {
	Runs     int
	Branches []string
	Reuse    bool
	JSON     bool
	Configs  []config.PrepareOverride
}

// BenchRun は1回分の実測結果である。source は貸出が cold start だったかを表す。
// Config はこの回に適用した準備設定で、Usage は返却前に引いた slot の使用量である。
type BenchRun struct {
	Source         string                     `json:"source"`
	Config         BenchConfig                `json:"config"`
	SessionID      string                     `json:"session_id"`
	Path           string                     `json:"path"`
	RetiredStandby int                        `json:"retired_standby"`
	LeaseMS        int64                      `json:"lease_ms"`
	EarlyReadyMS   int64                      `json:"early_ready_ms"`
	FullReadyMS    int64                      `json:"full_ready_ms"`
	Measurement    *daemon.PrepareMeasurement `json:"measurement,omitempty"`
	Usage          *BenchUsage                `json:"usage,omitempty"`
	Error          string                     `json:"error,omitempty"`
}

// benchReply は `wx bench --json` の出力である。Configs は設定ごとの集計で、表の比較行と同じ値を持つ。
type benchReply struct {
	Workspace string               `json:"workspace"`
	Runs      []BenchRun           `json:"runs"`
	Configs   []BenchConfigSummary `json:"configs"`
}

// RunBench は貸出から Early Ready・Full Ready までを実測し、daemon 側の区間内訳と併せて出力する。
// 既定では対象 workspace の待機中 standby を STALE にして cold start を測る。reuse では今のプールが返す経路をそのまま測る。
func (c Client) RunBench(ctx context.Context, opts BenchOptions) int {
	cwd, err := os.Getwd()
	if err != nil {
		cliError(c, err)
		return 1
	}
	return c.RunBenchFrom(ctx, cwd, opts)
}

// RunBenchFrom は TUI が明示した workspace を測り、process 全体の cwd に依存しない。
func (c Client) RunBenchFrom(ctx context.Context, cwd string, opts BenchOptions) int {
	if opts.Runs < 1 {
		lang := cliLanguage(c)
		fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage("--runs must be at least 1", lang))
		return 2
	}
	// 設定を振る測定は cold start を前提とするため、プールが返すものを測る --reuse とは両立しない。
	if opts.Reuse && len(opts.Configs) > 0 {
		lang := cliLanguage(c)
		fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage("--config and --sweep cannot be combined with --reuse; each configuration is measured as a cold start", lang))
		return 2
	}
	if err := c.checkLeaseWorktreeModeFrom(ctx, cwd); err != nil {
		return reportLeaseErrorLanguage(err, cliLanguage(c))
	}
	if err := c.ensureDaemon(ctx); err != nil {
		cliError(c, err)
		return 1
	}
	// standby を退役させる要求は workspace root で宛先を指すため、cold start の測定だけが root の解決を要する。
	root, resolved := c.leasePolicyRoot(ctx, cwd)
	if !resolved && !opts.Reuse {
		lang := cliLanguage(c)
		message := "cannot resolve a wx workspace from " + cwd + "; run wx bench --reuse to measure without retiring standby worktrees"
		fmt.Fprintln(os.Stderr, cliErrorPrefix(lang), localizeCLIMessage(message, lang))
		return 2
	}
	reply, failed := c.benchRuns(ctx, cwd, root, opts)
	if opts.JSON {
		data, err := json.Marshal(reply)
		if err != nil {
			cliError(c, err)
			return 1
		}
		fmt.Println(string(data))
	} else {
		if len(reply.Runs) > 1 {
			printBenchSummaryLanguage(reply.Runs, cliLanguage(c))
		}
		printBenchConfigsLanguage(reply.Configs, cliLanguage(c))
	}
	if failed {
		return 1
	}
	return 0
}

// benchRuns は設定 × --runs 回の測定を回して集計する。
// 設定ごとに固めず設定を1巡ずつ回すのは、測定中の機械の状態の移り変わりを設定間で均すためである。
func (c Client) benchRuns(ctx context.Context, cwd, root string, opts BenchOptions) (benchReply, bool) {
	reply := benchReply{Workspace: root}
	overrides := opts.Configs
	if len(overrides) == 0 {
		overrides = []config.PrepareOverride{{}}
	}
	failed, measured := false, 0
	total := opts.Runs * len(overrides)
	for range opts.Runs {
		for _, override := range overrides {
			if measured > 0 {
				c.waitBenchIdle(ctx)
			}
			measured++
			run := c.benchOnceForOutput(ctx, cwd, root, opts.Branches, opts.Reuse, override, !opts.JSON)
			reply.Runs = append(reply.Runs, run)
			if run.Error != "" {
				failed = true
			}
			if !opts.JSON {
				printBenchRunLanguage(measured, total, run, cliLanguage(c))
			}
		}
	}
	reply.Configs = summarizeBenchConfigs(reply.Runs)
	return reply, failed
}

// benchOnce は1回の貸出を測って返却する。失敗した回も、そこまでに測れた区間を結果に残す。
// override はこの貸出の準備にだけ適用する設定で、設定ファイルと daemon の実効設定はどちらも変えない。
func (c Client) benchOnce(ctx context.Context, cwd, root string, branches []string, reuse bool, override config.PrepareOverride) BenchRun {
	return c.benchOnceForOutput(ctx, cwd, root, branches, reuse, override, true)
}

// benchOnceForOutput は JSON 経路では表示言語を要求へ載せず、機械向けの
// JSON 契約と旧 daemon への payload を英語のまま保つ。
func (c Client) benchOnceForOutput(ctx context.Context, cwd, root string, branches []string, reuse bool, override config.PrepareOverride, includeLanguage bool) BenchRun {
	run := BenchRun{Source: "cold", Config: benchConfigOf(override)}
	if !reuse {
		retired, err := c.retireStandby(ctx, root)
		if err != nil {
			run.Error = "retire standby: " + err.Error()
			return run
		}
		run.RetiredStandby = retired
	}
	// 測定の貸出は wx new と同じ path 貸出にする。heartbeat を張らないので、
	// 中断で defer の返却を逃した回は親 session の終了か lease.ttl で返る。
	ownerID, ownerToken := leaseOwnerFromEnvironment()
	language := ""
	if includeLanguage {
		language = c.Config.LanguageForRPC()
	}
	params := rpc.ResolveAndLeaseParams{
		Agent: leaseAgentKindPath, Branches: branches, ClientPID: 0, CWD: cwd, ForceWorktree: c.forceWorktree,
		LeaseKind: state.LeaseKindPath, LeaseOwnerSessionID: ownerID, LeaseOwnerToken: ownerToken,
		PrepareCopyMode: override.CopyMode, PrepareCOWMinSizeKiB: override.COWMinSizeKiB,
		Language: language,
	}
	started := time.Now()
	leaseCtx, cancelLease := context.WithTimeout(ctx, c.discoveryTimeout())
	defer cancelLease()
	var lease daemon.Lease
	if err := c.RPC.Call(leaseCtx, "ResolveAndLease", params, &lease); err != nil {
		run.Error = "lease: " + err.Error()
		return run
	}
	run.SessionID, run.Path = lease.SessionID, lease.Path
	run.LeaseMS = time.Since(started).Milliseconds()
	defer c.releaseBenchLease(lease)
	if lease.Ready {
		// 準備済み slot をそのまま受け取った回は準備区間を持たない。cold start の比較対象にはならない。
		run.Source = "warm"
		run.EarlyReadyMS, run.FullReadyMS = run.LeaseMS, run.LeaseMS
		return run
	}
	if err := c.waitBenchReadiness(ctx, lease, "WaitEarlyReady"); err != nil {
		run.Error = "early ready: " + err.Error()
		run.Measurement = c.prepareMeasurement(ctx, lease.SessionID)
		return run
	}
	run.EarlyReadyMS = time.Since(started).Milliseconds()
	if err := c.waitBenchReadiness(ctx, lease, "WaitReady"); err != nil {
		run.Error = "full ready: " + err.Error()
		run.Measurement = c.prepareMeasurement(ctx, lease.SessionID)
		return run
	}
	run.FullReadyMS = time.Since(started).Milliseconds()
	run.Measurement = c.prepareMeasurement(ctx, lease.SessionID)
	// 使用量は返却の前に引く。discard で返した slot は `wx slots` に現れず、後から辿る経路がない。
	run.Usage = c.benchSlotUsage(ctx, lease.SessionID, started)
	return run
}

// benchSlotUsage は測り終えた slot の使用量が載るのを待って返す。
// 上限内に載らなかった場合と daemon から引けなかった場合は nil を返し、その回は時間だけの行になる。
// notBefore はこの回の貸出を要求した時刻で、それより古い測定は前の準備の値なので待ち続ける。
func (c Client) benchSlotUsage(ctx context.Context, sessionID string, notBefore time.Time) *BenchUsage {
	deadline := time.Now().Add(benchUsageTimeout)
	for {
		usage, err := c.benchSlotUsageOnce(ctx, sessionID, notBefore)
		if err != nil || (usage == nil && time.Now().After(deadline)) {
			return nil
		}
		if usage != nil {
			return usage
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(benchUsagePoll):
		}
	}
}

// benchSlotUsageOnce は slot 一覧から対象 session の行を探す。
// まだ測定が載っていない場合と、載っているのが notBefore より前の測定である場合は nil を返す。
func (c Client) benchSlotUsageOnce(ctx context.Context, sessionID string, notBefore time.Time) (*BenchUsage, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var slots []daemon.SlotView
	if err := c.RPC.Call(callCtx, "Slots", map[string]any{"all": false}, &slots); err != nil {
		return nil, err
	}
	for _, slot := range slots {
		if slot.SessionID != sessionID || !benchUsageMeasuredAfter(slot.MeasuredAt, notBefore) {
			continue
		}
		return &BenchUsage{
			Measurement: slot.Measurement, CopyMode: slot.CopyMode, Files: slot.Files,
			AllocatedBytes: slot.AllocatedBytes, SharedBytes: slot.SharedBytes, ExclusiveBytes: slot.ExclusiveBytes,
			MeasuredAt: slot.MeasuredAt,
		}, nil
	}
	return nil, nil
}

// benchUsageMeasuredAfter はこの回の準備を測った結果かを時刻で見分ける。
// 使用量は slot が再び準備へ入っても消えないため、時刻を見ないと前の世代の値を今回の結果として採ってしまう。
// 読めない時刻は採らない。単一マシンなので daemon と client の時計は同じである。
func benchUsageMeasuredAfter(measuredAt string, notBefore time.Time) bool {
	if measuredAt == "" {
		return false
	}
	parsed, err := state.ParseTime(measuredAt)
	if err != nil {
		return false
	}
	return !parsed.Before(notBefore)
}

func (c Client) waitBenchReadiness(ctx context.Context, lease daemon.Lease, method string) error {
	timeout := c.Config.Readiness.Timeout.Duration
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	params := map[string]any{"session_id": lease.SessionID, "token": lease.Token, "timeout_ms": int(timeout.Milliseconds())}
	return c.RPC.Call(waitCtx, method, params, nil)
}

// retireStandby は対象 workspace の待機中 standby を STALE にし、次の貸出を cold start にする。
func (c Client) retireStandby(ctx context.Context, root string) (int, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply struct {
		Retired []string `json:"retired"`
	}
	if err := c.RPC.Call(callCtx, "RetireStandby", map[string]any{"path": root}, &reply); err != nil {
		return 0, err
	}
	return len(reply.Retired), nil
}

// prepareMeasurement は daemon が保持する区間内訳を引く。取得できない場合は内訳なしで測定を続ける。
func (c Client) prepareMeasurement(ctx context.Context, sessionID string) *daemon.PrepareMeasurement {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reply struct {
		Measurements []daemon.PrepareMeasurement `json:"measurements"`
	}
	if err := c.RPC.Call(callCtx, "PrepareTimings", map[string]any{"session_id": sessionID}, &reply); err != nil {
		return nil
	}
	if len(reply.Measurements) == 0 {
		return nil
	}
	return &reply.Measurements[0]
}

// releaseBenchLease は測り終えた貸出を保存せずに返す。計測用の worktree を retention 分残さないためである。
func (c Client) releaseBenchLease(lease daemon.Lease) {
	if lease.SessionID == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	params := map[string]any{"session_id": lease.SessionID, "reason": "wx-bench", "discard": true}
	if err := c.RPC.Call(releaseCtx, "ReleaseLease", params, nil); err != nil {
		lang := cliLanguage(c)
		if lang == i18n.Japanese {
			fmt.Fprintln(os.Stderr, "警告: bench の貸出 "+lease.SessionID+" の返却に失敗:", err)
		} else {
			fmt.Fprintln(os.Stderr, "warning: release bench lease "+lease.SessionID+":", err)
		}
	}
}

// waitBenchIdle は前の run が残した job が掃けるまで待つ。
// 返却の保存・削除と standby 補充が次の run と並走すると、測るのが準備の重さではなくなるためである。
func (c Client) waitBenchIdle(ctx context.Context) {
	deadline := time.Now().Add(benchIdleTimeout)
	for time.Now().Before(deadline) {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var status struct {
			JobDetails struct {
				Pending int `json:"pending"`
				Running int `json:"running"`
			} `json:"job_details"`
		}
		err := c.RPC.Call(callCtx, "Status", nil, &status)
		cancel()
		if err != nil {
			return
		}
		if status.JobDetails.Pending == 0 && status.JobDetails.Running == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(benchIdlePoll):
		}
	}
}

func printBenchRun(index, runs int, run BenchRun) {
	printBenchRunLanguage(index, runs, run, i18n.English)
}

func printBenchRunLanguage(index, runs int, run BenchRun, lang i18n.Language) {
	runLabel := "run"
	if lang == i18n.Japanese {
		runLabel = "測定"
	}
	fmt.Printf("%s %d/%d  %s  %s\n", runLabel, index, runs, run.Source, run.Config.Label)
	if run.Error != "" {
		fmt.Println("  " + cliErrorPrefix(lang) + " " + localizeCLIMessage(run.Error, lang))
	}
	leaseLabel, retiredLabel, earlyLabel, fullLabel, usageLabel, prepareLabel := "lease", "retired", "EARLY READY", "FULL READY", "slot usage", "prepare job"
	if lang == i18n.Japanese {
		leaseLabel, retiredLabel, earlyLabel, fullLabel, usageLabel, prepareLabel = "貸出", "退役", "早期準備完了", "準備完了", "slot 使用量", "準備 job"
	}
	fmt.Printf("  %-13s %s\n", leaseLabel, formatBenchDuration(run.LeaseMS))
	if run.Source == "cold" && run.RetiredStandby > 0 {
		slotsLabel := "standby slot(s)"
		if lang == i18n.Japanese {
			slotsLabel = "standby slot"
		}
		fmt.Printf("  %-13s %d %s\n", retiredLabel, run.RetiredStandby, slotsLabel)
	}
	fmt.Printf("  %-13s %s\n", earlyLabel, formatBenchDuration(run.EarlyReadyMS))
	fmt.Printf("  %-13s %s\n", fullLabel, formatBenchDuration(run.FullReadyMS))
	if run.Usage != nil {
		exclusiveLabel, sharedLabel := "exclusive", "shared"
		if lang == i18n.Japanese {
			exclusiveLabel, sharedLabel = "専有", "共有"
		}
		fmt.Printf("  %-13s %s %s · %s %s (%s)\n", usageLabel,
			formatBenchBytes(run.Usage.ExclusiveBytes), exclusiveLabel, formatBenchBytes(run.Usage.SharedBytes), sharedLabel, run.Usage.Measurement)
	}
	if run.Measurement == nil {
		return
	}
	fmt.Printf("  %-13s %s (early %s)\n", prepareLabel, formatBenchDuration(run.Measurement.TotalMS), formatBenchDuration(run.Measurement.EarlyReadyMS))
	for _, phase := range run.Measurement.Phases {
		indent := "    "
		if strings.Contains(phase.Name, ".") {
			indent = "      "
		}
		// 時間を持たない区間は件数だけの記録なので、0秒を並べて時間の内訳と読み違えられないようにする。
		elapsed := formatBenchDuration(phase.MS)
		if phase.MS == 0 && phase.Count > 1 {
			elapsed = "-"
		}
		fmt.Printf("%s%-24s %9s  x%d\n", indent, phase.Name, elapsed, phase.Count)
	}
}

// printBenchSummary は複数 run の中央値と最小・最大を出す。1回の実測はキャッシュ状態に強く左右されるためである。
// 失敗が続いて成功が1回だけになった場合は、同じ値を3つ並べず実測値だけを出す。
func printBenchSummary(runs []BenchRun) {
	printBenchSummaryLanguage(runs, i18n.English)
}

func printBenchSummaryLanguage(runs []BenchRun, lang i18n.Language) {
	early, full := []int64{}, []int64{}
	for _, run := range runs {
		if run.Error != "" {
			continue
		}
		early = append(early, run.EarlyReadyMS)
		full = append(full, run.FullReadyMS)
	}
	if len(full) == 0 {
		return
	}
	if lang == i18n.Japanese {
		fmt.Printf("成功した測定 %d 件の概要\n", len(full))
		fmt.Printf("  早期準備完了  %s\n", formatBenchDistribution(early))
		fmt.Printf("  準備完了      %s\n", formatBenchDistribution(full))
		return
	}
	fmt.Printf("summary of %d successful run(s)\n", len(full))
	fmt.Printf("  EARLY READY   %s\n", formatBenchDistribution(early))
	fmt.Printf("  FULL READY    %s\n", formatBenchDistribution(full))
}

// formatBenchDistribution は分布を1行で書く。成功が1回だけなら分布ではないので、実測値をそのまま出す。
func formatBenchDistribution(values []int64) string {
	if len(values) == 1 {
		return formatBenchDuration(values[0])
	}
	return fmt.Sprintf("min %s  median %s  max %s", formatBenchDuration(minOf(values)), formatBenchDuration(medianOf(values)), formatBenchDuration(maxOf(values)))
}

func formatBenchDuration(ms int64) string {
	return fmt.Sprintf("%.3fs", float64(ms)/1000)
}

func minOf(values []int64) int64 {
	sorted := sortedCopy(values)
	return sorted[0]
}

func maxOf(values []int64) int64 {
	sorted := sortedCopy(values)
	return sorted[len(sorted)-1]
}

func medianOf(values []int64) int64 {
	sorted := sortedCopy(values)
	return sorted[len(sorted)/2]
}

func sortedCopy(values []int64) []int64 {
	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	return sorted
}
