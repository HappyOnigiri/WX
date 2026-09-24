package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/pflag"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
)

func runSlots(ctx context.Context, args []string) int {
	ctx = commandContext(ctx)
	fs := pflag.NewFlagSet("slots", pflag.ContinueOnError)
	all := fs.Bool("all", false, "also list sessions that no longer hold a slot")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsageLanguage(os.Stdout, "slots", i18n.LanguageFromContext(ctx)) }
	if code, done := finishFlagParse(fs, "slots", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsageLanguage(os.Stderr, "slots", i18n.LanguageFromContext(ctx))
		return 2
	}
	c, _ := rpcClient()
	var out []map[string]any
	if err := c.Call(ctx, "Slots", map[string]bool{"all": *all}, &out); err != nil {
		reportRPCErrorContext(ctx, err)
		return 1
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	rows := make([][]string, 0, len(out))
	for _, s := range out {
		rows = append(rows, []string{
			slotField(s, "slot_id"), slotState(s), slotRepositories(s), slotField(s, "session_id"), slotField(s, "agent"),
			slotCopyMode(s), slotSizeMB(s), slotField(s, "path"),
		})
	}
	// 見出しは全て大文字の機械的な列名で、訳語を持たない。表の値も payload の原文をそのまま出す。
	printSlotTable(os.Stdout, rows)
	return 0
}
