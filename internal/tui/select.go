// Package tui は CLI と daemon client で共有する端末表示を提供する。
// 対話的な選択画面と、待機中に1行を書き換え続ける進捗行が含まれる。
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/HappyOnigiri/WorktreeX/internal/i18n"
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
	// Preamble は質問より前に表示する複数行の判断材料。ClearOnExit と組み合わせると結果表示も一時画面へ閉じ込められる。
	Preamble string
	Options  []Option
	Initial  int
	// ClearOnExit は確定・キャンセル後に選択画面を消し、後続の対話表示へ結果行を残さない。
	ClearOnExit bool
	// Language は固定ラベルの表示言語。空文字は英語で、既存 caller と互換である。
	Language string
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
	view := tea.NewView(m.content())
	// 一時表示は alternate screen に閉じ込め、終了時に元の画面を復元する。
	// 空の最終 View だけでは Bubble Tea の inline renderer が表示済みの行を消去しない。
	view.AltScreen = m.selection.ClearOnExit
	return view
}

func (m selectionModel) content() string {
	if m.finished {
		if m.selection.ClearOnExit {
			return ""
		}
		if m.confirmed {
			return fmt.Sprintf("✓ %s: %s\n", singleLine(m.selection.Title), singleLine(m.selection.Options[m.cursor].Label))
		}
		return i18n.New(m.selection.Language).Localize("tui.select.cancelled", nil) + "\n"
	}
	var out strings.Builder
	if preamble := multiLine(m.selection.Preamble); preamble != "" {
		out.WriteString(preamble)
		if !strings.HasSuffix(preamble, "\n") {
			out.WriteByte('\n')
		}
		out.WriteByte('\n')
	} else {
		out.WriteByte('\n')
	}
	fmt.Fprintf(&out, "? %s\n", singleLine(m.selection.Title))
	if m.selection.Description != "" {
		fmt.Fprintf(&out, "  %s\n", singleLine(m.selection.Description))
	}
	out.WriteByte('\n')
	// 選択肢は左、説明は右の桁揃えした列に置き、装飾なしでも両者の区別が付くようにする。
	labels := make([]string, len(m.selection.Options))
	column := 0
	for index, option := range m.selection.Options {
		labels[index] = singleLine(option.Label)
		if width := ansi.StringWidth(labels[index]); width > column {
			column = width
		}
	}
	for index, option := range m.selection.Options {
		line := "    " + labels[index]
		if index == m.cursor {
			line = "  " + green("› "+labels[index])
		}
		if description := singleLine(option.Description); description != "" {
			line += strings.Repeat(" ", column-ansi.StringWidth(labels[index])+3) + description
		}
		fmt.Fprintf(&out, "%s\n", line)
	}
	out.WriteString("\n  " + i18n.New(m.selection.Language).Localize("tui.select.footer", nil) + "\n")
	return out.String()
}

// green は選択中の行を緑にする。
// bubbleteaのレンダラが端末の色プロファイルに合わせて落とすため、色を扱えない端末に制御列は残らない。
func green(value string) string {
	return "\x1b[32m" + value + "\x1b[39m"
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

// multiLine は改行だけを維持し、それ以外の制御文字を端末操作として解釈させない。
func multiLine(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
}
