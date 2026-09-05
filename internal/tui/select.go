// Package tui は CLI 間で共有する対話的な選択画面を提供する。
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
)

var ErrCancelled = errors.New("selection cancelled")

type Option struct {
	Value       string
	Label       string
	Description string
}

type Selection struct {
	Title       string
	Description string
	Options     []Option
	Initial     int
}

// Select は上下キーで移動し Enter で確定する。Esc/Ctrl+C と context の中断では値を返さず、端末状態を復元する。
// input と output は呼び出し側で確認済みの対話端末を渡す。
func Select(ctx context.Context, input io.Reader, output io.Writer, selection Selection) (string, error) {
	if len(selection.Options) == 0 || selection.Initial < 0 || selection.Initial >= len(selection.Options) {
		return "", errors.New("selection requires options and a valid initial index")
	}
	initial := selectionModel{selection: selection, cursor: selection.Initial}
	result, err := tea.NewProgram(initial, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output)).Run()
	if err != nil {
		return "", err
	}
	model, ok := result.(selectionModel)
	if !ok || !model.confirmed {
		return "", ErrCancelled
	}
	return model.selection.Options[model.cursor].Value, nil
}

type selectionModel struct {
	selection Selection
	cursor    int
	confirmed bool
	finished  bool
}

func (m selectionModel) Init() tea.Cmd { return nil }

func (m selectionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.finished {
		return m, nil
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "up", "k":
			m.cursor = (m.cursor + len(m.selection.Options) - 1) % len(m.selection.Options)
		case "down", "j":
			m.cursor = (m.cursor + 1) % len(m.selection.Options)
		case "enter":
			m.confirmed = true
			m.finished = true
			return m, tea.Quit
		case "esc", "ctrl+c":
			m.finished = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m selectionModel) View() tea.View {
	return tea.NewView(m.content())
}

func (m selectionModel) content() string {
	if m.finished {
		if m.confirmed {
			return fmt.Sprintf("✓ %s: %s\n", singleLine(m.selection.Title), singleLine(m.selection.Options[m.cursor].Label))
		}
		return "Selection cancelled.\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "\n%s\n", singleLine(m.selection.Title))
	if m.selection.Description != "" {
		fmt.Fprintf(&out, "%s\n", singleLine(m.selection.Description))
	}
	out.WriteByte('\n')
	for index, option := range m.selection.Options {
		marker := "  "
		if index == m.cursor {
			marker = "› "
		}
		fmt.Fprintf(&out, "%s%s\n", marker, singleLine(option.Label))
		if option.Description != "" {
			fmt.Fprintf(&out, "    %s\n", singleLine(option.Description))
		}
	}
	out.WriteString("\n↑/↓ move · Enter select · Esc cancel\n")
	return out.String()
}

// singleLine はパスや選択肢に含まれる制御文字を除き、画面の制御シーケンスとして扱わせない。
func singleLine(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
