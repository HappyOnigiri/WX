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

func runUpdate(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
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
	r := newTextRenderer(os.Stdout, i18n.LanguageFromContext(ctx))
	// 開発ビルドは埋め込み版が vX.Y.Z ではなく比較できず、install.sh の置き換え先とも一致しない。
	if !update.ReleaseBuild() {
		r.line("wx.update.development", map[string]any{"Version": versionString()})
		return 0
	}
	// 明示的な実行なので daemon のキャッシュではなく、その場で取得する。
	checkCtx, cancel := context.WithTimeout(ctx, updateCheckTimeout)
	defer cancel()
	release, err := update.Checker{}.Latest(checkCtx)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	current := versionString()
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
	return applyUpdate(ctx, r, release)
}

// updateCheckTimeout は `wx update` が最新リリースの問い合わせを待つ上限である。
const updateCheckTimeout = 20 * time.Second

// installScriptTimeout は install.sh のダウンロードを待つ上限である。
// 確認の問い合わせとは取得するものが違うので、updateCheckTimeout とは別に持つ。
const installScriptTimeout = 20 * time.Second

// applyUpdate は最新タグの install.sh を取り直して実行する。
// checksum 検証・版番号の照合・atomic な置換・daemon の再起動は install.sh が持つため、ここでは再実装しない。
func applyUpdate(ctx context.Context, r *textRenderer, release update.Release) int {
	// install.sh は macOS arm64 以外を前提条件の失敗として落とす。読みにくい失敗にせず、先にここで断る。
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", i18n.T(ctx, "wx.update.unsupported_platform", nil))
		return 1
	}
	script, err := fetchInstallScript(ctx, release.Tag)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "wx-update-")
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, script, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	r.line("wx.update.running", map[string]any{"Latest": release.Tag})
	runCtx, cancel := context.WithTimeout(ctx, applyDeadline)
	defer cancel()
	command := exec.CommandContext(runCtx, "bash", path)
	// install.sh 側の端末判定を働かせるため、標準入出力はそのまま渡す。
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		fmt.Fprintln(os.Stderr, i18n.T(ctx, "common.error", nil)+":", err)
		return 1
	}
	return 0
}

// fetchInstallScript は指定タグに添付された install.sh を読む。
func fetchInstallScript(ctx context.Context, tag string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, update.InstallScriptURL(tag), nil)
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
	return io.ReadAll(io.LimitReader(response.Body, installScriptLimit))
}
