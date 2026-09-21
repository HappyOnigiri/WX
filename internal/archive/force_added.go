package archive

import (
	"fmt"
	"strings"
)

// forceAddedEntries は、HEAD に無く現在の index に stage 0 で載る path の entry を
// `git update-index -z --index-info` の入力形式で返す。対象が無ければ nil を返す。
//
// snapshot と復元検証は作業ツリーの tree を「HEAD で初期化した一時 index への add -A」で作る。
// この一時 index から見ると、ignore 規則に一致する force-added path は未追跡の ignored file でしかなく
// add が飛ばすため、先に entry を持ち込まないと未 staged の作業内容が tree から丸ごと落ちる。
// 未解消 path は stage 1/2/3 しか持たず、conflict artifact 側が別に保存するのでここでは対象外である。
// commentlint:allow-long -- 一時 index へ entry を持ち込まないと作業内容が失われる理由を説明する
func forceAddedEntries(value gitValueFunc) ([]byte, error) {
	listing, err := value(nil, "diff-index", "--cached", "--diff-filter=A", "--name-only", "-z", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("list paths added to the index since HEAD: %w", err)
	}
	added := map[string]struct{}{}
	for _, path := range strings.Split(listing, "\x00") {
		if path != "" {
			added[path] = struct{}{}
		}
	}
	if len(added) == 0 {
		return nil, nil
	}
	staged, err := value(nil, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, fmt.Errorf("read index entries added since HEAD: %w", err)
	}
	var input strings.Builder
	for _, entry := range strings.Split(staged, "\x00") {
		mode, oid, path, ok := parseStageZeroEntry(entry)
		if !ok {
			continue
		}
		if _, want := added[path]; !want {
			continue
		}
		input.WriteString(mode + " " + oid + " 0\t" + path + "\x00")
	}
	if input.Len() == 0 {
		return nil, nil
	}
	return []byte(input.String()), nil
}

// parseStageZeroEntry は `git ls-files --stage -z` の 1 項目 `<mode> <oid> <stage>\t<path>` を分解する。
// stage 0 以外と読めない項目は ok=false を返す。path は -z のため quote されず、最初の TAB だけが区切りになる。
func parseStageZeroEntry(entry string) (mode, oid, path string, ok bool) {
	tab := strings.IndexByte(entry, '\t')
	if tab < 0 {
		return "", "", "", false
	}
	fields := strings.Fields(entry[:tab])
	if len(fields) != 3 || fields[2] != "0" {
		return "", "", "", false
	}
	return fields[0], fields[1], entry[tab+1:], true
}

// seedForceAddedEntries は env が指す一時 index へ、HEAD に無い index entry を持ち込む。
// add より前に呼ぶことで、その path は一時 index でも tracked になり ignore 判定を受けなくなる。
// 作業ツリーから消えた path は続く add -A が削除として記録するため、ここでの持ち込みは状態を固定しない。
func seedForceAddedEntries(run gitRunFunc, value gitValueFunc, env []string) error {
	entries, err := forceAddedEntries(value)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := run(env, entries, "update-index", "-z", "--index-info"); err != nil {
		return fmt.Errorf("seed paths added to the index since HEAD: %w", err)
	}
	return nil
}
