package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/HappyOnigiri/WX/internal/config"
	"github.com/HappyOnigiri/WX/internal/i18n"
	"github.com/HappyOnigiri/WX/internal/rpc"
)

func rpcClient() (rpc.Client, error) {
	socket, err := config.SocketPath()
	return rpc.Client{Socket: socket, Timeout: 5 * time.Second}, err
}

// daemonUnavailableMessage は daemon が待受していないときの案内で、daemon へ RPC する全コマンドで共有する。
// socket path と dial の syscall error を見せても利用者は次の操作を選べないため、状態と再試行の指示だけを示す。
const daemonUnavailableMessage = "wx daemon is not running or still starting; try again shortly"

// rpcErrorMessage は RPC の失敗を利用者向けの1行にする。
// 接続確立の失敗（daemon 未待受）だけを共通の案内に置き換え、接続後の失敗は原因をそのまま残す。
func rpcErrorMessage(err error) string {
	return rpcErrorMessageLanguage(err, i18n.English)
}

func rpcErrorMessageLanguage(err error, lang i18n.Language) string {
	if rpc.IsConnectError(err) {
		return i18n.New(string(lang)).Localize("rpc.daemon_unavailable", nil)
	}
	return err.Error()
}

// reportRPCError は RPC の失敗を stderr へ "error: " 付きで書く。
func reportRPCError(err error) {
	fmt.Fprintln(os.Stderr, "error:", rpcErrorMessage(err))
}

func reportRPCErrorContext(ctx context.Context, err error) {
	lang := i18n.LanguageFromContext(ctx)
	fmt.Fprintln(os.Stderr, i18n.New(string(lang)).Localize("common.error", nil)+":", rpcErrorMessageLanguage(err, lang))
}
