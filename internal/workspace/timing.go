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
	err := run()
	p.Phases.Observe(name, start)
	return err
}
