package main

import (
	"reflect"
	"testing"
)

func TestResumeFlagPrefixPreservesAgentArguments(t *testing.T) {
	for _, tc := range []struct{ args, wx, agent []string }{
		{[]string{"--model", "o3", "prompt"}, []string{}, []string{"--model", "o3", "prompt"}},
		{[]string{"--fresh", "--branch", "main", "--model", "o3"}, []string{"--fresh", "--branch", "main"}, []string{"--model", "o3"}},
		{[]string{"--fresh=false", "--branch=main"}, []string{"--fresh=false", "--branch=main"}, nil},
		{[]string{"--branch"}, []string{"--branch"}, nil},
		{[]string{"--", "--fresh"}, []string{}, []string{"--fresh"}},
		{[]string{"--fork-session", "--fresh"}, []string{}, []string{"--fork-session", "--fresh"}},
	} {
		wx, agent := resumeFlagPrefix(tc.args)
		if !reflect.DeepEqual(wx, tc.wx) || !reflect.DeepEqual(agent, tc.agent) {
			t.Fatalf("%v: wx=%v agent=%v", tc.args, wx, agent)
		}
	}
}
