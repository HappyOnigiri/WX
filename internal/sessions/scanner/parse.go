package scanner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxTitleRunes = 80

// parserVersion は JSONL からメタデータを取り出す規則の版である。
// タイトル・CWD・ID の決め方を変えたら上げる。cache の検証値に含めるため、古い解析結果は再利用されなくなる。
const parserVersion = 1

type fileMeta struct {
	nativeID string
	title    string
	cwd      string
	pending  string
	subagent bool
}

type claudeLine struct {
	Type             string          `json:"type"`
	CWD              string          `json:"cwd"`
	AITitle          string          `json:"aiTitle"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	Message          json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Content json.RawMessage `json:"content"`
}

type codexEnvelope struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type codexMeta struct {
	ID           string `json:"id"`
	CWD          string `json:"cwd"`
	ThreadSource string `json:"thread_source"`
}

type codexItem struct {
	Item    json.RawMessage `json:"item"`
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// readMetadata は開いた JSONL の先頭から必要なメタデータが揃うまで読む。
// file は呼び出し側が所有し、offset 0 から読める状態で渡す。path は Codex の ID 補完と error 文脈にだけ使う。
func readMetadata(ctx context.Context, file *os.File, path, tool string) (fileMeta, error) {
	meta := fileMeta{}
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return fileMeta{}, err
		}
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			parseLine(tool, line, &meta)
		}
		if metadataComplete(tool, path, meta) {
			return finalizeMetadata(meta), nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return finalizeMetadata(meta), nil
			}
			return fileMeta{}, fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}

func metadataComplete(tool, path string, meta fileMeta) bool {
	if meta.title == "" || meta.cwd == "" {
		return false
	}
	if tool == "claude" {
		return strings.TrimSuffix(filepath.Base(path), ".jsonl") != ""
	}
	return meta.nativeID != "" || codexIDFromPath(path) != ""
}

func finalizeMetadata(meta fileMeta) fileMeta {
	if meta.title == "" {
		meta.title = meta.pending
	}
	return meta
}

func parseLine(tool string, line []byte, meta *fileMeta) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if tool == "claude" {
		parseClaudeLine(line, meta)
		return
	}
	parseCodexLine(line, meta)
}

func parseClaudeLine(line []byte, meta *fileMeta) {
	var env claudeLine
	if json.Unmarshal(line, &env) != nil {
		return
	}
	if meta.cwd == "" {
		meta.cwd = env.CWD
	}
	if env.Type == "ai-title" && meta.title == "" {
		meta.title = firstLine(env.AITitle)
		return
	}
	if env.Type != "user" || env.IsCompactSummary || env.IsMeta || env.IsSidechain {
		return
	}
	var message claudeMessage
	if json.Unmarshal(env.Message, &message) != nil {
		return
	}
	text := claudeText(message.Content)
	setClaudeTitle(meta, text)
}

func claudeText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var result []string
	for _, part := range parts {
		if part.Text != "" && (part.Type == "" || part.Type == "text") {
			result = append(result, part.Text)
		}
	}
	return strings.Join(result, "\n")
}

func setClaudeTitle(meta *fileMeta, text string) {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "This session is being continued from a previous conversation") {
			continue
		}
		if strings.HasPrefix(line, "<local-command-") || strings.HasPrefix(line, "<command-") {
			continue
		}
		if strings.HasPrefix(line, "/") && !strings.ContainsAny(line, " \t") {
			if meta.pending == "" {
				meta.pending = truncateTitle(line)
			}
			continue
		}
		if meta.title == "" {
			meta.title = truncateTitle(line)
		}
		return
	}
}

func parseCodexLine(line []byte, meta *fileMeta) {
	var env codexEnvelope
	if json.Unmarshal(line, &env) != nil {
		return
	}
	switch env.Type {
	case "session_meta":
		var payload codexMeta
		if json.Unmarshal(env.Payload, &payload) != nil {
			return
		}
		if meta.nativeID == "" {
			meta.nativeID = payload.ID
		}
		if meta.cwd == "" {
			meta.cwd = payload.CWD
		}
		if payload.ThreadSource == "subagent" {
			meta.subagent = true
		}
	case "turn_context":
		if meta.cwd == "" {
			var payload codexMeta
			if json.Unmarshal(env.Payload, &payload) == nil {
				meta.cwd = payload.CWD
			}
		}
	case "response_item":
		role, content, ok := codexMessage(env.Payload)
		if !ok || strings.ToLower(role) != "user" || meta.title != "" {
			return
		}
		setCodexTitle(meta, codexText(content))
	}
}

func codexMessage(raw json.RawMessage) (role string, content json.RawMessage, ok bool) {
	var item codexItem
	if json.Unmarshal(raw, &item) != nil {
		return "", nil, false
	}
	if len(item.Item) > 0 && string(item.Item) != "null" {
		var nested codexItem
		if json.Unmarshal(item.Item, &nested) == nil && nested.Type != "" {
			item = nested
		}
	}
	if item.Type != "message" {
		return "", nil, false
	}
	return item.Role, item.Content, true
}

func codexText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []textPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var result []string
	for _, part := range parts {
		switch part.Type {
		case "", "input_text", "output_text", "text":
			if part.Text != "" {
				result = append(result, part.Text)
			}
		}
	}
	return strings.Join(result, "\n")
}

func setCodexTitle(meta *fileMeta, text string) {
	text = strings.TrimSpace(text)
	if text == "" || strings.HasPrefix(text, "# AGENTS.md instructions for ") || strings.HasPrefix(text, "<environment_context") || strings.HasPrefix(text, "<skill") || strings.HasPrefix(text, "<turn_aborted") || strings.HasPrefix(text, "The user interrupted the previous turn") || strings.HasPrefix(text, "Continue working toward the active thread goal.") || strings.HasPrefix(text, "<user_instructions") {
		return
	}
	for _, tag := range []string{"user_query", "user_message"} {
		if strings.HasPrefix(text, "<"+tag) {
			text = xmlBlock(text, tag)
			break
		}
	}
	if strings.Contains(text, "<codex_internal_context") {
		text = xmlBlock(text, "objective")
	}
	if title := firstLine(text); title != "" {
		meta.title = title
	}
}

func xmlBlock(text, tag string) string {
	start := strings.Index(text, "<"+tag)
	if start < 0 {
		return ""
	}
	openEnd := strings.IndexByte(text[start:], '>')
	if openEnd < 0 {
		return ""
	}
	contentStart := start + openEnd + 1
	close := strings.Index(text[contentStart:], "</"+tag+">")
	if close < 0 {
		return ""
	}
	return strings.TrimSpace(text[contentStart : contentStart+close])
}

func firstLine(text string) string {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return truncateTitle(line)
		}
	}
	return ""
}

func truncateTitle(text string) string {
	runes := []rune(text)
	if len(runes) <= maxTitleRunes {
		return text
	}
	return string(runes[:maxTitleRunes])
}

func codexIDFromPath(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	parts := strings.Split(base, "-")
	if len(parts) < 5 {
		return ""
	}
	parts = parts[len(parts)-5:]
	want := []int{8, 4, 4, 4, 12}
	for i, part := range parts {
		if len(part) != want[i] {
			return ""
		}
		for _, r := range part {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return ""
			}
		}
	}
	return strings.Join(parts, "-")
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
