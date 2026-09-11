package workspace

import (
	"sync"
	"time"
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
}

// runningPhase は開始済みで未終了の区間 1 件である。
type runningPhase struct {
	id    uint64
	name  string
	start time.Time
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
	t.running = append(t.running, runningPhase{id: t.nextRun, name: name, start: start})
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

// Active は実行中の区間のうち最後に始まったものの名前と開始時刻を返す。
// 入れ子では最も内側が最後に始まるため、いま実際に時間を使っている区間が返る。
// 実行中の区間が無ければ ok=false を返す。
func (t *PhaseTimings) Active() (name string, start time.Time, ok bool) {
	if t == nil {
		return "", time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.running) == 0 {
		return "", time.Time{}, false
	}
	last := t.running[len(t.running)-1]
	return last.name, last.start, true
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
