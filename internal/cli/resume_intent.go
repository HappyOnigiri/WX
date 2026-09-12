package cli

import "strings"

// resumeIntentKind はエージェント引数から抽出した resume 操作の種類である。
type resumeIntentKind string

const (
	resumeIntentNone           resumeIntentKind = "none"
	resumeIntentContinueLatest resumeIntentKind = "continueLatest"
	resumeIntentPicker         resumeIntentKind = "picker"
	resumeIntentLookup         resumeIntentKind = "lookup"
)

type resumeIntent struct {
	Kind           resumeIntentKind
	AgentSessionID string
	WidenScope     bool
	// Prefix は codex の resume より前に置かれた引数を保持する。
	// exec 形では exec 自身とそのオプションも含め、起動時に同じ位置へ戻す。
	Prefix []string
	Rest   []string
	// CodexExec は codex の exec サブコマンド形を保持する。
	CodexExec bool
	// Notice は resume として解釈できない exec resume 形式を検出したことを表す。
	Notice bool
}

// parseResumeIntent は agent の引数から wx が扱う resume 指定だけを取り出す。
// 未知の引数は順序を保ったまま Rest に残し、agent 本体の解釈に委ねる。
func parseResumeIntent(agent string, args []string) resumeIntent {
	switch agent {
	case "claude":
		return parseClaudeResumeIntent(args)
	case "codex":
		return parseCodexResumeIntent(args)
	default:
		return resumeIntent{Kind: resumeIntentNone, Rest: cloneResumeArgs(args)}
	}
}

func parseClaudeResumeIntent(args []string) resumeIntent {
	intent := resumeIntent{Kind: resumeIntentNone}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			intent.Rest = append(intent.Rest, args[i:]...)
			break
		}
		switch {
		case arg == "-c" || arg == "--continue":
			intent.Kind = resumeIntentContinueLatest
			intent.AgentSessionID = ""
		case arg == "-r" || arg == "--resume":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				intent.Kind = resumeIntentLookup
				intent.AgentSessionID = args[i]
			} else {
				intent.Kind = resumeIntentPicker
				intent.AgentSessionID = ""
			}
		case strings.HasPrefix(arg, "--resume="):
			id := strings.TrimPrefix(arg, "--resume=")
			if id == "" {
				intent.Kind = resumeIntentPicker
				intent.AgentSessionID = ""
			} else {
				intent.Kind = resumeIntentLookup
				intent.AgentSessionID = id
			}
		default:
			intent.Rest = append(intent.Rest, arg)
		}
	}
	return intent
}

func parseCodexResumeIntent(args []string) resumeIntent {
	shape, ok := codexResumeShape(args)
	if !ok {
		return resumeIntent{Kind: resumeIntentNone, Rest: cloneResumeArgs(args)}
	}
	if shape.resumeIndex < 0 {
		return resumeIntent{
			Kind:      resumeIntentNone,
			Rest:      cloneResumeArgs(args),
			Prefix:    stripCodexCDArgs(args[:shape.prefixEnd]),
			CodexExec: true,
		}
	}

	intent := parseCodexResumeTail(args[shape.resumeIndex+1:])
	intent.Prefix = stripCodexCDArgs(args[:shape.resumeIndex])
	intent.CodexExec = shape.exec
	if shape.exec && intent.Kind == resumeIntentPicker && intent.AgentSessionID == "" {
		// exec resume は picker を持たず、ID も --last も無ければ wx の resume として扱えない。
		// この場合は通常起動へ素通しするが、呼び出し側が明示的な wx resume なら Prefix/Rest を再利用できる。
		intent.Kind = resumeIntentNone
		intent.Notice = true
		intent.Rest = cloneResumeArgs(args)
	}
	return intent
}

