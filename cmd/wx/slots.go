package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/pflag"
)

func runSlots(ctx context.Context, args []string) int {
	fs := pflag.NewFlagSet("slots", pflag.ContinueOnError)
	all := fs.Bool("all", false, "include failed, quarantined, and released slots")
	jsonOut := fs.Bool("json", false, "print JSON")
	fs.Usage = func() { commandUsage(os.Stdout, "slots") }
	if code, done := finishFlagParse(fs, "slots", args); done {
		return code
	}
	if fs.NArg() != 0 {
		commandUsage(os.Stderr, "slots")
		return 2
	}
	c, _ := rpcClient()
	var out []map[string]any
	if err := c.Call(ctx, "Slots", map[string]bool{"all": *all}, &out); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if *jsonOut {
		data, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(data))
		return 0
	}
	fmt.Printf(slotRowFormat, "SLOT", "STATE", "SESSION", "AGENT", "COPY", "SIZE(MB)", "PATH")
	for _, s := range out {
		fmt.Printf(slotRowFormat,
			slotField(s, "slot_id"), slotField(s, "state"), slotField(s, "session_id"), slotField(s, "agent"),
			slotCopyMode(s), slotSizeMB(s), slotField(s, "path"))
	}
	return 0
}
