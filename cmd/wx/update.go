package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/update"
)

// applyDeadline は install.sh の実行に与える総時間である。
// ダウンロード・checksum 検証・daemon の停止待ちまでを含むため、確認そのものより長く取る。
const applyDeadline = 15 * time.Minute

// installScriptLimit は install.sh の読み取り上限である。配布している script は数十 KiB に収まる。
const installScriptLimit = 1 << 20

// updateAdapters は update コマンドが触る外部をまとめた差し替え点である。
// これを通さないと、確認・適用の各経路は実網と実 process なしには一度も通らない。
type updateAdapters struct {
	// releaseBuild は配布用ビルドかどうかを返す。
	releaseBuild func() bool
	// current は手元のバイナリの表示版を返す。
	current func() string
	// latest は公開済みの最新リリースを問い合わせる。
	latest func(context.Context) (update.Release, error)
	// supported は install.sh が扱える環境かどうかを返す。
	supported func() bool
	// fetchScript は指定タグの install.sh を読む。
	fetchScript func(ctx context.Context, tag string) ([]byte, error)
	// runScript は書き出した install.sh を実行する。
	runScript func(ctx context.Context, path string) error
	stdout    io.Writer
	stderr    io.Writer
}

// defaultUpdateAdapters は production の実装を返す。
func defaultUpdateAdapters() updateAdapters {
	return updateAdapters{
		releaseBuild: update.ReleaseBuild,
		current:      versionString,
		latest:       update.Checker{}.Latest,
		// install.sh は macOS arm64 以外を前提条件の失敗として落とす。読みにくい失敗にせず、先に断る。
		supported:   func() bool { return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" },
		fetchScript: fetchInstallScript,
		runScript:   runInstallScript,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	}
}

// updateCommand は runUpdate が使う実装である。test だけが差し替える。
var updateCommand = defaultUpdateAdapters()

func runUpdate(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	adapters := updateCommand
	fs := pflag.NewFlagSet("update", pflag.ContinueOnError)
	apply := fs.Bool("apply", false, "install the latest release")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "update", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "update", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "update", i18n.LanguageFromContext(ctx))
		return 2
	}
	r := newTextRenderer(adapters.stdout, i18n.LanguageFromContext(ctx))
	// 開発ビルドは埋め込み版が vX.Y.Z ではなく比較できず、install.sh の置き換え先とも一致しない。
	if !adapters.releaseBuild() {
		r.line("wx.update.development", map[string]any{"Version": adapters.current()})
		return 0
	}
	// 明示的な実行なので daemon のキャッシュではなく、その場で取得する。
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	release, err := adapters.latest(checkCtx)
	if err != nil {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	current := adapters.current()
	if !update.Newer(current, release.Tag) {
		r.line("wx.update.current", map[string]any{"Version": current})
		return 0
	}
	if !*apply {
		r.line("wx.update.available", map[string]any{"Current": current, "Latest": release.Tag})
		r.raw(release.URL)
		r.line("wx.update.apply_hint", nil)
		return 0
	}
	return applyUpdate(ctx, adapters, r, release)
}

// updateCheckTimeout は `wx update` が最新リリースの問い合わせを待つ上限である。
const updateCheckTimeout = 20 * time.Second

// installScriptTimeout は install.sh のダウンロードを待つ上限である。
// 確認の問い合わせとは取得するものが違うので、updateCheckTimeout とは別に持つ。
const installScriptTimeout = 20 * time.Second

// applyUpdate は最新タグの install.sh を取り直して実行する。
// checksum 検証・版番号の照合・atomic な置換・daemon の再起動は install.sh が持つため、ここでは再実装しない。
func applyUpdate(ctx context.Context, adapters updateAdapters, r *textRenderer, release update.Release) int {
	if !adapters.supported() {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", i18n.T(ctx, "wx.update.unsupported_platform", nil))
		return 1
	}
	script, err := adapters.fetchScript(ctx, release.Tag)
	if err != nil {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "wx-update-")
	if err != nil {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	r.line("wx.update.running", map[string]any{"Latest": release.Tag})
	if err := adapters.runScript(ctx, path); err != nil {
		fmt.Fprintln(adapters.stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	return 0
}

// runInstallScript は書き出した install.sh を bash で実行する。
func runInstallScript(ctx context.Context, path string) error {
	runCtx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	command := exec.CommandContext(runCtx, "bash", path)
	// install.sh 側の端末判定を働かせるため、標準入出力はそのまま渡す。
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

// fetchInstallScript は指定タグに添付された install.sh を読む。
func fetchInstallScript(ctx context.Context, tag string) ([]byte, error) {
	return fetchScriptFrom(ctx, update.InstallScriptURL(tag))
}

// fetchScriptFrom は install.sh を1本読む。取得先を引数に取り、上限と状態行の扱いを test から通せるようにする。
func fetchScriptFrom(ctx context.Context, endpoint string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: installScriptTimeout}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("install.sh download failed: %s", response.Status)
	}
	// 上限より1 byte 多く読み、切り詰めた script をそのまま bash へ渡さない。
	// 切り詰めを黙って返すと、失敗が内容の破損ではなく bash の構文 error としてしか見えない。
	script, err := io.ReadAll(io.LimitReader(response.Body, installScriptLimit+1))
	if err != nil {
		return nil, err
	}
	if len(script) > installScriptLimit {
		return nil, fmt.Errorf("install.sh is larger than %d bytes", installScriptLimit)
	}
	return script, nil
}
