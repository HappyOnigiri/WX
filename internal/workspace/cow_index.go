package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// cowIndexEntry は `ls-files --stage` の1件のうち CoW が使う情報である。
// oid は宛先 index の blob で、main 側 index と一致しない path を走査前に除く用途にだけ使う。
type cowIndexEntry struct {
	name string
	oid  string
}

// cowShareableIndexMode は共有候補にできる index entry かを mode と stage で判定する。
// symlink・gitlink・merge 途中の stage 付き entry は通常 checkout のまま残す。
func cowShareableIndexMode(fields []string) bool {
	return fields[2] == "0" && (fields[0] == "100644" || fields[0] == "100755")
}

// parseCOWIndexEntries は宛先 index の出力を共有候補へ変換する。
// path の逸脱は宛先の破壊に直結するため、skip ではなく失敗として返す。
func parseCOWIndexEntries(stdout string) ([]cowIndexEntry, error) {
	var entries []cowIndexEntry
	for _, entry := range strings.Split(stdout, "\x00") {
		if entry == "" {
			continue
		}
		header, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 {
			return nil, errors.New("invalid Git index entry for CoW")
		}
		if !cowShareableIndexMode(fields) {
			continue
		}
		if !filepath.IsLocal(name) || filepath.Clean(name) != name {
			return nil, errors.New("unsafe Git path for CoW")
		}
		entries = append(entries, cowIndexEntry{name: name, oid: fields[1]})
	}
	return entries, nil
}

// parseCOWSourceIndexOIDs は main 側 index を path から blob OID への表にする。
// 事前 skip の材料でしかないため、解釈できない行は表へ載せず、その path を共有対象外へ倒す。
func parseCOWSourceIndexOIDs(stdout string) map[string]string {
	oids := map[string]string{}
	for _, entry := range strings.Split(stdout, "\x00") {
		if entry == "" {
			continue
		}
		header, name, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !cowShareableIndexMode(fields) {
			continue
		}
		oids[name] = fields[1]
	}
	return oids
}

// readCOWSourceIndexOIDs は main worktree の index を読む。
// --no-optional-locks は stat refresh の書き戻しを止める指定で、ソースリポジトリの index を変えないために必須である。
func (p *Preparer) readCOWSourceIndexOIDs(ctx context.Context, source *os.Root) (map[string]string, error) {
	directory, err := source.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	result, err := p.runGitInDirectory(ctx, directory, "--no-optional-locks", "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	return parseCOWSourceIndexOIDs(result.Stdout), nil
}

// cowSourceIndexOIDs は事前 skip 用の表を返す。
// 共有を増やす方向には働かない補助情報なので、取得できなければ skip なしで従来どおり走査する。
func (p *Preparer) cowSourceIndexOIDs(ctx context.Context, source *os.Root) map[string]string {
	oids, err := p.readCOWSourceIndexOIDs(ctx, source)
	if err != nil {
		p.logSkip("CoW source index is unavailable", "error", err)
		return nil
	}
	return oids
}

// cowScope は compaction の候補を、その準備が実際に書き直した path へ限定する。
// nil は限定なしを表し、新規準備と復元は宛先に共有済みの実体が無いため常に nil を渡す。
type cowScope struct {
	rewritten map[string]bool
}

// narrow は書き直していない path の entry を落とす。
// 落とした path の宛先は前回の準備が作った実体そのままなので、共有済みなら共有のまま、そうでなければ通常 checkout のまま残る。
func (s *cowScope) narrow(entries []cowIndexEntry) []cowIndexEntry {
	if s == nil {
		return entries
	}
	candidates := make([]cowIndexEntry, 0, len(s.rewritten))
	for _, entry := range entries {
		if s.rewritten[entry.name] {
			candidates = append(candidates, entry)
		}
	}
	return candidates
}

// selectCOWCandidates は main 側 index と blob OID が一致する entry だけを残す。
// OID の一致は共有の根拠ではない（main が dirty なら内容は違う）ため、不一致を除く用途に限る。
func selectCOWCandidates(entries []cowIndexEntry, sourceOIDs map[string]string) []cowIndexEntry {
	if sourceOIDs == nil {
		return entries
	}
	candidates := make([]cowIndexEntry, 0, len(entries))
	for _, entry := range entries {
		if sourceOIDs[entry.name] == entry.oid {
			candidates = append(candidates, entry)
		}
	}
	return candidates
}
