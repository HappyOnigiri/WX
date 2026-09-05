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
	if view := model.content(); !strings.Contains(view, "› Cold") || !strings.Contains(view, "↑/↓") {
		t.Fatal(view)
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
