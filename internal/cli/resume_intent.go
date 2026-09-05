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
	Rest           []string
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
	if len(args) == 0 || args[0] != "resume" {
		return resumeIntent{Kind: resumeIntentNone, Rest: cloneResumeArgs(args)}
	}

	intent := resumeIntent{Kind: resumeIntentPicker}
	var sessionID string
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--last":
			intent.Kind = resumeIntentContinueLatest
		case "--all":
			intent.WidenScope = true
		default:
			if strings.HasPrefix(arg, "-") {
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

// codexResumeFlagTakesValue は resume の位置引数探索から値付きフラグを除外する。
// それ以外の引数は Rest に残すだけで、未知のフラグを wx 側で解釈しない。
func codexResumeFlagTakesValue(arg string) bool {
	if strings.Contains(arg, "=") {
		return false
	}
	switch arg {
	case "-c", "--config", "-m", "--model", "--cd", "--profile", "--sandbox", "--ask-for-approval", "--add-dir", "--image", "--output-last-message":
		return true
	default:
		return false
	}
}

func cloneResumeArgs(args []string) []string {
	return append([]string(nil), args...)
}
