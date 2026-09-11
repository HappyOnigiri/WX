package workspace

import (
	"sync"
	"time"

	"github.com/HappyOnigiri/WX/internal/discovery"
)

// Phase は準備の1区間の呼び出し回数と合計所要時間である。
type Phase struct {
	Name  string        `json:"name"`
	Count int           `json:"count"`
	Total time.Duration `json:"-"`
}

// PhaseTimings は準備の区間ごとの所要時間を、最初に現れた順で集計する。
// 計測は準備結果を変えないため、nil のままでも同じ worktree ができ、記録だけが落ちる。
// 名前に "." を含む区間は並列 worker の合計であり、上位区間の実時間には収まらない。
type PhaseTimings struct {
	mu     sync.Mutex
	order  []string
	phases map[string]*Phase
	// running は開始済みで未終了の区間を開始順に持つ。
	// 完了後の集計だけでは進捗を読めないため、実行中の区間を別に追跡する。
	running []runningPhase
	// nextRun は running の項目を識別する連番。同名の区間が入れ子になっても終了先を取り違えない。
	nextRun uint64
	// scope は以降に始まる区間が属する対象。区間名は repository をまたいで繰り返すため、
	// 名前だけでは何周目かを読めない。集計側の名前は変えず、実行中の表示にだけ付ける。
	scope PhaseScope
}

// PhaseScope は区間が属する対象である。単一 repository の workspace では Total が 1 になり、
// 表示側は対象を省ける。Target が空なら workspace root 自身の作業を指す。
type PhaseScope struct {
	// Target は repository の表示名。workspace root からの相対 path を使う。
	Target string
	// Index は Total 件中の何件目かで、1 始まりである。
	Index int
	Total int
}

// runningPhase は開始済みで未終了の区間 1 件である。
type runningPhase struct {
	id    uint64
	name  string
	start time.Time
	scope PhaseScope
}

// ActivePhase は実行中の区間 1 件である。
type ActivePhase struct {
	Name  string
	Scope PhaseScope
	Start time.Time
}

// Scope は以降に begin する区間が属する対象を差し替える。
// 準備は 1 slot につき 1 goroutine が逐次に進むため、対象は区間の開始時点の値で確定する。
func (t *PhaseTimings) Scope(scope PhaseScope) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.scope = scope
}

// begin は name の区間を実行中として登録し、end へ渡す識別子を返す。
// nil レシーバでは 0 を返し、end も何もしない。
func (t *PhaseTimings) begin(name string, start time.Time) uint64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextRun++
	t.running = append(t.running, runningPhase{id: t.nextRun, name: name, start: start, scope: t.scope})
	return t.nextRun
}

// end は begin で登録した区間を実行中から外す。
func (t *PhaseTimings) end(id uint64) {
	if t == nil || id == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for index, running := range t.running {
		if running.id == id {
			t.running = append(t.running[:index], t.running[index+1:]...)
			return
		}
	}
}

// Active は実行中の区間のうち最後に始まったものを返す。
// 入れ子では最も内側が最後に始まるため、いま実際に時間を使っている区間が返る。
// 区間の切れ目でも ok=false になるので、呼び出し側はこれを「準備が動いていない」の根拠にしてはならない。
func (t *PhaseTimings) Active() (active ActivePhase, ok bool) {
	if t == nil {
		return ActivePhase{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.running) == 0 {
		return ActivePhase{}, false
	}
	last := t.running[len(t.running)-1]
	return ActivePhase{Name: last.name, Scope: last.scope, Start: last.start}, true
}

// Observe は start から現在までを name の区間へ1回分加える。
func (t *PhaseTimings) Observe(name string, start time.Time) {
	t.Add(name, 1, time.Since(start))
}

// Add は既に測り終えた区間を加える。並列 worker の合計を持ち込む経路が使う。
func (t *PhaseTimings) Add(name string, count int, total time.Duration) {
	if t == nil || count == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phases == nil {
		t.phases = map[string]*Phase{}
	}
	phase, seen := t.phases[name]
	if !seen {
		phase = &Phase{Name: name}
		t.phases[name] = phase
		t.order = append(t.order, name)
	}
	phase.Count += count
	phase.Total += total
}

// Phases は集計結果を最初に現れた順で返す。
func (t *PhaseTimings) Phases() []Phase {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Phase, 0, len(t.order))
	for _, name := range t.order {
		out = append(out, *t.phases[name])
	}
	return out
}

// RepositoryScope は repository 1 件分の区間が属する対象を組む。
// workspace root 直下が repository の場合は相対 path が "." になるため、対象名を持たせない。
func RepositoryScope(repo discovery.Repository, index, total int) PhaseScope {
	target := repo.RelativePath
	if target == "." {
		target = ""
	}
	return PhaseScope{Target: target, Index: index, Total: total}
}

// timePhase は run の所要時間を name の区間へ記録する。
// 失敗した回も記録するのは、どの区間で止まったかが遅さの調査に必要だからである。
func (p *Preparer) timePhase(name string, run func() error) error {
	start := time.Now()
	running := p.Phases.begin(name, start)
	err := run()
	p.Phases.end(running)
	p.Phases.Observe(name, start)
	return err
}
