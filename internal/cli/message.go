package cli

import "github.com/HappyOnigiri/WX/internal/i18n"

// message は message ID と、その template が使うフィールドを 1 行で組み立てる。
// 診断は 1 つの finding で 3〜4 個の message を作るため、map literal を毎回書くと報告内容が埋もれる。
// pairs は名前と値を交互に並べ、奇数個なら最後の名前を捨てる。
func message(id string, pairs ...any) i18n.Message {
	if len(pairs) < 2 {
		return i18n.Message{ID: id}
	}
	// map の初期容量は message の外部契約ではなく、入力長の算術を判定対象にしない。
	data := make(map[string]any)
	for index := 0; index+1 < len(pairs); index += 2 {
		name, ok := pairs[index].(string)
		if !ok {
			continue
		}
		data[name] = pairs[index+1]
	}
	return i18n.Message{ID: id, Data: data}
}
