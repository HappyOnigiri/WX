package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WX/internal/diag"
	"github.com/HappyOnigiri/WX/internal/rpc"
	"github.com/HappyOnigiri/WX/internal/state"
)

// statusDisplayTimeout は Status/Doctor の制限時間。
// worktree root のディスク使用量も調べるため、大きな root では RPC の既定値を超え得る。
// status/doctor は失敗時に kickstart せず、エラーを報告するだけなので daemon は停止しない。
const statusDisplayTimeout = 40 * time.Second

func runRPCDisplay(ctx context.Context, method string, args []string) int {
	// doctor は接続エラー時に独自のフォールバックを持つため、互換用の経路として残す。
	if strings.EqualFold(method, "Doctor") {
		return runDoctor(ctx, args)
	}
	name := strings.ToLower(method)
	fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	var verbose *bool
	if strings.EqualFold(method, "Status") {
		verbose = fs.BoolP("verbose", "v", false, "show detailed status")
	}
	fs.Usage = func() { commandUsage(os.Stdout, name) }
	if code, done := finishFlagParse(fs, name, args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, name)
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, statusDisplayTimeout)
		defer cancel()
	}
	var out map[string]any
	if err := c.Call(ctx, method, struct{}{}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	switch {
	case *jsonOut:
		fmt.Println(string(data))
	case verbose != nil:
		printStatusDisplay(os.Stdout, out, *verbose)
	default:
		printDisplay(os.Stdout, out)
	}
	return 0
}

// runDoctor は汎用 RPC 表示処理と分ける。
// socket に応答する daemon がなくてもローカルの事実を報告するが、接続済み daemon の要求失敗にはフォールバックしない。
func runDoctor(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("doctor", pflag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "doctor") }
	if code, done := finishFlagParse(fs, "doctor", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "doctor")
		return 2
	}
	c, err := rpcClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, statusDisplayTimeout)
		defer cancel()
	}
	var out map[string]any
	if err := c.Call(ctx, "Doctor", struct{}{}, &out); err != nil {
		if !rpc.IsConnectError(err) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		out = map[string]any{
			"schema_version":    state.JSONSchemaVersion,
			"db_schema_version": state.SchemaVersion,
			"checks":            diag.LocalChecks(ctx, err),
		}
		data, _ := json.MarshalIndent(out, "", "  ")
		if *jsonOut {
			fmt.Println(string(data))
		} else {
			printDisplay(os.Stdout, out)
		}
		// ローカルの報告だけでは daemon の健全性を確認できないため、従来の失敗終了コードを保つ。
		return 1
	}
	data, _ := json.MarshalIndent(out, "", "  ")
	if *jsonOut {
		fmt.Println(string(data))
	} else {
		printDisplay(os.Stdout, out)
	}
	return 0
}
