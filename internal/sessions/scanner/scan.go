package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/HappyOnigiri/WX/internal/domain"
	"github.com/HappyOnigiri/WX/internal/sessions/config"
	"github.com/HappyOnigiri/WX/internal/sessions/metacache"
)

// Options は走査が使う cache と計測先を表す。ゼロ値は cache を使わない従来どおりの走査である。
type Options struct {
	Cache *metacache.Cache
	Stats *Stats
}

// Stats は 1 回の走査の計測値で、cache の効きを検証するテストが読む。
type Stats struct {
	Enumerated int // 列挙した JSONL の数
	Parsed     int // readMetadata まで進んだ数
	Reused     int // cache の解析結果を再利用した数
}

func (s *Stats) enumerated() {
	if s != nil {
		s.Enumerated++
	}
}

func (s *Stats) parsed() {
	if s != nil {
		s.Parsed++
	}
}

func (s *Stats) reused() {
	if s != nil {
		s.Reused++
	}
}

// Scan は設定された Claude と Codex の JSONL からメタ情報を都度走査する。
func Scan(ctx context.Context, cfg config.Config, agents ...string) ([]Session, error) {
	return ScanWith(ctx, cfg, Options{}, agents...)
}

// ScanWith は cache を使って未変更 JSONL の再解析を省く走査を行う。
// ディレクトリ列挙とファイル属性の確認は毎回行うため、新規・追記・truncate・置換は呼び出しごとに反映される。
func ScanWith(ctx context.Context, cfg config.Config, opts Options, agents ...string) ([]Session, error) {
	sessions, err := collect(ctx, cfg, opts, agents, "")
	if err != nil {
		return nil, err
	}
	return deduplicate(sessions), nil
}

// Find は native ID が一致する会話だけを、全体の整列を経ずに探す。
// 重複 ID では走査済みのうち mtime が最も新しいものを返し、見つからなければ found=false を返す。
func Find(ctx context.Context, cfg config.Config, opts Options, agent, id string) (Session, bool, error) {
	if err := validateAgents([]string{agent}); err != nil {
		return Session{}, false, err
	}
	if id == "" {
		return Session{}, false, nil
	}
	found, err := collect(ctx, cfg, opts, []string{agent}, id)
	if err != nil {
		return Session{}, false, err
	}
	var latest Session
	ok := false
	for _, session := range found {
		if !ok || session.Mtime > latest.Mtime {
			latest, ok = session, true
		}
	}
	return latest, ok, nil
}

func validateAgents(agents []string) error {
	for _, agent := range agents {
		if agent != "claude" && agent != "codex" {
			return fmt.Errorf("unsupported agent: %s", agent)
		}
	}
	return nil
}

// collect は want が空なら全会話を、そうでなければ native ID が want の会話だけを集める。
func collect(ctx context.Context, cfg config.Config, opts Options, agents []string, want string) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(agents) == 0 {
		agents = []string{"claude", "codex"}
	}
	if err := validateAgents(agents); err != nil {
		return nil, err
	}
	var sessions []Session
	for _, agent := range agents {
		// 保存済み entry は走査の先頭で 1 回だけ読み、ファイルごとの問い合わせをなくす。
		state := &scanState{opts: opts, agent: agent, entries: opts.Cache.Snapshot(ctx, agent), volumes: map[string]string{}}
		for _, root := range cfg.SessionPaths(agent) {
			found, err := scanRoot(ctx, expandHome(root), state, want)
			state.flush(ctx)
			if err != nil {
				return nil, err
			}
			sessions = append(sessions, found...)
		}
	}
	return sessions, nil
}

func deduplicate(sessions []Session) []Session {
	seen := make(map[string]int, len(sessions))
	result := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		key := session.Tool + "\x00" + session.SessionID
		index, ok := seen[key]
		if !ok {
			seen[key] = len(result)
			result = append(result, session)
			continue
		}
		if session.Mtime > result[index].Mtime {
			result[index] = session
		}
	}
	return result
}

// scanState は 1 つの agent の走査で共有する cache 状態を保つ。
// volumes は directory ごとの volume 識別子を覚える。regular file は mount point になれないため、file の volume は親 directory と一致する。
type scanState struct {
	opts    Options
	agent   string
	entries map[string]metacache.Entry
	volumes map[string]string
	pending map[string]metacache.Entry
}

// reuse は属性が完全に一致する保存済み解析結果を返す。
func (s *scanState) reuse(path string, validator metacache.Validator) (metacache.Record, bool) {
	entry, ok := s.entries[path]
	if !ok || entry.Validator != validator {
		return metacache.Record{}, false
	}
	return entry.Record, true
}

func (s *scanState) store(path string, validator metacache.Validator, record metacache.Record) {
	if s.pending == nil {
		s.pending = map[string]metacache.Entry{}
	}
	if s.entries == nil {
		s.entries = map[string]metacache.Entry{}
	}
	s.pending[path] = metacache.Entry{Validator: validator, Record: record}
	s.entries[path] = s.pending[path]
}

// flush は 1 つの root で確定した解析結果をまとめて保存する。
func (s *scanState) flush(ctx context.Context) {
	if len(s.pending) == 0 {
		return
	}
	s.opts.Cache.Save(ctx, s.agent, s.pending)
	s.pending = nil
}