// parseCodexResumeTail は resume の後ろから wx が扱う指定を取り出す。
func parseCodexResumeTail(args []string) resumeIntent {
	intent := resumeIntent{Kind: resumeIntentPicker}
	var sessionID string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			intent.Rest = append(intent.Rest, args[i:]...)
			break
		}
		switch arg {
		case "--last":
			intent.Kind = resumeIntentContinueLatest
		case "--all":
			intent.WidenScope = true
		default:
			if strings.HasPrefix(arg, "-") {
				if codexResumeCDArg(arg) {
					if !strings.Contains(arg, "=") && i+1 < len(args) {
						i++
					}
					continue
				}
				intent.Rest = append(intent.Rest, arg)
				if codexResumeFlagTakesValue(arg) && i+1 < len(args) {
					i++
					intent.Rest = append(intent.Rest, args[i])
				}
				continue
			}
			if sessionID == "" {
				sessionID = arg
				intent.Kind = resumeIntentLookup
				continue
			}
			intent.Rest = append(intent.Rest, arg)
		}
	}
	if sessionID != "" {
		intent.Kind = resumeIntentLookup
		intent.AgentSessionID = sessionID
	} else if intent.Kind == resumeIntentContinueLatest {
		intent.AgentSessionID = ""
	}
	return intent
}

type codexResumeLocation struct {
	resumeIndex int
	exec        bool
	prefixEnd   int
}

// codexResumeShape は codex のグローバル引数と exec のオプションを読み飛ばし、resume の形を返す。
// `exec` の resume は exec 自身の後ろにあり、native 形とは別の引数位置へ組み直す必要がある。
func codexResumeShape(args []string) (codexResumeLocation, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return codexResumeLocation{}, false
		}
		if strings.HasPrefix(arg, "-") {
			if codexResumeFlagTakesValue(arg) && i+1 < len(args) {
				i++
			}
			continue
		}
		switch arg {
		case "resume":
			return codexResumeLocation{resumeIndex: i, prefixEnd: i}, true
		case "exec", "e":
			for j := i + 1; j < len(args); j++ {
				nested := args[j]
				if nested == "--" {
					return codexResumeLocation{}, false
				}
				if strings.HasPrefix(nested, "-") {
					if codexResumeFlagTakesValue(nested) && j+1 < len(args) {
						j++
					}
					continue
				}
				if nested == "resume" {
					return codexResumeLocation{resumeIndex: j, exec: true, prefixEnd: j}, true
				}
				// fork/review は別の exec サブコマンドなので、resume として扱わない。
				if nested == "fork" || nested == "review" {
					return codexResumeLocation{}, false
				}
				// wx resume の明示経路では、exec の後ろへ resume を差し込む。
				return codexResumeLocation{resumeIndex: -1, exec: true, prefixEnd: j}, true
			}
			return codexResumeLocation{resumeIndex: -1, exec: true, prefixEnd: len(args)}, true
		default:
			return codexResumeLocation{}, false
		}
	}
	return codexResumeLocation{}, false
}

// codexResumeSubcommandIndex は互換性のため native 形の resume 位置を返す。
// exec 形を含む解析は codexResumeShape が担う。
func codexResumeSubcommandIndex(args []string) (int, bool) {
	shape, ok := codexResumeShape(args)
	if !ok || shape.exec || shape.resumeIndex < 0 {
		return 0, false
	}
	return shape.resumeIndex, true
}

// codexResumeFlagTakesValue は resume の位置引数探索から値付きフラグを除外する。
// それ以外の引数は Rest に残すだけで、未知のフラグを wx 側で解釈しない。
func codexResumeFlagTakesValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "-c", "--config", "-m", "--model", "--cd", "-C", "--profile", "--sandbox", "--ask-for-approval", "--add-dir", "--image", "--output-last-message", "-p", "--enable", "--disable", "--local-provider", "--thread-source", "--output-schema", "--color":
		return true
	default:
		return false
	}
}

// codexResumeCDArg は resume 経路で wx が貸出先を固定するため除去する指定を判定する。
func codexResumeCDArg(arg string) bool {
	return arg == "--cd" || arg == "-C" || strings.HasPrefix(arg, "--cd=") || strings.HasPrefix(arg, "-C=")
}

// stripCodexCDArgs は利用者が渡した CWD 指定を落とし、wx の貸出先だけを残す。
func stripCodexCDArgs(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	clean := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if codexResumeCDArg(arg) {
			if !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
			}
			continue
		}
		clean = append(clean, arg)
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

func cloneResumeArgs(args []string) []string {
	return append([]string(nil), args...)
}
