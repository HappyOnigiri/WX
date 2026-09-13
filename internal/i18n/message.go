package i18n

import "errors"

// message.go は「解決前の表示文」を値として持ち回るための型を持つ。
// 描画済みの文字列を後から置換する経路を作らないために、生成側は message ID を、
// 表示側は言語を持ち、両者が出会う場所だけで訳文を確定させる。

// Message は解決前の表示文である。Data のキーは catalog の template が使う
// フィールド名で、path・ID・外部コマンドの出力などの不透明値をここへ載せる。
type Message struct {
	ID   string
	Data map[string]any
}

// MessageError は message ID を保持する error である。
// Error は英語を返すので、ログ・テスト・外部へ渡る文字列は言語設定に左右されない。
type MessageError struct {
	Message Message
	wrapped error
}

// NewError は message ID だけを持つ error を返す。
func NewError(id string, data map[string]any) error {
	return &MessageError{Message: Message{ID: id, Data: data}}
}

// WrapError は下位 error を errors.Is・errors.As へ残したまま message ID を付ける。
// 下位 error の本文を表示へ出すときは、その文字列を data のフィールドへ渡す。
func WrapError(err error, id string, data map[string]any) error {
	return &MessageError{Message: Message{ID: id, Data: data}, wrapped: err}
}

func (e *MessageError) Error() string {
	return New(string(English)).Message(e.Message)
}

func (e *MessageError) Unwrap() error { return e.wrapped }

// Message は Message を解決する。Localizer を使い回すことで、行ごとに bundle を
// 組み直さずに済む。Data に入れ子の Message があれば先に解決するので、
// 文の一部だけが言語ごとに変わる場合も、断片を英語のまま連結せずに済む。
func (l *Localizer) Message(m Message) string {
	if m.ID == "" {
		return ""
	}
	data := m.Data
	if len(data) > 0 {
		resolved := make(map[string]any, len(data))
		for key, value := range data {
			if nested, ok := value.(Message); ok {
				resolved[key] = l.Message(nested)
				continue
			}
			resolved[key] = value
		}
		data = resolved
	}
	return l.Localize(m.ID, data)
}

// Error は err の表示文を返す。message ID を持たない外部 error は原文のまま返す。
func (l *Localizer) Error(err error) string {
	if err == nil {
		return ""
	}
	var target *MessageError
	if errors.As(err, &target) {
		return l.Message(target.Message)
	}
	return err.Error()
}

// ErrorValue は error を template のデータとして渡せる値にする。
// message ID を持つ error は入れ子の Message として渡って表示言語で解決され、
// 外部 error はその原文が入る。
func ErrorValue(err error) any {
	if err == nil {
		return ""
	}
	var target *MessageError
	if errors.As(err, &target) {
		return target.Message
	}
	return err.Error()
}

// LocalizeError は 1 回限りの解決に使う短縮 API である。
func LocalizeError(err error, lang Language) string {
	return New(string(lang)).Error(err)
}