// volumeOf は directory の volume 識別子を覚えながら返す。
func (s *scanState) volumeOf(dir string) (string, error) {
	if volume, ok := s.volumes[dir]; ok {
		if volume == "" {
			return "", errors.New("volume identifier is unavailable")
		}
		return volume, nil
	}
	volume, err := readVolume(dir)
	s.volumes[dir] = volume
	if err != nil {
		return "", err
	}
	return volume, nil
}

func readVolume(dir string) (string, error) {
	file, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	identity, err := domain.FileIdentity(file)
	if err != nil {
		return "", err
	}
	_, volume, ok := domain.IdentityFields(identity)
	if !ok {
		return "", fmt.Errorf("unsupported directory identity %q", identity)
	}
	return volume, nil
}

func scanRoot(ctx context.Context, root string, state *scanState, want string) ([]Session, error) {
	tool := state.agent
	var sessions []Session
	seen := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			if tool == "claude" && entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		// symlink は lstat 由来の mode で弾き、以降の open が実体を辿らないようにする。
		if !info.Mode().IsRegular() {
			return nil
		}
		seen[path] = struct{}{}
		state.opts.Stats.enumerated()
		if !mayMatch(tool, path, want) {
			return nil
		}
		session, ok, err := resolveFile(ctx, path, info, state)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ok && (want == "" || session.SessionID == want) {
			sessions = append(sessions, session)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessions, nil
		}
		return nil, fmt.Errorf("scan %s: %w", root, err)
	}
	// 完走した走査だけが、この root で消えた entry を整理してよい。
	state.flush(ctx)
	state.opts.Cache.Prune(ctx, tool, root, seen)
	return sessions, nil
}

// mayMatch は解析前に候補外と確定できるかを判定する。
// Claude の native ID はファイル名で決まるが、Codex は payload の ID が優先されるためファイル名では確定できない。
// Codex の絞り込みは cache が ID を確定できたファイルにだけ効き、未分類・変更されたファイルは解析へ進む。
func mayMatch(tool, path, want string) bool {
	if want == "" || tool != "claude" {
		return true
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl") == want
}

// resolveFile は 1 つの JSONL からメタデータを得る。列挙時の属性が cache と一致すれば open も解析もしない。
// 解析したファイルは読み取り後の属性を取り直し、途中で変わっていたら cache へ確定しない。
func resolveFile(ctx context.Context, path string, info os.FileInfo, state *scanState) (Session, bool, error) {
	tool := state.agent
	mtimeNS := info.ModTime().UnixNano()
	// mtime と size は列挙時の stat から取る。cache ヒット時も同じ経路を通るため、両者の由来がずれない。
	size := info.Size()
	volume, volumeErr := state.volumeOf(filepath.Dir(path))
	var before metacache.Validator
	attrErr := volumeErr
	if attrErr == nil {
		before, attrErr = validatorFrom(info, volume)
	}
	if attrErr == nil {
		if record, hit := state.reuse(path, before); hit {
			state.opts.Stats.reused()
			session, ok := sessionFrom(tool, path, record, mtimeNS, size)
			return session, ok, nil
		}
	}
	state.opts.Stats.parsed()
	file, err := os.Open(path)
	if err != nil {
		return Session{}, false, err
	}
	defer func() { _ = file.Close() }()
	meta, err := readMetadata(ctx, file, path, tool)
	if err != nil {
		return Session{}, false, err
	}
	record := recordFrom(tool, path, meta)
	if attrErr == nil {
		if after, err := statValidator(file, volume); err == nil && after == before {
			state.store(path, before, record)
		}
	}
	session, ok := sessionFrom(tool, path, record, mtimeNS, size)
	return session, ok, nil
}

func statValidator(file *os.File, volume string) (metacache.Validator, error) {
	info, err := file.Stat()
	if err != nil {
		return metacache.Validator{}, err
	}
	return validatorFrom(info, volume)
}

// validatorFrom は cache の再利用可否に使う属性を、stat 結果と親 directory の volume から組み立てる。
func validatorFrom(info os.FileInfo, volume string) (metacache.Validator, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return metacache.Validator{}, fmt.Errorf("unsupported file attribute type %T", info.Sys())
	}
	return metacache.Validator{
		Identity:      domain.FormatIdentity(strconv.FormatUint(stat.Ino, 10), volume),
		Size:          info.Size(),
		MtimeNS:       info.ModTime().UnixNano(),
		CtimeNS:       changeTimeNanos(stat),
		ParserVersion: parserVersion,
	}, nil
}

// recordFrom は解析結果を、native ID を確定させた保存形へ直す。
func recordFrom(tool, path string, meta fileMeta) metacache.Record {
	nativeID := meta.nativeID
	if tool == "claude" {
		nativeID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	} else if nativeID == "" {
		nativeID = codexIDFromPath(path)
	}
	return metacache.Record{
		NativeID: nativeID,
		Title:    meta.title,
		CWD:      meta.cwd,
		Excluded: meta.subagent || nativeID == "" || meta.title == "",
	}
}

func sessionFrom(tool, path string, record metacache.Record, mtimeNS, size int64) (Session, bool) {
	if record.Excluded {
		return Session{}, false
	}
	return Session{
		Tool:      tool,
		SessionID: record.NativeID,
		Title:     record.Title,
		CWD:       record.CWD,
		StableID:  StableID(tool, record.NativeID),
		Mtime:     float64(mtimeNS) / 1e9,
		Size:      size,
		RawPath:   path,
	}, true
}
