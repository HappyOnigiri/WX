package cli

import (
	"reflect"
	"testing"
)

func TestAddDirArgsMutationBoundariesKeepArgumentOrder(t *testing.T) {
	for _, tt := range []struct {
		name string
		dirs []string
		args []string
		want []string
	}{
		{name: "no directories", args: []string{"--model", "opus"}, want: []string{"--model", "opus"}},
		{name: "one directory", dirs: []string{"/repo"}, args: []string{"prompt"}, want: []string{"--add-dir", "/repo", "prompt"}},
		{name: "multiple directories", dirs: []string{"/one", "/two"}, args: []string{"--flag", "value"}, want: []string{"--add-dir", "/one", "--add-dir", "/two", "--flag", "value"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := addDirArgs(tt.dirs, tt.args); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("addDirArgs(%v, %v)=%v, want %v", tt.dirs, tt.args, got, tt.want)
			}
		})
	}
}
