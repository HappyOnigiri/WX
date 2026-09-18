package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func testSelection() Selection {
	return Selection{Title: "Choose", Description: "Description", Initial: 1, Options: []Option{{Value: "hot", Label: "Hot", Description: "Ready"}, {Value: "cold", Label: "Cold"}, {Value: "off", Label: "Off"}}}
}

func TestSelectionNavigationAndConfirmation(t *testing.T) {
	initial := testSelection()
	model := selectionModel{selection: initial, cursor: initial.Initial}
	if model.Init() != nil {
		t.Fatal("unexpected initial command")
	}
	// 説明はラベルと同じ行の桁揃えした列に出て、選択中の行は緑になる。
	view := model.content()
	for _, want := range []string{"? Choose", "Hot    Ready", "\x1b[32m› Cold\x1b[39m", "↑/↓"} {
		if !strings.Contains(view, want) {
			t.Fatalf("want=%q view=%s", want, view)
		}
	}
	for _, key := range []rune{tea.KeyDown, tea.KeyDown, tea.KeyUp, tea.KeyEnter} {
		next, _ := model.Update(tea.KeyPressMsg{Code: key})
		model = next.(selectionModel)
	}
	if !model.confirmed || model.selection.Options[model.cursor].Value != "off" {
		t.Fatalf("model=%+v", model)
	}
	if !strings.Contains(model.content(), "✓ Choose: Off") {
		t.Fatal(model.content())
	}
	next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if next.(selectionModel).cursor != model.cursor {
		t.Fatal("changed after confirmation")
	}
}

func TestSelectionClearOnExitRemovesCompletedView(t *testing.T) {
	selection := testSelection()
	selection.ClearOnExit = true
	model := selectionModel{selection: selection, cursor: selection.Initial}
	next, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := next.(selectionModel).content(); got != "" {
		t.Fatalf("completed content=%q, want empty", got)
	}
	next, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if got := next.(selectionModel).content(); got != "" {
		t.Fatalf("cancelled content=%q, want empty", got)
	}
}

func TestSelectAcceptsTheFirstOptionAtTheLowerBoundary(t *testing.T) {
	selection := testSelection()
	selection.Initial = 0
	got, err := Select(context.Background(), strings.NewReader("\r"), io.Discard, selection)
	if err != nil || got != "hot" {
		t.Fatalf("got=%q err=%v, want the first option", got, err)
	}
}

func TestSelectRejectsAnInitialIndexAtTheOptionCount(t *testing.T) {
	selection := testSelection()
	selection.Initial = len(selection.Options)
	_, err := Select(context.Background(), strings.NewReader("\x03"), io.Discard, selection)
	if err == nil || err.Error() != "selection requires options and a valid initial index" {
		t.Fatalf("error=%v, want invalid initial index", err)
	}
}

func TestSelectionContentKeepsEqualWidthLabelsAligned(t *testing.T) {
	selection := Selection{
		Title: "Choose",
		Options: []Option{
			{Label: "One", Description: "first"},
			{Label: "Two", Description: "second"},
		},
		Initial: 0,
	}
	view := (selectionModel{selection: selection, cursor: selection.Initial}).content()
	for _, want := range []string{"\x1b[39m   first", "    Two   second"} {
		if !strings.Contains(view, want) {
			t.Fatalf("want=%q view=%s", want, view)
		}
	}
}

func TestSelectionCancellationAndUnsafeText(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEsc}, {Code: 'c', Mod: tea.ModCtrl}} {
		model := selectionModel{selection: testSelection()}
		next, command := model.Update(key)
		result := next.(selectionModel)
		if result.confirmed || !result.finished || command == nil || !strings.Contains(result.content(), "cancelled") {
			t.Fatalf("result=%+v", result)
		}
	}
	if got := singleLine("path\n\x1b[2J"); strings.ContainsAny(got, "\n\x1b") {
		t.Fatal(got)
	}
}

func TestSelectProgram(t *testing.T) {
	for _, test := range []struct {
		input, want string
		cancel      bool
	}{{"\x1b[B\r", "off", false}, {"\x1b[A\r", "hot", false}, {"\x03", "", true}} {
		got, err := Select(context.Background(), strings.NewReader(test.input), io.Discard, testSelection())
		if got != test.want || (test.cancel && !errors.Is(err, ErrCancelled)) || (!test.cancel && err != nil) {
			t.Fatalf("got=%q err=%v", got, err)
		}
	}
	if _, err := Select(context.Background(), nil, io.Discard, Selection{}); err == nil {
		t.Fatal("empty selection accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Select(ctx, strings.NewReader(""), io.Discard, testSelection()); err == nil {
		t.Fatal("cancelled context accepted")
	}
}
