package setup

import "github.com/HappyOnigiri/WX/internal/i18n"

// message は message ID と、その template が使うフィールドを 1 行で組み立てる。
// 項目の収集は短い分岐の連続なので、map literal を毎回書くと判定そのものが読み取れなくなる。
// pairs は名前と値を交互に並べ、奇数個なら最後の名前を捨てる。
func message(id string, pairs ...any) i18n.Message {
	if len(pairs) < 2 {
		return i18n.Message{ID: id}
	}
	data := make(map[string]any, len(pairs)/2)
	for index := 0; index+1 < len(pairs); index += 2 {
		name, ok := pairs[index].(string)
		if !ok {
			continue
		}
		data[name] = pairs[index+1]
	}
	return i18n.Message{ID: id, Data: data}
}

// messageError は message ID を持つ error を、message と同じ書き方で作る。
func messageError(id string, pairs ...any) error {
	return i18n.NewError(id, message(id, pairs...).Data)
}

// pathProblem は path の検査結果を理由 1 件にする。
// diag が message を持たない結果（Lstat の失敗）は外部由来の本文なので、原文のまま本文へ載せる。
func pathProblem(path, result string, reason i18n.Message) i18n.Message {
	if reason.ID == "" {
		return message("setup.reason.path_problem_raw", "Path", path, "Detail", result)
	}
	return message("setup.reason.path_problem", "Path", path, "Reason", reason)
}
